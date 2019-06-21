package cmd

import (
	"bufio"
	"context"
	"fmt"
	"html"
	"io"
	"os"
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
	// Start the spotlight command in the background.
	cmd := a.makeShCmd(a.role.spotlightCmd)
	log.Infof(ctx, "executing: %s", strings.Join(cmd.Args, " "))
	outstream, err := cmd.StderrPipe()
	if err != nil {
		return errors.WithContextTags(errors.Wrap(err, "setting up"), ctx)
	}
	cmd.Stdout = cmd.Stderr
	if err := cmd.Start(); err != nil {
		return errors.WithContextTags(errors.Wrap(err, "exec"), ctx)
	}

	defer func() {
		// When the function returns, we'll close the stream but also try
		// to terminate the command gracefully. If that fails, we'll be a
		// bit more agressive.
		outstream.Close()
		if cmd.Process != nil {
			cmd.Process.Signal(os.Interrupt)
			time.Sleep(1)
			cmd.Process.Kill()
		}
		err := cmd.Wait()
		log.Infof(ctx, "spotlight terminated: %+v", err)
	}()

	// We'll use a buffered reader to extract lines of data from it.
	rd := bufio.NewReader(outstream)

	type res struct {
		line string
		err  error
	}
	lines := make(chan res)

	readCtx := logtags.AddTag(ctx, "reader", nil)
	runWorker(readCtx, ap.stopper, func(ctx context.Context) {
		defer func() { close(lines) }()
		// The reader runs asynchronously, until there is no more data to
		// read or the context is canceled.
		for {
			line, err := rd.ReadString('\n')
			line = strings.TrimSpace(line)
			if line != "" {
				select {
				case <-ap.stopper.ShouldStop():
					log.Info(ctx, "interrupted")
					return
				case <-ctx.Done():
					log.Info(ctx, "canceled")
					return
				case lines <- res{line, nil}:
					// ok
				}
			}
			if err != nil {
				if err != io.EOF {
					select {
					case <-ap.stopper.ShouldStop():
						log.Info(ctx, "interrupted")
					case <-ctx.Done():
						log.Info(ctx, "canceled")
					case lines <- res{"", errors.WithContextTags(errors.WithStack(err), ctx)}:
					}
				} else {
					log.Info(ctx, "EOF")
				}
				return
			}
		}
	})

	// Process the lines received from the actor.
	for {
		select {
		case res := <-lines:
			if res.err != nil {
				return errors.WithContextTags(errors.WithStack(res.err), ctx)
			}
			if res.line == "" {
				return nil
			}
			monLogger.Logf(ctx, "clamors: %q", res.line)
			sigCtx := logtags.AddTag(ctx, "signals", nil)
			ap.detectSignals(sigCtx, a, collectorChan, auChan, res.line)

		case <-ap.stopper.ShouldStop():
			log.Info(ctx, "interrupted")
			return nil

		case <-ctx.Done():
			log.Info(ctx, "canceled")
			return errors.WithContextTags(errors.WithStack(ctx.Err()), ctx)
		}
	}

	return nil
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
		case <-ap.stopper.ShouldStop():
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
		case <-ap.stopper.ShouldStop():
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
