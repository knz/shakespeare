package cmd

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/stop"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

type prompter struct {
	r       reporter
	cfg     *config
	stopper *stop.Stopper

	// Where the prompter should report performed actions.
	actionCh chan<- actionReport

	// Where the prompter should report act changes.
	actChangeCh chan<- actChange

	// Where the prompter should report mood changes.
	moodChangeCh chan<- moodChange

	// This channels is closed when the prompter reaches the end of
	// the script, it should instruct the spotlight to terminate.
	termCh chan<- struct{}

	// Where the prompter should return errors. The prompter
	// also closes this when it terminates.
	errCh chan<- error
}

// prompt directs the actors to perform actions in accordance with the script.
func (pr *prompter) prompt(ctx context.Context) error {
	// surpriseDur is the duration of a scene beyond which the prompter
	// will express surprise.
	surpriseDur := 2 * pr.cfg.tempo.Seconds()

	// Play.
	for j := 0; j < len(pr.cfg.play); j++ {
		act := pr.cfg.play[j]
		actNum := j + 1
		actCtx := logtags.AddTag(ctx, "act", actNum)
		actStart := timeutil.Now()
		if err := pr.signalActChange(actCtx, actStart, actNum); err != nil {
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
			case <-pr.stopper.ShouldQuiesce():
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

			sceneTime := timeutil.Now()

			extraMsg := ""
			if !pr.cfg.avoidTimeProgress {
				var extra bytes.Buffer
				elapsedTotal := sceneTime.Sub(pr.r.epoch())
				elapsedAct := sceneTime.Sub(actStart)
				fmt.Fprintf(&extra, " (~%ds total, ~%ds in act", int(elapsedTotal.Seconds()), int(elapsedAct.Seconds()))
				if toWait < -time.Duration(1)*time.Millisecond {
					fmt.Fprintf(&extra, ", running %.4fs behind schedule", -toWait.Seconds())
				}
				extra.WriteByte(')')
				extraMsg = extra.String()
			}

			pr.r.narrate(I, "", "act %d, scene %d%s:", actNum, sceneNum, extraMsg)

			// Now run the scene.
			err := pr.runScene(sceneCtx, scene.concurrentLines)

			// In any case, make a statement about the duration.
			if pr.cfg.tempo != 0 && !pr.cfg.avoidTimeProgress {
				sceneDur := timeutil.Now().Sub(sceneTime).Seconds()
				if sceneDur >= surpriseDur {
					pr.r.narrate(W, "😮", "woah! scene %d lasted %.1fx longer than expected!",
						sceneNum, sceneDur/pr.cfg.tempo.Seconds())
				}
			}

			// If there was an error, return it.
			if err != nil {
				return errors.WithContextTags(err, ctx)
			}
		}

		// End of act. Are we looping?
		if pr.cfg.repeatActNum > 0 && j == len(pr.cfg.play)-1 {
			pr.r.narrate(I, "🔁", "restarting from act %d", pr.cfg.repeatActNum)
			// "num" is 1-indexed, but j is 0-indexed. shift.
			repeatIdx := pr.cfg.repeatActNum - 1
			// the loop iteration will increment j before
			// the next iteration, so start one lower.
			j = repeatIdx - 1
		}
	}

	return nil
}

// runScene runs one scene.
// A scene is the concurrent execution of each actor's lines.
func (pr *prompter) runScene(ctx context.Context, lines []scriptLine) (err error) {
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
			if err := pr.runMoodChange(ctx, line.steps[0].action); err != nil {
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
		if err := runAsyncTask(lineCtx, pr.stopper, func(ctx context.Context) {
			defer wg.Done()
			err := pr.runLine(ctx, a, steps)
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
func (pr *prompter) runLine(ctx context.Context, a *actor, steps []step) error {
	for stepNum, step := range steps {
		stepCtx := logtags.AddTag(ctx, "step", stepNum+1)

		switch step.typ {
		case stepAmbiance:
			if err := pr.runMoodChange(stepCtx, step.action); err != nil {
				return err
			}

		case stepDo:
			qc := '!'
			if step.failOk {
				qc = '?'
			}
			pr.r.narrate(I, "🥁", "    %s: %s%c", a.name, step.action, qc)
			ev, err := a.runAction(stepCtx, pr, step.action)
			if err != nil {
				return err
			}
			ev.failOk = step.failOk
			reportErr := pr.reportActionEvent(stepCtx, ev)
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

func (pr *prompter) signalActChange(ctx context.Context, actStart time.Time, actNum int) error {
	pr.r.narrate(I, "🎬", "act %d starts", actNum)
	elapsed := actStart.Sub(pr.r.epoch()).Seconds()
	ev := actChange{ts: elapsed, actNum: actNum}
	select {
	case <-pr.stopper.ShouldQuiesce():
		log.Info(ctx, "terminated")
		return nil
	case <-ctx.Done():
		log.Info(ctx, "interrupted")
		return wrapCtxErr(ctx)
	case pr.actChangeCh <- ev:
		// ok
	}
	return nil
}

func (pr *prompter) runMoodChange(ctx context.Context, newMood string) error {
	pr.r.narrate(I, "🎊", "    (mood %s)", newMood)
	now := timeutil.Now()
	elapsed := now.Sub(pr.r.epoch()).Seconds()
	if err := pr.reportMoodEvent(ctx, moodChange{ts: elapsed, newMood: newMood}); err != nil {
		return err
	}
	ev := actionReport{
		typ:       reportMoodChange,
		startTime: elapsed,
		output:    newMood,
	}
	return pr.reportActionEvent(ctx, ev)
}

func (pr *prompter) reportActionEvent(ctx context.Context, ev actionReport) error {
	select {
	case <-pr.stopper.ShouldQuiesce():
		log.Info(ctx, "terminated")
		return nil
	case <-ctx.Done():
		log.Info(ctx, "interrupted")
		return wrapCtxErr(ctx)
	case pr.actionCh <- ev:
		// ok
	}
	return nil
}

func (pr *prompter) reportMoodEvent(ctx context.Context, chg moodChange) error {
	select {
	case <-pr.stopper.ShouldQuiesce():
		log.Info(ctx, "terminated")
		return nil
	case <-ctx.Done():
		log.Info(ctx, "interrupted")
		return wrapCtxErr(ctx)
	case pr.moodChangeCh <- chg:
		// ok
	}
	return nil
}

func (a *actor) runAction(ctx context.Context, pr *prompter, action string) (actionReport, error) {
	aCmd, ok := a.role.actionCmds[action]
	if !ok {
		return actionReport{}, errors.Errorf("unknown action: %q", action)
	}
	ctx = logtags.AddTag(ctx, "action", action)

	actStart := timeutil.Now()
	outdata, ps, err, exitErr := a.runActorCommand(ctx, pr.stopper, 0 /*timeout*/, true /*interruptible*/, aCmd)
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
		startTime: actStart.Sub(pr.r.epoch()).Seconds(),
		duration:  dur.Seconds(),
		actor:     a.name,
		action:    action,
		result:    result,
		output:    html.EscapeString(combinedErrOutput),
		extOutput: strings.TrimSpace(outdata),
	}
	return ev, nil
}
