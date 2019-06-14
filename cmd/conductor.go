package main

import (
	"context"
	"html"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/knz/shakespeare/cmd/log"
	"github.com/knz/shakespeare/cmd/log/logtags"
	"github.com/knz/shakespeare/cmd/stop"
	"github.com/knz/shakespeare/cmd/timeutil"
)

// conduct runs the play.
func (ap *app) conduct(ctx context.Context) (err error) {
	// Prepare all the working directories.
	for _, a := range ap.cfg.actors {
		if err := os.MkdirAll(a.workDir, os.ModePerm); err != nil {
			return errors.Wrapf(err, "mkdir %s", a.workDir)
		}
	}

	// Initialize all the actors.
	if err := ap.runCleanup(ctx); err != nil {
		return err
	}

	// Ensure the cleanup actions are run at the end
	// even during early return.
	defer func() {
		if cleanupErr := ap.runCleanup(ctx); cleanupErr != nil {
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

	// Start the audition. This initializes the epoch, and thus needs to
	// happen before the collector and the auditors start.
	ap.au.start(ctx)

	errCh := make(chan error, len(ap.cfg.actors)+2)
	defer func() {
		close(errCh)
		err = collectErrors(ctx, errCh, "play")
	}()

	spotlightChan := make(chan dataEvent, len(ap.cfg.actors))
	actionChan := make(chan actionEvent, len(ap.cfg.actors))

	// Start the collector.
	// The collector is running in the background.
	var wgcol sync.WaitGroup
	colDone := ap.startCollector(ctx, &wgcol, dataLogger, actionChan, spotlightChan, errCh)
	defer func() {
		log.Info(ctx, "cancelling collector")
		colDone()
		wgcol.Wait()
	}()

	// Start the spotlights.
	// The spotlights are running in the background until canceled
	// via allSpotsDone().
	var wgspot sync.WaitGroup
	allSpotsDone := ap.startSpotlights(ctx, &wgspot, monLogger, spotlightChan, errCh)
	defer func() {
		log.Info(ctx, "cancelling spotlights")
		allSpotsDone()
		wgspot.Wait()
	}()

	// Run the prompter.
	// The play steps will thus run in the main thread.
	ap.runPrompter(ctx, actionChan, errCh)

	// errors are collected by the defer above.
	return nil
}

// runPrompter runs the prompter until completion.
func (ap *app) runPrompter(ctx context.Context, actionChan chan<- actionEvent, errCh chan<- error) {
	dirCtx := logtags.AddTag(ctx, "prompter", nil)
	log.Info(dirCtx, "<intrat>")
	if err := ap.prompt(dirCtx, actionChan); err != nil {
		errCh <- errors.Wrap(err, "prompter")
	}
	log.Info(dirCtx, "<exit>")
}

// startCollector starts the collector in the background.
func (ap *app) startCollector(
	ctx context.Context,
	wg *sync.WaitGroup,
	dataLogger *log.SecondaryLogger,
	actionChan <-chan actionEvent,
	spotlightChan <-chan dataEvent,
	errCh chan<- error,
) (cancelFunc func()) {
	colCtx, colDone := context.WithCancel(ctx)
	colCtx = logtags.AddTag(colCtx, "collector", nil)
	wg.Add(1)
	runWorker(colCtx, ap.stopper, func(ctx context.Context) {
		defer wg.Done()
		log.Info(colCtx, "<intrat>")
		if err := ap.collect(colCtx, dataLogger, actionChan, spotlightChan); err != nil && err != context.Canceled {
			// We ignore cancellation errors here, so as to avoid reporting
			// a general error when the collector is merely canceled at the
			// end of the play.
			errCh <- errors.Wrap(err, "collector")
		}
		log.Info(colCtx, "<exit>")
	})
	return colDone
}

// startSpotlights starts all the spotlights in the background.
func (ap *app) startSpotlights(
	ctx context.Context,
	wg *sync.WaitGroup,
	monLogger *log.SecondaryLogger,
	spotlightChan chan<- dataEvent,
	errCh chan<- error,
) (cancelFunc func()) {
	allSpotsCtx, allSpotsDone := context.WithCancel(ctx)
	for actName, thisActor := range ap.cfg.actors {
		if thisActor.role.spotlightCmd == "" {
			// No spotlight defined, don't start anything.
			continue
		}
		spotCtx := logtags.AddTag(allSpotsCtx, "spotlight", nil)
		spotCtx = logtags.AddTag(spotCtx, "actor", actName)
		spotCtx = logtags.AddTag(spotCtx, "role", thisActor.role.name)
		a := thisActor
		wg.Add(1)
		runWorker(spotCtx, ap.stopper, func(ctx context.Context) {
			defer wg.Done()
			log.Info(spotCtx, "<shining>")
			if err := a.spotlight(ctx, ap.stopper, monLogger, spotlightChan); err != nil && err != context.Canceled {
				// We ignore cancellation errors here, so as to avoid reporting
				// a general error when a spotlight is merely canceled at the
				// end of the play.
				errCh <- errors.Wrapf(err, "%s [%s]", a.role.name, a.name)
			}
			log.Info(ctx, "<off>")
		})
	}
	return allSpotsDone
}

func (ap *app) runCleanup(ctx context.Context) error {
	return ap.runForAllActors(ctx, "cleanup", func(a *actor) cmd { return a.role.cleanupCmd })
}

func (ap *app) runForAllActors(
	ctx context.Context, prefix string, getCommand func(a *actor) cmd,
) error {
	errCh := make(chan error, len(ap.cfg.actors))
	var wg sync.WaitGroup
	for actName, thisActor := range ap.cfg.actors {
		pCmd := getCommand(thisActor)
		if pCmd == "" {
			// No command to run. Nothing to do.
			continue
		}
		actCtx := logtags.AddTag(ctx, prefix, nil)
		actCtx = logtags.AddTag(actCtx, "actor", actName)
		actCtx = logtags.AddTag(actCtx, "role", thisActor.role.name)
		a := thisActor
		wg.Add(1)
		runWorker(actCtx, ap.stopper, func(ctx context.Context) {
			defer wg.Done()
			// Start one actor.
			log.Info(ctx, "<start>")
			if err := a.runActorCommand(ctx, ap.stopper, pCmd); err != nil {
				errCh <- errors.Wrapf(err, "%s %s", a.role.name, a.name)
			}
			log.Info(ctx, "<done>")
		})
	}
	wg.Wait()

	close(errCh)
	return collectErrors(ctx, errCh, prefix)
}

func collectErrors(ctx context.Context, errCh <-chan error, prefix string) error {
	numErr := 0
	err := errors.New("collected errors")
	for stErr := range errCh {
		log.Errorf(ctx, "complaint during %s: %+v", prefix, stErr)
		err = errors.WithSecondaryError(err, stErr)
		numErr++
	}
	if numErr > 0 {
		return errors.Wrapf(err, "%d %s errors", numErr, prefix)
	}
	return nil
}

type moodPeriod struct {
	startTime float64
	endTime   float64
	mood      string
}

func (ap *app) prompt(ctx context.Context, actionChan chan<- actionEvent) error {
	startTime := timeutil.Now()

	for i, scene := range ap.cfg.play {
		sceneCtx := logtags.AddTag(ctx, "scene", i)

		log.Info(sceneCtx, showRunning(ap.stopper))

		now := timeutil.Now()
		elapsed := now.Sub(startTime)
		toWait := scene.waitUntil - elapsed
		if toWait > 0 {
			log.Infof(sceneCtx, "waiting for %.2fs", toWait.Seconds())
		} else {
			if toWait < 0 {
				log.Infof(sceneCtx, "running behind schedule: %s", toWait)
			}
		}
		// Note: we have to fire a timer in any case, otherwise
		// we'll be stuck waiting for the stopper / context cancellation.
		tm := time.After(toWait)
		select {
		case <-ap.stopper.ShouldStop():
			log.Info(ctx, "interrupted")
			return nil
		case <-ctx.Done():
			log.Info(ctx, "canceled")
			return ctx.Err()
		case <-tm:
			// Wait.
		}

		if err := ap.runLines(sceneCtx, startTime, scene.concurrentLines, actionChan); err != nil {
			return err
		}
	}

	return nil
}

func (ap *app) runLines(
	ctx context.Context, startTime time.Time, lines []scriptLine, actionChan chan<- actionEvent,
) (err error) {
	errCh := make(chan error, len(lines))
	defer func() {
		close(errCh)
		err = collectErrors(ctx, errCh, "prompt")
	}()

	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
	}()

	for _, line := range lines {
		a := line.actor
		steps := line.steps
		lineCtx := logtags.AddTag(ctx, "actor", a.name)
		lineCtx = logtags.AddTag(lineCtx, "role", a.role.name)
		wg.Add(1)
		if err := runAsyncTask(lineCtx, ap.stopper, func(ctx context.Context) {
			defer wg.Done()
			if err := a.runLine(ctx, ap.stopper, startTime, steps, actionChan); err != nil {
				errCh <- errors.Wrapf(err, "%s %s", a.role.name, a.name)
			}
		}); err != nil {
			wg.Done()
			if err != stop.ErrUnavailable {
				errCh <- errors.Wrap(err, "stopper")
			}
		}
	}

	// errors are collected by the defer above.
	return nil
}

func (a *actor) runLine(
	ctx context.Context,
	stopper *stop.Stopper,
	startTime time.Time,
	steps []step,
	actionChan chan<- actionEvent,
) error {
	for stepNum, step := range steps {
		stepCtx := logtags.AddTag(ctx, "step", stepNum+1)

		switch step.typ {
		case stepAmbiance:
			log.Infof(stepCtx, "(mood %s)", step.action)
			if err := reportMoodEvt(ctx, stopper, step.action, actionChan); err != nil {
				return err
			}

		case stepDo:
			log.Infof(stepCtx, "%s: %s!", a.name, step.action)
			if err := a.runAction(stepCtx, stopper, step.action, actionChan); err != nil {
				return err
			}
		}
	}
	return nil
}

func reportMoodEvt(
	ctx context.Context, stopper *stop.Stopper, mood string, actionChan chan<- actionEvent,
) error {
	ev := actionEvent{
		typ:       actEvtMood,
		startTime: timeutil.Now(),
		output:    mood,
	}
	select {
	case <-stopper.ShouldStop():
		log.Info(ctx, "interrupted")
		return nil
	case <-ctx.Done():
		log.Info(ctx, "canceled")
		return ctx.Err()
	case actionChan <- ev:
		// ok
	}
	return nil
}

func (a *actor) runAction(
	ctx context.Context, stopper *stop.Stopper, action string, actionChan chan<- actionEvent,
) error {
	aCmd, ok := a.role.actionCmds[action]
	if !ok {
		return errors.Errorf("unknown action: %q", action)
	}

	cmd := a.makeShCmd(aCmd)
	log.Infof(ctx, "executing %q: %s", action, strings.Join(cmd.Args, " "))

	complete := make(chan struct{})
	var wg sync.WaitGroup
	defer func() {
		close(complete)
		wg.Wait()
	}()

	stopCtx := logtags.AddTag(ctx, "action stopper", nil)
	wg.Add(1)
	runWorker(stopCtx, stopper, func(ctx context.Context) {
		defer wg.Done()
		select {
		case <-complete:
			return
		case <-stopper.ShouldStop():
			log.Info(ctx, "interrupted")
		case <-ctx.Done():
			log.Info(ctx, "canceled")
		}
		proc := cmd.Process
		if proc != nil {
			proc.Signal(os.Interrupt)
			time.Sleep(1)
			proc.Kill()
		}
	})

	actStart := timeutil.Now()
	outdata, err := cmd.CombinedOutput()
	actEnd := timeutil.Now()

	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		return err
	}

	return a.reportAction(ctx, stopper, actionChan, action,
		actStart, actEnd,
		string(outdata), cmd.ProcessState)
}

func (a *actor) runActorCommand(bctx context.Context, stopper *stop.Stopper, pCmd cmd) error {
	// If the command does not complete within 10 seconds, we'll terminate it.
	// Note: we are not considering the stopper here, because
	// we want cleanup actions to execute even during interrupt shutdown.
	ctx, cancel := context.WithDeadline(bctx, timeutil.Now().Add(10*time.Second))
	defer cancel()
	cmd := a.makeShCmd(pCmd)
	log.Infof(ctx, "running: %s", strings.Join(cmd.Args, " "))

	complete := make(chan struct{})
	defer func() { close(complete) }()

	stopCtx := logtags.AddTag(ctx, "cmd stopper", nil)
	runWorker(stopCtx, stopper, func(ctx context.Context) {
		select {
		case <-complete:
			return
		// case <-stopper.ShouldStop():
		//	log.Info(ctx, "interrupted")
		case <-ctx.Done():
			log.Info(ctx, "canceled")
		}
		proc := cmd.Process
		if proc != nil {
			proc.Signal(os.Interrupt)
			time.Sleep(1)
			proc.Kill()
		}
	})

	outdata, err := cmd.CombinedOutput()
	log.Infof(ctx, "done\n%s\n-- %s", string(outdata), cmd.ProcessState.String())
	return err
}

func (a *actor) makeShCmd(pcmd cmd) exec.Cmd {
	cmd := exec.Cmd{
		Path: a.shellPath,
		Dir:  a.workDir,
		// set -euxo pipefail:
		//    -e fail commands on error
		//    -x trace commands (and show variable expansions)
		//    -u fail command if a variable is not set
		//    -o pipefail   fail entire pipeline if one command fails
		// trap: terminate all the process group when the shell exits.
		Args: []string{
			a.shellPath,
			"-c",
			`set -euo pipefail; export TMPDIR=$PWD HOME=$PWD/..; shpid=$$; trap "set +x; kill -TERM -$shpid 2>/dev/null || true" EXIT; set -x;` + "\n" + string(pcmd)},
	}
	if a.extraEnv != "" {
		cmd.Path = "/usr/bin/env"
		cmd.Args = append([]string{"/usr/bin/env", "-S", a.extraEnv}, cmd.Args...)
	}
	return cmd
}

func (a *actor) reportAction(
	ctx context.Context,
	stopper *stop.Stopper,
	actionChan chan<- actionEvent,
	action string,
	actStart, actEnd time.Time,
	outdata string,
	ps *os.ProcessState,
) error {
	dur := actEnd.Sub(actStart)
	log.Infof(ctx, "%q done (%s)\n%s-- %s", action, dur, outdata, ps)
	ev := actionEvent{
		typ:       actEvtExec,
		startTime: actStart,
		duration:  dur.Seconds(),
		actor:     a.name,
		action:    action,
		success:   ps.Success(),
		output:    html.EscapeString(ps.String()),
	}
	select {
	case <-stopper.ShouldStop():
		log.Info(ctx, "interrupted")
		return nil
	case <-ctx.Done():
		log.Info(ctx, "canceled")
		return ctx.Err()
	case actionChan <- ev:
		// ok
	}
	return nil
}
