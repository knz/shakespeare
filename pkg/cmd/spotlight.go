package cmd

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/stop"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

type spotMgr struct {
	r       reporter
	cfg     *config
	stopper *stop.Stopper

	// Where to log detected signals.
	logger *log.SecondaryLogger

	// The spotlight manager listens on this channel and terminates
	// gracefully when it is closed.
	termCh <-chan struct{}

	// Where to send observed signals to.
	// The spotlight manager also sends terminate when it finishes
	// turning the spotlights off.
	auditCh chan<- auditableEvent

	// Where the spotlight manager should return errors.
	// It also closes this when it terminates.
	errCh chan<- error
}

func (spm *spotMgr) manageSpotlights(ctx context.Context) (err error) {
	// errCh collects the errors from the concurrent spotlights.
	errCh := make(chan error, len(spm.cfg.actors)+1)

	// This context is to cancel other spotlights when one of them fails.
	stopCtx, stopOnError := context.WithCancel(ctx)
	defer stopOnError()

	defer func() {
		if r := recover(); r != nil {
			panic(r)
		}
		// Collect all the errors.
		err = collectErrors(ctx, nil, errCh, "spotlight-supervisor")

		err = combineErrors(spm.signalAuditTermination(ctx), err)
	}()

	// We'll cancel any remaining spotlights, and wait, at the end.
	var wg sync.WaitGroup
	defer func() { wg.Wait() }()

	numSpotlights := 0
	for actName, thisActor := range spm.cfg.actors {
		if thisActor.role.spotlightCmd == "" {
			// No spotlight defined, don't start anything.
			continue
		}
		spotCtx := logtags.AddTag(stopCtx, "spotlight", nil)
		spotCtx = logtags.AddTag(spotCtx, "actor", actName)
		spotCtx = logtags.AddTag(spotCtx, "role", thisActor.role.name)
		a := thisActor
		wg.Add(1)
		runWorker(spotCtx, spm.stopper, func(ctx context.Context) {
			defer wg.Done()
			log.Info(spotCtx, "<shining>")
			err := errors.WithContextTags(spm.spotlight(ctx, a), ctx)
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
		errCh <- spm.pseudoSpotlight(ctx)
	}

	return nil
}

func (spm *spotMgr) signalAuditTermination(ctx context.Context) error {
	select {
	case spm.auditCh <- terminate{}:
		return nil
	case <-ctx.Done():
		log.Info(ctx, "interrupted")
		return wrapCtxErr(ctx)
	case <-spm.stopper.ShouldQuiesce():
		log.Info(ctx, "terminated")
		return nil
	}
}

func (spm *spotMgr) pseudoSpotlight(ctx context.Context) error {
	ctx = logtags.AddTag(ctx, "pseudo-spotlight", nil)
	select {
	case <-spm.termCh:
		return nil
	case <-ctx.Done():
		log.Info(ctx, "interrupted")
		return wrapCtxErr(ctx)
	case <-spm.stopper.ShouldQuiesce():
		log.Info(ctx, "terminated")
		return nil
	}
}

// spotlight stats the monitoring (spotlight) thread for a given actor.
// The collected events are sent to the given auChan.
func (spm *spotMgr) spotlight(ctx context.Context, a *actor) error {
	ps, err, exitErr := a.runActorCommandWithConsumer(ctx, spm.stopper, 0, true, a.spotlightScript, spm.termCh, func(line string) error {
		spm.logger.Logf(ctx, "clamors: %q", line)
		sigCtx := logtags.AddTag(ctx, "signals", nil)
		line = strings.TrimSpace(line)
		spm.detectSignals(sigCtx, a, line)
		return nil
	})
	log.Infof(ctx, "spotlight terminated (%+v)", err)

	// If we are terminating, we're going to ignore the termination status below.
	terminating := false
	select {
	case <-spm.termCh:
		terminating = true
	default:
	}

	if !terminating && ps != nil && !ps.Success() {
		spm.r.narrate(E, "😞", "%s's spotlight failed: %s (see log for details)", a.name, exitErr)
		err = combineErrors(err, errors.WithContextTags(errors.Wrap(exitErr, "spotlight terminated abnormally"), ctx))
	}

	return err
}

// detectSignals parses a line produced by the spotlight to detect any
// signal is contains. Detected signals are sent to the auChan.
func (spm *spotMgr) detectSignals(ctx context.Context, a *actor, line string) {
	evs := map[float64]*sigEvent{}
	var tss []float64

	epoch := spm.r.epoch()

	tsNow := timeutil.Now()

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
			ts = tsNow
		case "ts_deltasecs":
			logTime := rp.re.ReplaceAllString(line, "${ts_deltasecs}")
			delta, err := strconv.ParseFloat(logTime, 64)
			if err != nil {
				spm.logger.Logf(ctx, "signal %s: invalid second delta %q: %+v", rp.name, logTime, err)
				continue
			}
			ts = epoch.Add(time.Duration(delta * float64(time.Second)))
		default:
			var err error
			logTime := rp.re.ReplaceAllString(line, "${"+rp.reGroup+"}")
			ts, err = time.Parse(rp.timeLayout, logTime)
			if err != nil {
				spm.logger.Logf(ctx, "signal %s: invalid log timestamp %q in %q: %+v", rp.name, logTime, line, err)
				continue
			}
		}

		elapsed := ts.Sub(epoch).Seconds()

		ev, ok := evs[elapsed]
		if !ok {
			ev = &sigEvent{ts: elapsed}
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
				spm.logger.Logf(ctx, "signal %s: invalid scalar %q in %q: %+v", rp.name, valS, line, err)
				continue
			}
			valHolder.val = x
		case sigTypDelta:
			curValS := rp.re.ReplaceAllString(line, "${delta}")
			curVal, err := strconv.ParseFloat(curValS, 64)
			if err != nil {
				spm.logger.Logf(ctx, "signal %s: error parsing %q for delta: %+v", rp.name, curValS, err)
				continue
			}
			valHolder.val = curVal - sink.lastVal
			sink.lastVal = curVal
		}

		spm.r.witness(ctx, "(%s's %s:) %v", a.name, rp.name, valHolder.val)
		ev.values = append(ev.values, valHolder)
	}

	sort.Float64s(tss)

	for _, ts := range tss {
		ev := evs[ts]
		spm.logger.Logf(ctx, "at %.2fs, found event: %+v", ts, ev)
		// Emit events to the audition.
		select {
		case <-spm.stopper.ShouldQuiesce():
			log.Info(ctx, "interrupted")
			return
		case <-ctx.Done():
			log.Info(ctx, "canceled")
			return
		case spm.auditCh <- ev:
			// ok
		}
	}
}
