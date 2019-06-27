package cmd

import (
	"context"
	"fmt"
	"html"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/stop"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

// prompt directs the actors to perform actions in accordance with the script.
func (ap *app) prompt(
	ctx context.Context,
	actionChan chan<- actionReport,
	moodCh chan<- moodChange,
	actCh chan<- actChange,
) error {
	// surpriseDur is the duration of a scene beyond which the prompter
	// will express surprise.
	surpriseDur := 2 * ap.cfg.tempo.Seconds()

	// Play.
	for j, act := range ap.cfg.play {
		actNum := j + 1
		actCtx := logtags.AddTag(ctx, "act", actNum)
		actStart := timeutil.Now()
		if err := ap.signalActChange(actCtx, actCh, actStart, actNum); err != nil {
			return err
		}

		for i, scene := range act {
			sceneNum := i + 1
			sceneCtx := logtags.AddTag(actCtx, "scene", sceneNum)

			// log.Info(sceneCtx, showRunning(ap.stopper))

			// Determine the amount of time to wait before the start of the
			// next scene.
			var toWait time.Duration
			if scene.waitUntil != 0 {
				elapsed := timeutil.Now().Sub(actStart)
				toWait = scene.waitUntil - elapsed
				if toWait > 0 {
					log.Infof(sceneCtx, "at %.4fs, waiting for %.4fs", elapsed.Seconds(), toWait.Seconds())
				}
			}

			// Now wait for that time. Note: we have to fire a timer in any
			// case, to have something to select on - we need a select to also
			// test the context cancellation or stopper.ShouldQuiesce().
			tm := time.After(toWait)
			select {
			case <-ap.stopper.ShouldQuiesce():
				log.Info(sceneCtx, "terminated")
				return nil
			case <-ctx.Done():
				log.Info(sceneCtx, "interrupted")
				return wrapCtxErr(ctx)
			case <-tm:
				// Wait.
			}

			if scene.isEmpty() {
				// Nothing to do further in this scene.
				continue
			}

			extraMsg := ""
			if toWait < -time.Duration(1)*time.Millisecond {
				extraMsg = fmt.Sprintf(", running %.4fs behind schedule", -toWait.Seconds())
			}

			sceneTime := timeutil.Now()
			elapsedTotal := sceneTime.Sub(ap.au.epoch)
			elapsedAct := sceneTime.Sub(actStart)
			ap.narrate(I, "", "act %d, scene %d (~%ds total, ~%ds in act%s):",
				actNum, sceneNum, int(elapsedTotal.Seconds()), int(elapsedAct.Seconds()), extraMsg)

			// Now run the scene.
			err := ap.runScene(sceneCtx, scene.concurrentLines, actionChan, moodCh)

			// In any case, make a statement about the duration.
			if ap.cfg.tempo != 0 {
				sceneDur := timeutil.Now().Sub(sceneTime).Seconds()
				if sceneDur >= surpriseDur {
					ap.narrate(W, "😮", "woah! scene %d lasted %.1fx longer than expected!",
						sceneNum, sceneDur/ap.cfg.tempo.Seconds())
				}
			}

			// If there was an error, return it.
			if err != nil {
				return errors.WithContextTags(err, ctx)
			}
		}
	}

	return nil
}

// runScene runs one scene.
// A scene is the concurrent execution of each actor's lines.
func (ap *app) runScene(
	ctx context.Context, lines []scriptLine, actionChan chan<- actionReport, moodCh chan<- moodChange,
) (err error) {
	// errCh collects the errors from the concurrent actors.
	errCh := make(chan error, len(lines)+1)

	stopCtx, stopOnError := context.WithCancel(ctx)
	defer stopOnError()

	defer func() {
		if r := recover(); r != nil {
			panic(r)
		}
		// At the end of the scene, make runScene() return the collected
		// errors.
		err = collectErrors(ctx, nil, errCh, "prompt")
	}()

	// We'll cancel any remaining actions, and wait, at the end of the
	// scene.
	var wg sync.WaitGroup
	defer func() { wg.Wait() }()

	for _, line := range lines {
		a := line.actor
		if a == nil {
			// No actor: this is just a mood change.
			// Ensure collectErrors finishes in any case.
			errCh <- nil
			if len(line.steps) != 1 || (len(line.steps) > 0 && line.steps[0].typ != stepAmbiance) {
				return errors.WithContextTags(errors.AssertionFailedf("unexpected missing actor"), ctx)
			}
			if err := ap.runMoodChange(ctx, line.steps[0].action, actionChan, moodCh); err != nil {
				return err
			}
			continue
		}
		// Start the scene for this actor.
		steps := line.steps
		lineCtx := logtags.AddTag(stopCtx, "actor", a.name)
		lineCtx = logtags.AddTag(lineCtx, "role", a.role.name)
		wg.Add(1)
		// we use runAsyncTask() here instead of runWorker(), so that if
		// the stopper is requesting a stop by the time we reach this
		// point, the task won't even start.
		if err := runAsyncTask(lineCtx, ap.stopper, func(ctx context.Context) {
			defer wg.Done()
			err := ap.runLine(ctx, a, steps, actionChan, moodCh)
			if errors.Is(err, context.Canceled) {
				// It's ok if an action gets aborted.
				err = nil
			}
			errCh <- err
			if err != nil {
				// Stop the concurrent actions.
				// We wait for a few moments to leave any concurrent failing
				// actions a chance to report their failure, too.
				// This is useful in the (relatively common) case
				// where a user mistake causes the action to fail immediately.
				time.Sleep(time.Second / 2)
				stopOnError()
			}
		}); err != nil {
			// runAsyncTask() returns err when the task hasn't
			// started. However, we still need to wait on the sync group, so
			// we have to signal the task was given up.
			wg.Done()
			errCh <- errors.WithContextTags(err, ctx)
		}
	}
	if len(lines) == 0 {
		// Nothing was launched, ensure collectErrors finishes in any case.
		errCh <- nil
	}

	// errors are collected by the defer above.
	return nil
}

// runLine runs a script scene for just one actor.
func (ap *app) runLine(
	ctx context.Context,
	a *actor,
	steps []step,
	actionChan chan<- actionReport,
	moodCh chan<- moodChange,
) error {
	for stepNum, step := range steps {
		stepCtx := logtags.AddTag(ctx, "step", stepNum+1)

		switch step.typ {
		case stepAmbiance:
			if err := ap.runMoodChange(stepCtx, step.action, actionChan, moodCh); err != nil {
				return err
			}

		case stepDo:
			qc := '!'
			if step.failOk {
				qc = '?'
			}
			ap.narrate(I, "🥁", "    %s: %s%c", a.name, step.action, qc)
			ev, err := a.runAction(stepCtx, ap.stopper, ap.au.epoch, step.action, actionChan)
			if err != nil {
				return err
			}
			ev.failOk = step.failOk
			reportErr := reportActionEvent(stepCtx, ap.stopper, actionChan, ev)
			if ev.result != resOk && !step.failOk {
				return combineErrors(reportErr,
					errors.WithContextTags(
						errors.Newf("action %s:%s failed: %s\n%s",
							ev.actor, ev.action, ev.output, ev.extOutput), stepCtx))
			}
			if reportErr != nil {
				return reportErr
			}
		}
	}
	return nil
}

func (ap *app) signalActChange(
	ctx context.Context, actCh chan<- actChange, actStart time.Time, actNum int,
) error {
	ap.narrate(I, "🎬", "act %d starts", actNum)
	elapsed := actStart.Sub(ap.au.epoch).Seconds()
	ev := actChange{ts: elapsed, actNum: actNum}
	select {
	case <-ap.stopper.ShouldQuiesce():
		log.Info(ctx, "terminated")
		return nil
	case <-ctx.Done():
		log.Info(ctx, "interrupted")
		return wrapCtxErr(ctx)
	case actCh <- ev:
		// ok
	}
	return nil
}

func (ap *app) runMoodChange(
	ctx context.Context, newMood string, actionChan chan<- actionReport, moodCh chan<- moodChange,
) error {
	ap.narrate(I, "🎊", "    (mood %s)", newMood)
	now := timeutil.Now()
	elapsed := now.Sub(ap.au.epoch).Seconds()
	if err := reportMoodEvent(ctx, ap.stopper, moodCh,
		moodChange{ts: elapsed, newMood: newMood}); err != nil {
		return err
	}
	ev := actionReport{
		typ:       reportMoodChange,
		startTime: elapsed,
		output:    newMood,
	}
	return reportActionEvent(ctx, ap.stopper, actionChan, ev)
}

func reportActionEvent(
	ctx context.Context, stopper *stop.Stopper, actionChan chan<- actionReport, ev actionReport,
) error {
	select {
	case <-stopper.ShouldQuiesce():
		log.Info(ctx, "terminated")
		return nil
	case <-ctx.Done():
		log.Info(ctx, "interrupted")
		return wrapCtxErr(ctx)
	case actionChan <- ev:
		// ok
	}
	return nil
}

func reportMoodEvent(
	ctx context.Context, stopper *stop.Stopper, moodCh chan<- moodChange, chg moodChange,
) error {
	select {
	case <-stopper.ShouldQuiesce():
		log.Info(ctx, "terminated")
		return nil
	case <-ctx.Done():
		log.Info(ctx, "interrupted")
		return wrapCtxErr(ctx)
	case moodCh <- chg:
		// ok
	}
	return nil
}

func (a *actor) runAction(
	ctx context.Context,
	stopper *stop.Stopper,
	epoch time.Time,
	action string,
	actionChan chan<- actionReport,
) (actionReport, error) {
	aCmd, ok := a.role.actionCmds[action]
	if !ok {
		return actionReport{}, errors.Errorf("unknown action: %q", action)
	}
	ctx = logtags.AddTag(ctx, "action", action)

	actStart := timeutil.Now()
	outdata, ps, err, exitErr := a.runActorCommand(ctx, stopper, 0 /*timeout*/, true /*interruptible*/, aCmd)
	actEnd := timeutil.Now()

	dur := actEnd.Sub(actStart)
	log.Infof(ctx, "%q done (%s)\n%s-- %s (%v / %v)", action, dur, outdata, ps, err, exitErr)

	if err != nil && ps == nil {
		// If we don't have a process status,
		// the command was not even executed.
		return actionReport{}, err
	}
	var result result
	switch {
	case ps == nil:
		result = resErr
	case err == nil && ps.Success():
		result = resOk
	default:
		result = resFailure
	}

	var combinedErrOutput string
	if err != nil {
		combinedErrOutput = fmt.Sprintf("%v / %v", err, ps)
	} else {
		combinedErrOutput = ps.String()
	}
	ev := actionReport{
		typ:       reportActionExec,
		startTime: actStart.Sub(epoch).Seconds(),
		duration:  dur.Seconds(),
		actor:     a.name,
		action:    action,
		result:    result,
		output:    html.EscapeString(combinedErrOutput),
		extOutput: outdata,
	}
	return ev, nil
}
