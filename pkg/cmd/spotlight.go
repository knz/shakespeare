package cmd

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

// spotlight stats the monitoring (spotlight) thread for a given actor.
// The collected events are sent to the given collectorChan.
func (ap *app) spotlight(
	ctx context.Context,
	a *actor,
	monLogger *log.SecondaryLogger,
	collectorChan chan<- observation,
	auChan chan<- auditableEvent,
) error {
	ps, err, exitErr := a.runActorCommandWithConsumer(ctx, ap.stopper, 0, true, a.role.spotlightCmd, func(line string) error {
		monLogger.Logf(ctx, "clamors: %q", line)
		sigCtx := logtags.AddTag(ctx, "signals", nil)
		line = strings.TrimSpace(line)
		ap.detectSignals(sigCtx, a, collectorChan, auChan, line)
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
// signal is contains. Detected signals are sent to the collectorChan.
func (ap *app) detectSignals(
	ctx context.Context,
	a *actor,
	collectorChan chan<- observation,
	auChan chan<- auditableEvent,
	line string,
) {
	for _, rp := range a.role.resParsers {
		sink, ok := a.sinks[rp.name]
		if !ok || (len(sink.observers) == 0 && len(sink.auditors) == 0) {
			// No audience for this signal: don't even bother collecting the data.
			continue
		}
		if !rp.re.MatchString(line) {
			continue
		}
		ev := observation{
			typ:       rp.typ,
			observers: sink.observers,
			auditors:  sink.auditors,
			actorName: a.name,
			sigName:   rp.name,
		}

		// Parse the timestamp.
		if rp.reGroup == "" {
			// If the reGroup is empty, that means we're OK with
			// the auto-generated "now" timestamp.
			ev.ts = timeutil.Now()
		} else {
			var err error
			logTime := rp.re.ReplaceAllString(line, "${"+rp.reGroup+"}")
			ev.ts, err = time.Parse(rp.timeLayout, logTime)
			if err != nil {
				log.Warningf(ctx, "invalid log timestamp %q in %q: %+v", logTime, line, err)
				continue
			}
		}

		// Parse the data.
		switch rp.typ {
		case parseEvent:
			evText := rp.re.ReplaceAllString(line, "${event}")
			evText = html.EscapeString(evText)
			ev.val = fmt.Sprintf("%q", evText)
		case parseScalar:
			ev.val = rp.re.ReplaceAllString(line, "${scalar}")
		case parseDelta:
			curValS := rp.re.ReplaceAllString(line, "${delta}")
			curVal, err := strconv.ParseFloat(curValS, 64)
			if err != nil {
				log.Warningf(ctx,
					"signal %s: error parsing %q for delta: %+v",
					ev.sigName, curValS, err)
				continue
			}
			ev.val = fmt.Sprintf("%f", curVal-sink.lastVal)
			sink.lastVal = curVal
		}

		ap.witness(ctx, "(%s's %s:) %s", ev.actorName, ev.sigName, ev.val)

		// Emit to the observers.
		select {
		case <-ap.stopper.ShouldQuiesce():
			log.Info(ctx, "interrupted")
			return
		case <-ctx.Done():
			log.Info(ctx, "canceled")
			return
		case collectorChan <- ev:
			// ok
		}
		// Emit to the audition.
		select {
		case <-ap.stopper.ShouldQuiesce():
			log.Info(ctx, "interrupted")
			return
		case <-ctx.Done():
			log.Info(ctx, "canceled")
			return
		case auChan <- auditableEvent{ev}:
			// ok
		}
	}
}
