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
	ctx context.Context, actionChan chan<- actionReport, moodCh chan<- moodChange,
) error {
	// surpriseDur is the duration of a scene beyond which the prompter
	// will express surprise.
	surpriseDur := 2 * ap.cfg.tempo.Seconds()

	// Play.
	for i, scene := range ap.cfg.play {
		sceneNum := i + 1
		sceneCtx := logtags.AddTag(ctx, "scene", sceneNum)

		// log.Info(sceneCtx, showRunning(ap.stopper))

		// Determine the amount of time to wait before the start of the
		// next scene.
		elapsed := timeutil.Now().Sub(ap.au.epoch)
		toWait := scene.waitUntil - elapsed
		if toWait > 0 {
			log.Infof(sceneCtx, "at %.4fs, waiting for %.4fs", elapsed.Seconds(), toWait.Seconds())
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
		if toWait < 0 {
			extraMsg = fmt.Sprintf(", running %.4fs behind schedule", toWait.Seconds())
		}

		sceneTime := timeutil.Now()
		elapsed = sceneTime.Sub(ap.au.epoch)
		ap.narrate(I, "", "scene %d (~%ds in%s):", sceneNum, int(elapsed.Seconds()), extraMsg)

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

	return nil
}

// runScene runs one scene.
// A scene is the concurrent execution of each actor's lines.
func (ap *app) runScene(
	ctx context.Context, lines []scriptLine, actionChan chan<- actionReport, moodCh chan<- moodChange,
) (err error) {
	// errCh collects the errors from the concurrent actors.
	errCh := make(chan error, len(lines)+1)

	defer func() {
		// At the end of the scene, make runScene() return the collected
		// errors.
		err = collectErrors(ctx, nil, errCh, "prompt")
	}()

	// There is a barrier at the end of the scene.
	var wg sync.WaitGroup
	defer func() { wg.Wait() }()

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
			errCh <- ap.runLine(ctx, a, steps, actionChan, moodCh)
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
			ap.narrate(I, "🎊", "    (mood %s)", step.action)
			ts := timeutil.Now()
			if err := a.reportMoodEvent(stepCtx, ap.stopper, moodCh,
				moodChange{ts: ts, newMood: step.action}); err != nil {
				return err
			}
			ev := actionReport{
				typ:       reportMoodChange,
				startTime: ts,
				output:    step.action,
			}
			if err := a.reportActionEvent(stepCtx, ap.stopper, actionChan, ev); err != nil {
				return err
			}

		case stepDo:
			ap.narrate(I, "🥁", "    %s: %s!", a.name, step.action)
			ev, err := a.runAction(stepCtx, ap.stopper, step.action, actionChan)
			if err != nil {
				return err
			}
			if err := a.reportActionEvent(stepCtx, ap.stopper, actionChan, ev); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *actor) reportActionEvent(
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

func (a *actor) reportMoodEvent(
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
	ctx context.Context, stopper *stop.Stopper, action string, actionChan chan<- actionReport,
) (actionReport, error) {
	aCmd, ok := a.role.actionCmds[action]
	if !ok {
		return actionReport{}, errors.Errorf("unknown action: %q", action)
	}
	ctx = logtags.AddTag(ctx, "action", action)

	actStart := timeutil.Now()
	outdata, ps, err, exitErr := a.runActorCommand(ctx, stopper, 0 /*timeout*/, true /*interruptible*/, aCmd)
	if err != nil {
		return actionReport{}, err
	}
	actEnd := timeutil.Now()

	dur := actEnd.Sub(actStart)
	log.Infof(ctx, "%q done (%s)\n%s-- %s (%v)", action, dur, outdata, ps, exitErr)

	ev := actionReport{
		typ:       reportActionExec,
		startTime: actStart,
		duration:  dur.Seconds(),
		actor:     a.name,
		action:    action,
		success:   ps.Success(),
		output:    html.EscapeString(ps.String()),
	}
	return ev, nil
}
