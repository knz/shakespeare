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
	"github.com/knz/shakespeare/cmd/timeutil"
)

type status struct {
	who string
	err error
}

// conduct runs the play.
func conduct(ctx context.Context) (err error) {
	// Prepare all the working directories.
	for _, a := range actors {
		if err := os.MkdirAll(a.workDir, os.ModePerm); err != nil {
			return fmt.Errorf("mkdir %s: %+v", a.workDir, err)
		}
	}

	// Initialize all the actors.
	if err := runCleanup(ctx); err != nil {
		return err
	}

	// Ensure the cleanup actions are run at the end
	// even during early return.
	defer func() {
		if cleanupErr := runCleanup(ctx); cleanupErr != nil {
			// Error during cleanup. runCleanup already
			// printed out the details via log.Errorf.
			if err == nil {
				// We only propagate the cleanup error as final error
				// if there was no error yet.
				err = cleanupErr
			}
		}
	}()

	// We'll log all the monitored and extracted data to a secondary logger.
	monLogger := log.NewSecondaryLogger(ctx, nil, "spotlight", true /*enableGc*/, false /*forceSyncWrite*/)
	dataLogger := log.NewSecondaryLogger(ctx, nil, "collector", true /*enableGc*/, false /*forceSyncWrite*/)
	defer func() { log.Flush() }()

	errCh := make(chan status, len(actors)+2)
	spotlightChan := make(chan dataEvent, len(actors))
	actionChan := make(chan actionEvent, len(actors))

	// Start the collector.
	// The collector is running in the background.
	var wgcol sync.WaitGroup
	colDone := startCollector(ctx, &wgcol, dataLogger, actionChan, spotlightChan, errCh)

	// Start the spotlights.
	// The spotlights are running in the background until canceled
	// via allSpotsDone().
	var wgspot sync.WaitGroup
	allSpotsDone := startSpotlights(ctx, &wgspot, monLogger, spotlightChan, errCh)

	// Run the prompter.
	// The play steps will thus run in the main thread.
	runPrompter(ctx, actionChan, errCh)

	// Stop all the spotlights and wait for them to turn off.
	allSpotsDone()
	wgspot.Wait()

	// Stop the collector and wait for it to complete.
	colDone()
	wgcol.Wait()

	close(errCh)
	return collectErrors(ctx, errCh, "play")
}

// runPrompter runs the prompter until completion.
func runPrompter(ctx context.Context, actionChan chan<- actionEvent, errCh chan<- status) {
	dirCtx := logtags.AddTag(ctx, "prompter", nil)
	log.Info(dirCtx, "<intrat>")
	if err := prompt(dirCtx, actionChan); err != nil {
		errCh <- status{who: "prompter", err: err}
	}
	log.Info(dirCtx, "<exit>")
}

// startCollector starts the collector in the background.
func startCollector(
	ctx context.Context,
	wg *sync.WaitGroup,
	dataLogger *log.SecondaryLogger,
	actionChan <-chan actionEvent,
	spotlightChan <-chan dataEvent,
	errCh chan<- status,
) (cancelFunc func()) {
	wg.Add(1)
	colCtx, colDone := context.WithCancel(ctx)
	go func() {
		colCtx = logtags.AddTag(colCtx, "collector", nil)
		log.Info(colCtx, "<intrat>")
		if err := collect(colCtx, dataLogger, actionChan, spotlightChan); err != nil && err != context.Canceled {
			// We ignore cancellation errors here, so as to avoid reporting
			// a general error when the collector is merely canceled at the
			// end of the play.
			errCh <- status{who: "collector", err: err}
		}
		log.Info(colCtx, "<exit>")
		wg.Done()
	}()
	return colDone
}

// startSpotlights starts all the spotlights in the background.
func startSpotlights(
	ctx context.Context,
	wg *sync.WaitGroup,
	monLogger *log.SecondaryLogger,
	spotlightChan chan<- dataEvent,
	errCh chan<- status,
) (cancelFunc func()) {
	allSpotsCtx, allSpotsDone := context.WithCancel(ctx)
	for actName, thisActor := range actors {
		if thisActor.role.spotlightCmd == "" {
			// No spotlight defined, don't start anything.
			continue
		}
		spotCtx := logtags.AddTag(allSpotsCtx, "spotlight", nil)
		spotCtx = logtags.AddTag(spotCtx, "actor", actName)
		spotCtx = logtags.AddTag(spotCtx, "role", thisActor.role.name)
		log.Info(spotCtx, "<shining>")
		wg.Add(1)
		go func(ctx context.Context, a *actor) {
			if err := a.spotlight(ctx, monLogger, spotlightChan); err != nil && err != context.Canceled {
				// We ignore cancellation errors here, so as to avoid reporting
				// a general error when a spotlight is merely canceled at the
				// end of the play.
				errCh <- status{who: fmt.Sprintf("%s [%s]", a.role.name, a.name), err: err}
			}
			log.Info(ctx, "<off>")
			wg.Done()
		}(spotCtx, thisActor)
	}
	return allSpotsDone
}

func runCleanup(ctx context.Context) error {
	return runForAllActors(ctx, "cleanup", func(a *actor) cmd { return a.role.cleanupCmd })
}

func runForAllActors(ctx context.Context, prefix string, getCommand func(a *actor) cmd) error {
	errCh := make(chan status, len(actors))
	var wg sync.WaitGroup
	for actName, thisActor := range actors {
		pCmd := getCommand(thisActor)
		if pCmd == "" {
			// No command to run. Nothing to do.
			continue
		}
		// Start one actor.
		actCtx := logtags.AddTag(ctx, prefix, nil)
		actCtx = logtags.AddTag(actCtx, "actor", actName)
		actCtx = logtags.AddTag(actCtx, "role", thisActor.role.name)
		log.Info(actCtx, "<start>")
		wg.Add(1)
		go func(ctx context.Context, a *actor) {
			if err := a.runActorCommand(ctx, pCmd); err != nil {
				errCh <- status{who: fmt.Sprintf("%s %s", a.role.name, a.name), err: err}
			}
			log.Info(ctx, "<done>")
			wg.Done()
		}(actCtx, thisActor)
	}
	wg.Wait()

	close(errCh)
	return collectErrors(ctx, errCh, prefix)
}

func collectErrors(ctx context.Context, errCh <-chan status, prefix string) error {
	numErr := 0
	for st := range errCh {
		log.Errorf(ctx, "complaint from %s during %s: %+v", st.who, prefix, st.err)
		numErr++
	}
	if numErr > 0 {
		return fmt.Errorf("%d %s errors occurred", numErr, prefix)
	}
	return nil
}

var curAmbiance = "clear"
var curAmbianceStart float64 = math.Inf(-1)

type ambiancePeriod struct {
	startTime float64
	endTime   float64
	ambiance  string
}

var ambiances []ambiancePeriod

func prompt(ctx context.Context, actionChan chan<- actionEvent) error {
	startTime := timeutil.Now()

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
	}()

	for i, scene := range play {
		sceneCtx := logtags.AddTag(ctx, "act", i)

		now := timeutil.Now()
		elapsed := now.Sub(startTime)
		toWait := scene.waitUntil - elapsed
		if toWait > 0 {
			log.Infof(sceneCtx, "waiting for %.2fs", toWait.Seconds())
			tm := time.After(toWait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tm:
				// Wait.
			}
		} else {
			if toWait < 0 {
				log.Infof(sceneCtx, "running behind schedule: %s", toWait)
			}
		}

		if err := runLines(sceneCtx, startTime, scene.concurrentLines, actionChan); err != nil {
			return err
		}
	}

	return nil
}

func runLines(
	ctx context.Context, startTime time.Time, lines []scriptLine, actionChan chan<- actionEvent,
) error {
	errCh := make(chan status, len(lines))
	var wg sync.WaitGroup
	for _, line := range lines {
		wg.Add(1)
		go func(a *actor, steps []step) {
			actorCtx := logtags.AddTag(ctx, "role", a.role.name)
			actorCtx = logtags.AddTag(ctx, "actor", a.name)
			if err := a.runLine(actorCtx, startTime, steps, actionChan); err != nil {
				errCh <- status{who: fmt.Sprintf("%s %s", a.role.name, a.name), err: err}
			}
			wg.Done()
		}(line.actor, line.steps)
	}
	wg.Wait()
	close(errCh)
	return collectErrors(ctx, errCh, "prompt")
}

func (a *actor) runLine(
	ctx context.Context, startTime time.Time, steps []step, actionChan chan<- actionEvent,
) error {
	for stepNum, step := range steps {
		stepCtx := logtags.AddTag(ctx, "step", stepNum+1)

		now := timeutil.Now()
		elapsed := now.Sub(startTime)

		switch step.typ {
		case stepAmbiance:
			if step.action == curAmbiance {
				continue
			}
			log.Infof(stepCtx, "the mood changes to %s", step.action)
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
			log.Infof(stepCtx, "%s: %s!", a.name, step.action)
			if err := a.runAction(stepCtx, step.action, actionChan); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *actor) runAction(ctx context.Context, action string, actionChan chan<- actionEvent) error {
	aCmd, ok := a.role.actionCmds[action]
	if !ok {
		return fmt.Errorf("unknown action: %q", action)
	}

	cmd := a.makeShCmd(aCmd)
	log.Infof(ctx, "executing %q: %s", action, strings.Join(cmd.Args, " "))

	go func() {
		select {
		case <-ctx.Done():
			proc := cmd.Process
			if proc != nil {
				proc.Signal(os.Interrupt)
				time.Sleep(1)
				proc.Kill()
			}
			err := cmd.Wait()
			log.Infof(ctx, "command terminated: %+v", err)
		}
	}()

	actStart := timeutil.Now()
	outdata, err := cmd.CombinedOutput()
	actEnd := timeutil.Now()

	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		return err
	}

	return a.reportAction(ctx, actionChan, action,
		actStart, actEnd,
		string(outdata), cmd.ProcessState)
}

func (a *actor) runActorCommand(bctx context.Context, pCmd cmd) error {
	// If the command does not complete within 10 seconds, we'll terminate it.
	ctx, cancel := context.WithDeadline(bctx, timeutil.Now().Add(10*time.Second))
	defer cancel()
	cmd := a.makeShCmd(pCmd)
	log.Infof(ctx, "running: %s", strings.Join(cmd.Args, " "))
	go func() {
		select {
		case <-ctx.Done():
			proc := cmd.Process
			if proc != nil {
				proc.Signal(os.Interrupt)
				time.Sleep(1)
				proc.Kill()
			}
		}
	}()
	outdata, err := cmd.CombinedOutput()
	log.Infof(ctx, "done\n%s\n-- %s", string(outdata), cmd.ProcessState.String())
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
			actStart := timeutil.Now()
			outdata, err := cmd.CombinedOutput()
			actEnd := timeutil.Now()
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
