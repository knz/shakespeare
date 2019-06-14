package main

import (
	"context"
	"html"
	"os/exec"
	"sync"
	"time"

	"github.com/knz/shakespeare/cmd/log"
	"github.com/knz/shakespeare/cmd/log/logtags"
	"github.com/knz/shakespeare/cmd/stop"
	"github.com/knz/shakespeare/cmd/timeutil"
	"github.com/pkg/errors"
)

// prompt directs the actors to perform actions in accordance with the script.
func (ap *app) prompt(ctx context.Context, actionChan chan<- actionEvent) error {
	lastReport := timeutil.Now()
	for i, scene := range ap.cfg.play {
		sceneCtx := logtags.AddTag(ctx, "scene", i)

		now := timeutil.Now()
		if now.Sub(lastReport) >= time.Second {
			ap.report("... now playing: scene %d ...", i)
			lastReport = now
		}

		// log.Info(sceneCtx, showRunning(ap.stopper))

		// Determine the amount of time to wait before the start of the
		// next scene.
		elapsed := now.Sub(ap.au.epoch)
		toWait := scene.waitUntil - elapsed
		if toWait > 0 {
			log.Infof(sceneCtx, "waiting for %.2fs", toWait.Seconds())
		} else {
			if toWait < 0 {
				log.Infof(sceneCtx, "running behind schedule: %s", toWait)
			}
		}

		// Now wait for that time. Note: we have to fire a timer in any
		// case, to have something to select on - we need a select to also
		// test the context cancellation or stopper.ShouldStop().
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

		// Now run the scene.
		if err := ap.runScene(sceneCtx, scene.concurrentLines, actionChan); err != nil {
			return err
		}
	}

	return nil
}

// runScene runs one scene.
// A scene is the concurrent execution of each actor's lines.
func (ap *app) runScene(
	ctx context.Context, lines []scriptLine, actionChan chan<- actionEvent,
) (err error) {
	// errCh collects the errors from the concurrent actors.
	errCh := make(chan error, len(lines))
	defer func() {
		// At the end of the scene, make runScene() return the collected
		// errors.
		close(errCh)
		err = collectErrors(ctx, errCh, "prompt")
	}()

	// There is a barrier at the end of the scene.
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
	}()

	for _, line := range lines {
		// Start the scene for this actor.
		a := line.actor
		steps := line.steps
		lineCtx := logtags.AddTag(ctx, "actor", a.name)
		lineCtx = logtags.AddTag(lineCtx, "role", a.role.name)
		wg.Add(1)
		// we use runAsyncTask() here instead of runWorker(), so that if
		// the stopper is requesting a stop by the time we reach this
		// point, the task won't even start.
		if err := runAsyncTask(lineCtx, ap.stopper, func(ctx context.Context) {
			defer wg.Done()
			if err := a.runLine(ctx, ap.stopper, ap.au.epoch, steps, actionChan); err != nil {
				errCh <- errors.Wrapf(err, "%s %s", a.role.name, a.name)
			}
		}); err != nil {
			// runAsyncTask() returns err when the task hasn't
			// started. However, we still need to wait on the sync group, so
			// we have to signal the task was given up.
			wg.Done()
			if err != stop.ErrUnavailable {
				// that should never happen, but just in case report it.
				errCh <- errors.Wrap(err, "stopper")
			}
		}
	}

	// errors are collected by the defer above.
	return nil
}

// runLine runs a script scene for just one actor.
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
			ev := actionEvent{
				typ:       actEvtMood,
				startTime: timeutil.Now(),
				output:    step.action,
			}
			if err := a.reportActionEvent(ctx, stopper, actionChan, ev); err != nil {
				return err
			}

		case stepDo:
			log.Infof(stepCtx, "%s: %s!", a.name, step.action)
			ev, err := a.runAction(stepCtx, stopper, step.action, actionChan)
			if err != nil {
				return err
			}
			if err := a.reportActionEvent(ctx, stopper, actionChan, ev); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *actor) reportActionEvent(
	ctx context.Context, stopper *stop.Stopper, actionChan chan<- actionEvent, ev actionEvent,
) error {
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
) (actionEvent, error) {
	aCmd, ok := a.role.actionCmds[action]
	if !ok {
		return actionEvent{}, errors.Errorf("unknown action: %q", action)
	}
	ctx = logtags.AddTag(ctx, "action", action)

	actStart := timeutil.Now()
	outdata, ps, err := a.runActorCommand(ctx, stopper, 0 /*timeout*/, true /*interruptible*/, aCmd)
	actEnd := timeutil.Now()

	dur := actEnd.Sub(actStart)
	log.Infof(ctx, "%q done (%s)\n%s-- %s", action, dur, outdata, ps)

	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		return actionEvent{}, err
	}

	ev := actionEvent{
		typ:       actEvtExec,
		startTime: actStart,
		duration:  dur.Seconds(),
		actor:     a.name,
		action:    action,
		success:   ps.Success(),
		output:    html.EscapeString(ps.String()),
	}
	return ev, nil
}
