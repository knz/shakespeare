package main

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

	"github.com/cockroachdb/cockroach/pkg/util/log"
)

// spotlight stats the monitoring (spotlight) thread for a given actor.
// The collected events are sent to the given spotlightChan.
func (a *actor) spotlight(
	ctx context.Context, monLogger *log.SecondaryLogger, spotlightChan chan<- dataEvent,
) error {
	// Start the spotlight command in the background.
	cmd := a.makeShCmd(a.role.spotlightCmd)
	log.Infof(ctx, "executing: %s", strings.Join(cmd.Args, " "))
	outstream, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("setting up: %+v", err)
	}
	cmd.Stdout = cmd.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec error: %+v", err)
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
	go func() {
		// The reader runs asynchronously, until there is no more data to
		// read or the context is canceled.
		for {
			line, err := rd.ReadString('\n')
			line = strings.TrimSpace(line)
			if line != "" {
				select {
				case <-ctx.Done():
					// Context canceled. don't even try going further.
					return
				case lines <- res{line, nil}:
					// ok
				}
			}
			if err != nil {
				if err != io.EOF {
					select {
					case <-ctx.Done():
						// Context canceled. don't even try going further.
						return
					case lines <- res{"", err}:
						// ok
					}
				}
				close(lines)
				return
			}
		}
	}()

	// Process the lines received from the actor.
	for {
		select {
		case res := <-lines:
			if res.err != nil {
				return res.err
			}
			if res.line == "" {
				return nil
			}
			monLogger.Logf(ctx, "clamors: %q", res.line)
			a.detectSignals(ctx, spotlightChan, res.line)

		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

// detectSignals parses a line produced by the spotlight to detect any
// signal is contains. Detected signals are sent to the spotlightChan.
func (a *actor) detectSignals(ctx context.Context, spotlightChan chan<- dataEvent, line string) {
	for _, rp := range a.role.resParsers {
		sink, ok := a.audiences[rp.name]
		if !ok || len(sink.audiences) == 0 {
			// No audience for this signal: don't even bother collecting the data.
			continue
		}
		if !rp.re.MatchString(line) {
			continue
		}
		ev := dataEvent{
			typ:       rp.typ,
			audiences: sink.audiences,
			actorName: a.name,
			sigName:   rp.name,
		}

		// Parse the timestamp.
		if rp.reGroup == "" {
			// If the reGroup is empty, that means we're OK with
			// the auto-generated "now" timestamp.
			ev.ts = time.Now().UTC()
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

		select {
		case <-ctx.Done():
			return
		case spotlightChan <- ev:
			// ok
		}
	}
}
