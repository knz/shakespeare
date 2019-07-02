package cmd

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

func (ap *app) manageSpotlights(
	ctx context.Context,
	monLogger *log.SecondaryLogger,
	auChan chan<- auditableEvent,
	termCh <-chan struct{},
) (err error) {
	// errCh collects the errors from the concurrent spotlights.
	errCh := make(chan error, len(ap.cfg.actors)+1)

	// This context is to cancel other spotlights when one of them fails.
	stopCtx, stopOnError := context.WithCancel(ctx)
	defer stopOnError()

	defer func() {
		if r := recover(); r != nil {
			panic(r)
		}
		// Collect all the errors.
		err = collectErrors(ctx, nil, errCh, "spotlight-supervisor")
	}()

	// We'll cancel any remaining spotlights, and wait, at the end.
	var wg sync.WaitGroup
	defer func() { wg.Wait() }()

	numSpotlights := 0
	for actName, thisActor := range ap.cfg.actors {
		if thisActor.role.spotlightCmd == "" {
			// No spotlight defined, don't start anything.
			continue
		}
		spotCtx := logtags.AddTag(stopCtx, "spotlight", nil)
		spotCtx = logtags.AddTag(spotCtx, "actor", actName)
		spotCtx = logtags.AddTag(spotCtx, "role", thisActor.role.name)
		a := thisActor
		wg.Add(1)
		runWorker(spotCtx, ap.stopper, func(ctx context.Context) {
			defer wg.Done()
			log.Info(spotCtx, "<shining>")
			err := errors.WithContextTags(ap.spotlight(ctx, a, monLogger, auChan, termCh), ctx)
			if errors.Is(err, context.Canceled) {
				// It's ok if a sportlight is canceled.
				err = nil
			}
			errCh <- err
			log.Info(ctx, "<off>")
		})
		numSpotlights++
	}
	if numSpotlights == 0 {
		// There are no spotlights defined! However we can't just
		// let the spotlight worker terminate immediately, because
		// this would cause the conductor to abort the play too early.
		// Instead, we use a "do nothing" collector that simply forwards
		// the prompt termination to the auditors.
		errCh <- ap.pseudoSpotlight(ctx, auChan, termCh)
	}

	return nil
}

func (ap *app) pseudoSpotlight(
	ctx context.Context, auCh chan<- auditableEvent, termCh <-chan struct{},
) error {
	ctx = logtags.AddTag(ctx, "pseudo-spotlight", nil)
	select {
	case <-termCh:
		return nil
	case <-ctx.Done():
		log.Info(ctx, "interrupted")
		return wrapCtxErr(ctx)
	case <-ap.stopper.ShouldQuiesce():
		log.Info(ctx, "terminated")
		return nil
	}
}

// spotlight stats the monitoring (spotlight) thread for a given actor.
// The collected events are sent to the given auChan.
func (ap *app) spotlight(
	ctx context.Context,
	a *actor,
	monLogger *log.SecondaryLogger,
	auChan chan<- auditableEvent,
	termCh <-chan struct{},
) error {
	ps, err, exitErr := a.runActorCommandWithConsumer(ctx, ap.stopper, 0, true, a.role.spotlightCmd, termCh, func(line string) error {
		monLogger.Logf(ctx, "clamors: %q", line)
		sigCtx := logtags.AddTag(ctx, "signals", nil)
		line = strings.TrimSpace(line)
		ap.detectSignals(sigCtx, monLogger, a, auChan, line)
		return nil
	})
	log.Infof(ctx, "spotlight terminated (%+v)", err)
	if ps != nil && !ps.Success() {
		st := ps.Sys().(syscall.WaitStatus)
		if st.Signaled() && st.Signal() == syscall.SIGHUP {
			// It's expected that the spotlight gets a hangup at the end of execution. It's not worth reporting.
		} else {
			ap.narrate(E, "😞", "%s's spotlight failed: %s (see log for details)", a.name, exitErr)
			err = combineErrors(err, errors.WithContextTags(errors.Wrap(exitErr, "spotlight terminated abnormally"), ctx))
		}
	}

	return err
}

// detectSignals parses a line produced by the spotlight to detect any
// signal is contains. Detected signals are sent to the auChan.
func (ap *app) detectSignals(
	ctx context.Context,
	monLogger *log.SecondaryLogger,
	a *actor,
	auChan chan<- auditableEvent,
	line string,
) {
	evs := map[float64]*auditableEvent{}
	var tss []float64

	for _, rp := range a.role.sigParsers {
		sink, ok := a.sinks[rp.name]
		if !ok {
			// No audience for this signal: don't even bother collecting the data.
			continue
		}
		if !rp.re.MatchString(line) {
			continue
		}
		valHolder := auditableValue{
			typ:     rp.typ,
			varName: varName{actorName: a.name, sigName: rp.name},
		}

		var ts time.Time

		// Parse the timestamp.
		switch rp.reGroup {
		case "":
			// If the reGroup is empty, that means we're OK with
			// the auto-generated "now" timestamp.
			ts = timeutil.Now()
		case "ts_deltasecs":
			logTime := rp.re.ReplaceAllString(line, "${ts_deltasecs}")
			delta, err := strconv.ParseFloat(logTime, 64)
			if err != nil {
				monLogger.Logf(ctx, "signal %s: invalid second delta %q: %+v", rp.name, logTime, err)
				continue
			}
			ts = ap.theater.epoch.Add(time.Duration(delta * float64(time.Second)))
		default:
			var err error
			logTime := rp.re.ReplaceAllString(line, "${"+rp.reGroup+"}")
			ts, err = time.Parse(rp.timeLayout, logTime)
			if err != nil {
				monLogger.Logf(ctx, "signal %s: invalid log timestamp %q in %q: %+v", rp.name, logTime, line, err)
				continue
			}
		}

		elapsed := ts.Sub(ap.theater.epoch).Seconds()

		ev, ok := evs[elapsed]
		if !ok {
			ev = &auditableEvent{ts: elapsed}
			evs[elapsed] = ev
			tss = append(tss, elapsed)
		}

		// Parse the data.
		switch rp.typ {
		case sigTypEvent:
			valHolder.val = rp.re.ReplaceAllString(line, "${event}")
		case sigTypScalar:
			valS := rp.re.ReplaceAllString(line, "${scalar}")
			x, err := strconv.ParseFloat(valS, 64)
			if err != nil {
				monLogger.Logf(ctx, "signal %s: invalid scalar %q in %q: %+v", rp.name, valS, line, err)
				continue
			}
			valHolder.val = x
		case sigTypDelta:
			curValS := rp.re.ReplaceAllString(line, "${delta}")
			curVal, err := strconv.ParseFloat(curValS, 64)
			if err != nil {
				monLogger.Logf(ctx, "signal %s: error parsing %q for delta: %+v", rp.name, curValS, err)
				continue
			}
			valHolder.val = curVal - sink.lastVal
			sink.lastVal = curVal
		}

		ap.witness(ctx, "(%s's %s:) %v", a.name, rp.name, valHolder.val)
		ev.values = append(ev.values, valHolder)
	}

	sort.Float64s(tss)

	for _, ts := range tss {
		ev := *evs[ts]
		monLogger.Logf(ctx, "at %.2fs, found event: %+v", ts, ev)
		// Emit events to the audition.
		select {
		case <-ap.stopper.ShouldQuiesce():
			log.Info(ctx, "interrupted")
			return
		case <-ctx.Done():
			log.Info(ctx, "canceled")
			return
		case auChan <- ev:
			// ok
		}
	}
}
