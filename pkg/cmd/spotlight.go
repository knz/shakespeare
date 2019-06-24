package cmd

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

// spotlight stats the monitoring (spotlight) thread for a given actor.
// The collected events are sent to the given auChan.
func (ap *app) spotlight(
	ctx context.Context, a *actor, monLogger *log.SecondaryLogger, auChan chan<- auditableEvent,
) error {
	ps, err, exitErr := a.runActorCommandWithConsumer(ctx, ap.stopper, 0, true, a.role.spotlightCmd, func(line string) error {
		monLogger.Logf(ctx, "clamors: %q", line)
		sigCtx := logtags.AddTag(ctx, "signals", nil)
		line = strings.TrimSpace(line)
		ap.detectSignals(sigCtx, monLogger, a, auChan, line)
		return nil
	})
	log.Infof(ctx, "spotlight terminated (%+v)", err)
	if ps != nil && !ps.Success() {
		if errors.Is(err, context.Canceled) {
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

	for _, rp := range a.role.resParsers {
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
			varName: exprVar{actorName: a.name, sigName: rp.name},
		}

		var ts time.Time

		// Parse the timestamp.
		if rp.reGroup == "" {
			// If the reGroup is empty, that means we're OK with
			// the auto-generated "now" timestamp.
			ts = timeutil.Now()
		} else {
			var err error
			logTime := rp.re.ReplaceAllString(line, "${"+rp.reGroup+"}")
			ts, err = time.Parse(rp.timeLayout, logTime)
			if err != nil {
				monLogger.Logf(ctx, "signal %s: invalid log timestamp %q in %q: %+v", rp.name, logTime, line, err)
				continue
			}
		}

		elapsed := ts.Sub(ap.au.epoch).Seconds()

		ev, ok := evs[elapsed]
		if !ok {
			ev = &auditableEvent{ts: elapsed}
			evs[elapsed] = ev
			tss = append(tss, elapsed)
		}

		// Parse the data.
		switch rp.typ {
		case parseEvent:
			valHolder.val = rp.re.ReplaceAllString(line, "${event}")
		case parseScalar:
			valS := rp.re.ReplaceAllString(line, "${scalar}")
			x, err := strconv.ParseFloat(valS, 64)
			if err != nil {
				monLogger.Logf(ctx, "signal %s: invalid scalar %q in %q: %+v", rp.name, valS, line, err)
				continue
			}
			valHolder.val = x
		case parseDelta:
			curValS := rp.re.ReplaceAllString(line, "${delta}")
			curVal, err := strconv.ParseFloat(curValS, 64)
			if err != nil {
				monLogger.Logf(ctx, "signal %s: error parsing %q for delta: %+v", rp.name, curValS, err)
				continue
			}
			valHolder.val = curVal - sink.lastVal
			sink.lastVal = curVal
		}

		ap.witness(ctx, "(%s's %s:) %s", a.name, rp.name, valHolder.val)
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
