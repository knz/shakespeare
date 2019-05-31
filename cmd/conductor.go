package main

import (
	"context"
	"fmt"
	"html"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/log/logtags"
)

// conduct runs the play.
func conduct(ctx context.Context) bool {
	// Prepare all the working directories.
	for _, a := range actors {
		if err := os.MkdirAll(a.workDir, os.ModePerm); err != nil {
			log.Errorf(ctx, "mkdir %s: %+v", a.workDir, err)
			return true
		}
	}

	// We'll log all the monitored and extracted data to a secondary logger.
	monLogger := log.NewSecondaryLogger(ctx, nil, "spotlight", true /*enableGc*/, false /*forceSyncWrite*/)
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
			if err := a.prepare(ctx); err != nil {
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

	actorChans := make(map[string]chan string)
	spotlightChan := make(chan dataEvent, len(actors))
	actionChan := make(chan actionEvent, len(actors))
	errCh = make(chan status, len(actors)+2)

	// Start the collector.
	var wgcol sync.WaitGroup
	wgcol.Add(1)
	colCtx, colDone := context.WithCancel(ctx)
	go func() {
		colCtx = logtags.AddTag(colCtx, "collector", nil)
		log.Info(colCtx, "<intrat>")
		if err := collect(colCtx, dataLogger, actionChan, spotlightChan); err != nil {
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
		actorChan := make(chan string)
		actorChans[actName] = actorChan
		wg.Add(1)
		go func(ctx context.Context, a *actor) {
			if err := a.run(ctx, &wg, monLogger, actionChan, spotlightChan, actorChan); err != nil {
				errCh <- status{who: fmt.Sprintf("%s [%s]", a.role.name, a.name), err: err}
			}
			log.Info(ctx, "<exit>")
			wg.Done()
		}(actCtx, actors[actName])
	}

	// Start the prompter.
	wg.Add(1)
	go func() {
		dirCtx := logtags.AddTag(ctx, "prompter", nil)
		log.Info(dirCtx, "<intrat>")
		if err := prompt(dirCtx, actorChans); err != nil {
			errCh <- status{who: "prompter", err: err}
		}
		log.Info(dirCtx, "<exit>")
		wg.Done()
	}()

	// Wait for the prompter and actors to complete.
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

func prompt(ctx context.Context, actorChans map[string]chan string) error {
	startTime := time.Now().UTC()

	// At end:
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
		for _, ch := range actorChans {
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
			ch := actorChans[step.character]
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

func (a *actor) prepare(bctx context.Context) error {
	if cCmd := a.role.cleanupCmd; cCmd != "" {
		// If there is a cleanup command, run it before the prepare, to
		// ensure a pristime environment.
		a.cleanup(bctx)
	}

	pCmd := a.role.prepareCmd
	if pCmd == "" {
		return nil
	}
	// If the prepare command does not complete within 10 seconds, we'll terminate it.
	ctx, cancel := context.WithDeadline(bctx, time.Now().Add(10*time.Second))
	defer cancel()
	cmd := a.makeShCmd(pCmd)
	log.Infof(ctx, "prepare: %s", strings.Join(cmd.Args, " "))
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				cmd.Process.Signal(os.Interrupt)
				time.Sleep(1)
				cmd.Process.Kill()
			}
		}
	}()
	outdata, err := cmd.CombinedOutput()
	log.Infof(ctx, "prepare done\n%s\n-- %s", string(outdata), cmd.ProcessState.String())
	return err
}

func (a *actor) makeShCmd(pcmd cmd) exec.Cmd {
	cmd := exec.Cmd{
		Path: shellPath,
		Dir:  a.workDir,
		// set -euxo pipefail:
		//    -e fail commands on error
		//    -x trace commands (and show variable expansions)
		//    -u fail command if a variable is not set
		//    -o pipefail   fail entire pipeline if one command fails
		// trap: terminate all the process group when the shell exits.
		Args: []string{
			shellPath,
			"-c",
			`set -euo pipefail; export TMPDIR=$PWD HOME=$PWD/..; shpid=$$; trap "set +x; kill -TERM -$shpid 2>/dev/null || true" EXIT; set -x;` + "\n" + string(pcmd)},
	}
	if a.extraEnv != "" {
		cmd.Path = "/usr/bin/env"
		cmd.Args = append([]string{"/usr/bin/env", "-S", a.extraEnv}, cmd.Args...)
	}
	return cmd
}

func (a *actor) cleanup(ctx context.Context) {
	cCmd := a.role.cleanupCmd
	cmd := a.makeShCmd(cCmd)
	log.Infof(ctx, "cleanup: %s", strings.Join(cmd.Args, " "))
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
	actionChan chan<- actionEvent,
	spotlightChan chan<- dataEvent,
	events <-chan string,
) error {
	if a.role.spotlightCmd != "" {
		monCtx, monCancel := context.WithCancel(ctx)
		monCtx = logtags.AddTag(monCtx, "spotlight", nil)

		log.Info(monCtx, "<intrat>")
		wg.Add(1)
		go func() {
			a.spotlight(monCtx, monLogger, spotlightChan)
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
			cmd := a.makeShCmd(aCmd)
			log.Infof(ctx, "executing %q: %s", ev, strings.Join(cmd.Args, " "))
			actStart := time.Now()
			outdata, err := cmd.CombinedOutput()
			actEnd := time.Now()
			if _, ok := err.(*exec.ExitError); err != nil && !ok {
				log.Errorf(ctx, "exec error: %+v", err)
				continue
			}
			if err := a.reportAction(ctx, actionChan, ev,
				actStart, actEnd,
				string(outdata), cmd.ProcessState); err != nil {
				return err
			}
		}
	}

	return nil
}

func (a *actor) reportAction(
	ctx context.Context,
	actionChan chan<- actionEvent,
	action string,
	actStart, actEnd time.Time,
	outdata string,
	ps *os.ProcessState,
) error {
	dur := actEnd.Sub(actStart)
	log.Infof(ctx, "%q done (%s)\n%s-- %s", action, dur, outdata, ps)
	ev := actionEvent{
		startTime: actStart,
		duration:  dur.Seconds(),
		actor:     a.name,
		action:    action,
		success:   ps.Success(),
		output:    html.EscapeString(ps.String()),
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case actionChan <- ev:
		// ok
	}
	return nil
}
