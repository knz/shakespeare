package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/log/logtags"
)

// direct runs the play.
func direct(ctx context.Context) bool {
	// We'll log all the monitored and extracted data to a secondary logger.
	monLogger := log.NewSecondaryLogger(ctx, nil, "monitor", true /*enableGc*/, false /*forceSyncWrite*/)
	dataLogger := log.NewSecondaryLogger(ctx, nil, "collector", true /*enableGc*/, false /*forceSyncWrite*/)
	defer func() { log.Flush() }()

	type status struct {
		who string
		err error
	}
	errCh := make(chan status, len(actors))

	// Run the pre-flight routines.
	var wg sync.WaitGroup
	for actName := range actors {
		// Start one actor.
		actCtx := logtags.AddTag(ctx, "init", nil)
		actCtx = logtags.AddTag(actCtx, "actor", actName)
		actCtx = logtags.AddTag(actCtx, "role", actors[actName].role.name)
		log.Info(actCtx, "<prepare>")
		wg.Add(1)
		go func(ctx context.Context, a *actor) {
			if err := a.preflight(ctx); err != nil {
				errCh <- status{who: fmt.Sprintf("%s [%s]", a.role.name, a.name), err: err}
			}
			log.Info(ctx, "<ready>")
			wg.Done()
		}(actCtx, actors[actName])
	}
	wg.Wait()

	close(errCh)
	hasErr := false
	for st := range errCh {
		if st.err != context.Canceled {
			log.Errorf(ctx, "complaint from %s: %+v", st.who, st.err)
			hasErr = true
		}
	}
	if hasErr {
		return true
	}

	actChans := make(map[string]chan string)
	collector := make(chan dataEvent, len(actors))
	errCh = make(chan status, len(actors)+2)

	// Start the collector.
	var wgcol sync.WaitGroup
	wgcol.Add(1)
	colCtx, colDone := context.WithCancel(ctx)
	go func() {
		colCtx = logtags.AddTag(colCtx, "collector", nil)
		log.Info(colCtx, "<intrat>")
		if err := collect(colCtx, dataLogger, collector); err != nil {
			errCh <- status{who: "collector", err: err}
		}
		log.Info(colCtx, "<exit>")
		wgcol.Done()
	}()

	for actName := range actors {
		// Start one actor.
		actCtx := logtags.AddTag(ctx, "actor", actName)
		actCtx = logtags.AddTag(actCtx, "role", actors[actName].role.name)
		log.Info(actCtx, "<intrat>")
		actChan := make(chan string)
		actChans[actName] = actChan
		wg.Add(1)
		go func(ctx context.Context, a *actor) {
			if err := a.run(ctx, &wg, monLogger, collector, actChan); err != nil {
				errCh <- status{who: fmt.Sprintf("%s [%s]", a.role.name, a.name), err: err}
			}
			log.Info(ctx, "<exit>")
			wg.Done()
		}(actCtx, actors[actName])
	}

	// Start the dispatcher.
	wg.Add(1)
	go func() {
		dirCtx := logtags.AddTag(ctx, "dispatch", nil)
		log.Info(dirCtx, "<intrat>")
		if err := dispatch(dirCtx, actChans); err != nil {
			errCh <- status{who: "dispatch", err: err}
		}
		log.Info(dirCtx, "<exit>")
		colDone()
		wg.Done()
	}()

	// Wait for the dispatcher and actors to complete.
	wg.Wait()

	// Stop the collector and wait for it to complete.
	colDone()
	wgcol.Wait()

	close(errCh)
	hasErr = false
	for st := range errCh {
		if st.err != context.Canceled {
			log.Errorf(ctx, "complaint from %s: %+v", st.who, st.err)
			hasErr = true
		}
	}

	return hasErr
}

var curAmbiance = "clear"
var curAmbianceStart float64 = math.Inf(-1)

type ambiancePeriod struct {
	startTime float64
	endTime   float64
	ambiance  string
}

var ambiances []ambiancePeriod

func dispatch(ctx context.Context, actChans map[string]chan string) error {
	startTime := time.Now().UTC()
	defer func() {
		// Close the mood chapter, if one was open.
		if curAmbiance != "clear" {
			ambiances = append(ambiances, ambiancePeriod{
				startTime: curAmbianceStart,
				endTime:   math.Inf(1),
				ambiance:  curAmbiance,
			})
		}

		// Tell all actors to exit.
		for _, ch := range actChans {
			select {
			case <-ctx.Done():
			case ch <- "":
			}
		}
	}()

	for i, step := range steps {
		stepCtx := logtags.AddTag(ctx, "step", i)

		switch step.typ {
		case stepWaitUntil:
			now := time.Now().UTC()
			elapsed := now.Sub(startTime)
			toWait := step.dur - elapsed
			if toWait > 0 {
				log.Infof(stepCtx, "waiting for %.2fs", toWait.Seconds())
				tm := time.After(toWait)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-tm:
					// Wait.
				}
			} else {
				if toWait < 0 {
					log.Infof(stepCtx, "running behind schedule: %s", toWait)
				}
			}

		case stepAmbiance:
			if step.action == curAmbiance {
				continue
			}
			log.Infof(stepCtx, "the mood changes to %s", step.action)
			now := time.Now().UTC()
			elapsed := now.Sub(startTime)
			if curAmbiance != "clear" {
				ambiances = append(ambiances, ambiancePeriod{
					startTime: curAmbianceStart,
					endTime:   elapsed.Seconds(),
					ambiance:  curAmbiance,
				})
			}
			curAmbianceStart = elapsed.Seconds()
			curAmbiance = step.action

		case stepDo:
			log.Infof(stepCtx, "%s: %s!", step.character, step.action)
			ch := actChans[step.character]
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- step.action:
				// Ok done.
			}
		}
	}

	return nil
}

var shellPath = os.Getenv("SHELL")

func (a *actor) preflight(bctx context.Context) error {
	if cCmd := a.role.cleanupCmd; cCmd != "" {
		// If there is a cleanup command, run it before the preflight, to
		// ensure a pristime environment.
		a.cleanup(bctx)
	}

	pCmd := a.role.preflightCmd
	if pCmd == "" {
		return nil
	}
	ctx, cancel := context.WithDeadline(bctx, time.Now().Add(10*time.Second))
	defer cancel()
	log.Infof(ctx, "preflight: %s", pCmd)
	cmd := exec.Cmd{
		Path: shellPath,
		Args: []string{shellPath, "-c", string(pCmd)},
	}
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
	}()
	outdata, err := cmd.CombinedOutput()
	log.Infof(ctx, "preflight done\n%s\n-- %s", string(outdata), cmd.ProcessState.String())
	return err
}

func (a *actor) cleanup(ctx context.Context) {
	cCmd := a.role.cleanupCmd
	log.Infof(ctx, "cleanup: %s", cCmd)
	cmd := exec.Cmd{
		Path: shellPath,
		Args: []string{shellPath, "-c", string(cCmd)},
	}
	outdata, err := cmd.CombinedOutput()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		log.Errorf(ctx, "exec error: %+v", err)
	} else {
		log.Infof(ctx, "cleanup done\n%s\n-- %s", string(outdata), cmd.ProcessState.String())
	}
}

func (a *actor) run(
	ctx context.Context,
	wg *sync.WaitGroup,
	monLogger *log.SecondaryLogger,
	collector chan<- dataEvent,
	events <-chan string,
) error {
	if a.role.monitorCmd != "" {
		monCtx, monCancel := context.WithCancel(ctx)
		monCtx = logtags.AddTag(monCtx, "monitor", nil)

		log.Info(monCtx, "<intrat>")
		wg.Add(1)
		go func() {
			a.monitor(monCtx, monLogger, collector)
			log.Info(monCtx, "<exit>")
			wg.Done()
		}()
		defer func() {
			monCancel()
		}()
	}

	if cCmd := a.role.cleanupCmd; cCmd != "" {
		// If there is a cleanup command, run it at the end.
		defer func() {
			a.cleanup(ctx)
		}()
	}

	for {
		select {
		case <-ctx.Done():
			log.Infof(ctx, "stopped: %+v", ctx.Err())
			return ctx.Err()

		case ev := <-events:
			if ev == "" {
				// Special command to exit.
				log.Info(ctx, "bye!")
				return nil
			}

			aCmd, ok := a.role.actionCmds[ev]
			if !ok {
				log.Errorf(ctx, "unknown action: %q", ev)
				continue
			}
			log.Infof(ctx, "executing %q: %s", ev, aCmd)
			cmd := exec.Cmd{
				Path: shellPath,
				Args: []string{shellPath, "-c", string(aCmd)},
			}
			outdata, err := cmd.CombinedOutput()
			if _, ok := err.(*exec.ExitError); err != nil && !ok {
				log.Errorf(ctx, "exec error: %+v", err)
				continue
			}
			log.Infof(ctx, "%q done\n%s\n-- %s", ev, string(outdata), cmd.ProcessState.String())
		}
	}

	return nil
}

func (a *actor) monitor(
	ctx context.Context, monLogger *log.SecondaryLogger, collector chan<- dataEvent,
) error {
	monCmd := a.role.monitorCmd
	log.Infof(ctx, "executing: %s", monCmd)
	cmd := exec.Cmd{
		Path: shellPath,
		Args: []string{shellPath, "-c", string(monCmd)},
	}
	outstream, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("setting up: %+v", err)
	}
	cmd.Stdout = cmd.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec error: %+v", err)
	}
	rd := bufio.NewReader(outstream)

	type res struct {
		line string
		err  error
	}
	lines := make(chan res)
	go func() {
		for {
			line, err := rd.ReadString('\n')
			line = strings.TrimSpace(line)
			if line != "" {
				lines <- res{line, nil}
			}
			if err != nil {
				if err != io.EOF {
					lines <- res{"", err}
				}
				close(lines)
				return
			}
		}
	}()

	defer func() {
		// outstream.Close()
		cmd.Process.Kill()
		err := cmd.Wait()
		log.Infof(ctx, "monitor terminated: %+v", err)
	}()

	for {
		select {
		case res := <-lines:
			if res.err != nil {
				return res.err
			}
			if res.line == "" {
				return nil
			}
			monLogger.Logf(ctx, "mon data: %q", res.line)
			a.analyzeLine(ctx, collector, res.line)

		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

const logTimeLayout = "060102 15:04:05.999999"

func (a *actor) analyzeLine(ctx context.Context, collector chan<- dataEvent, line string) {
	for _, rp := range a.role.resParsers {
		if !rp.re.MatchString(line) {
			continue
		}
		ev := dataEvent{ts: time.Now().UTC(), typ: rp.typ, actorName: a.name, eventName: rp.name}

		var err error
		switch rp.tsTyp {
		case timeStyleLog:
			logTime := rp.re.ReplaceAllString(line, "${logtime}")
			ev.ts, err = time.Parse(logTimeLayout, logTime)
			if err != nil {
				log.Warningf(ctx, "invalid log timestamp %q in %q: %+v", logTime, line, err)
				continue
			}
		case timeStyleAbs:
			absTime := rp.re.ReplaceAllString(line, "${abstime}")
			ev.ts, err = time.Parse(time.RFC3339Nano, absTime)
			if err != nil {
				log.Warningf(ctx, "invalid log timestamp %q in %q: %+v", absTime, line, err)
				continue
			}
		}

		switch rp.typ {
		case parseEvent:
			ev.val = rp.re.ReplaceAllString(line, "${event}")
		case parseScalar:
			ev.val = rp.re.ReplaceAllString(line, "${scalar}")
		case parseDelta:
			ev.val = rp.re.ReplaceAllString(line, "${delta}")
		}

		select {
		case <-ctx.Done():
			return
		case collector <- ev:
			// ok
		}
	}
}
