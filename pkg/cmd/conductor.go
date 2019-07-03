package cmd

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
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
			err = combineErrors(err, cleanupErr)
		}
	}()

	// Ensure the data gets to the log by the end of the play.
	defer func() { log.Flush() }()

	// Prepare the theater.
	th := ap.makeTheater(ctx)

	// Start the audition. This initializes the epoch, and thus needs to
	// happen before the collector and the auditors start.
	ap.openDoors(ctx)

	// Start the audition.
	var wgau sync.WaitGroup
	auDone := th.au.startAudition(ctx, &wgau)

	// Start the collector.
	var wgcol sync.WaitGroup
	colDone := th.col.startCollector(ctx, &wgcol)
	defer func() {
		if !errors.Is(err, errAuditViolation) {
			// This happens in the common case when a play is left to
			// terminate without early failure on audit errors: in that case,
			// the collector's context is canceled, the cancel error overtakes
			// the audit failure, and then dismissed (we're not reporting
			// context cancellation as a process failure).
			// In that case, we still want to verify whether there
			// are failures remaining.
			err = combineErrors(err, th.col.checkAuditViolations())
		}
	}()

	// Start the spotlights.
	var wgspot sync.WaitGroup
	allSpotsDone := th.spm.startSpotlights(ctx, &wgspot)

	// Start the prompter.
	var wgPrompt sync.WaitGroup
	promptDone := th.pr.startPrompter(ctx, &wgPrompt)

	// The shutdown sequence without cancellation/stopper is:
	// - prompter exits, this closes spotTermCh
	// - spotlights detect closed spotTermCh, terminate, then close auChan.
	// - auditors detect closed auChan, exit, this closes collectorChan.
	// - collectors detects closed collectorChan and exits.
	// However it's possible for things to terminate out of order:
	// - spotlights can detect a command error.
	// - auditors can encounter an audit failure.
	// - collector can encounter a file failure.
	// So at each of the shutdown stages below, we detect if a stage
	// later has completed and cancel the stages before.

	// TODO: this code can probably factored into a loop, not yet found
	// out how.

	var finalErr error
	var interrupt bool
	// First stage of shutdown: wait for the prompter to finish.
	select {
	case err := <-th.prErrCh:
		finalErr = combineErrors(err, finalErr)
		// ok
	case err := <-th.spotErrCh:
		finalErr = combineErrors(err, finalErr)
		interrupt = true
	case err := <-th.auErrCh:
		finalErr = combineErrors(err, finalErr)
		interrupt = true
	case err := <-th.colErrCh:
		finalErr = combineErrors(err, finalErr)
		interrupt = true
	}
	if interrupt {
		log.Info(ctx, "something went wrong other than prompter, cancelling everything")
		promptDone()
		finalErr = combineErrors(ignCancel(<-th.prErrCh), finalErr)
		allSpotsDone()
		finalErr = combineErrors(ignCancel(<-th.spotErrCh), finalErr)
		auDone()
		finalErr = combineErrors(ignCancel(<-th.auErrCh), finalErr)
		colDone()
		finalErr = combineErrors(ignCancel(<-th.colErrCh), finalErr)
		interrupt = false
	}
	wgPrompt.Wait()
	promptDone() // in case not called before.

	// Second stage: wait for the spotlights to finish.
	select {
	case err := <-th.spotErrCh:
		finalErr = combineErrors(err, finalErr)
		// ok
	case err := <-th.auErrCh:
		finalErr = combineErrors(err, finalErr)
		interrupt = true
	case err := <-th.colErrCh:
		finalErr = combineErrors(err, finalErr)
		interrupt = true
	}
	if interrupt {
		log.Info(ctx, "something went wrong after prompter terminated: cancelling spotlights, audience and collector")
		allSpotsDone()
		finalErr = combineErrors(ignCancel(<-th.spotErrCh), finalErr)
		auDone()
		finalErr = combineErrors(ignCancel(<-th.auErrCh), finalErr)
		colDone()
		finalErr = combineErrors(ignCancel(<-th.colErrCh), finalErr)
		interrupt = false
	}
	wgspot.Wait()
	allSpotsDone() // in case not called before.

	// Third stage: wait for the auditors to finish.
	select {
	case err := <-th.auErrCh:
		finalErr = combineErrors(err, finalErr)
		// ok
	case err := <-th.colErrCh:
		finalErr = combineErrors(err, finalErr)
		interrupt = true
	}
	if interrupt {
		log.Info(ctx, "something went wrong after spotlights terminated, cancelling audience and collector")
		auDone()
		finalErr = combineErrors(ignCancel(<-th.auErrCh), finalErr)
		colDone()
		finalErr = combineErrors(ignCancel(<-th.colErrCh), finalErr)
		interrupt = false
	}
	wgau.Wait()
	auDone() // in case not called before.

	// Fourth stage: wait for the collector to finish.
	finalErr = combineErrors(ignCancel(<-th.colErrCh), finalErr)
	wgcol.Wait()
	colDone() // in case not called before.

	return finalErr
}

type theater struct {
	auErrCh <-chan error
	au      audition

	prErrCh <-chan error
	pr      prompter

	colErrCh <-chan error
	col      collector

	spotErrCh <-chan error
	spm       spotMgr
}

func (ap *app) makeTheater(ctx context.Context) (th theater) {
	prompterAndAuditiontoCollectorCh := make(chan collectorEvent, 10)
	prompterAndSpotlightsToAuditionCh := make(chan auditableEvent, len(ap.cfg.actors))
	prompterToConductorErrCh := make(chan error, 1)
	prompterToSpotlightsTermCh := make(chan struct{})
	th.prErrCh = prompterToConductorErrCh
	th.pr = prompter{
		r:       ap,
		cfg:     ap.cfg,
		stopper: ap.stopper,
		collCh:  prompterAndAuditiontoCollectorCh,
		auditCh: prompterAndSpotlightsToAuditionCh,
		termCh:  prompterToSpotlightsTermCh,
		errCh:   prompterToConductorErrCh,
	}

	spotlightsToConductorErrCh := make(chan error, 1)
	th.spotErrCh = spotlightsToConductorErrCh
	th.spm = spotMgr{
		r:       ap,
		cfg:     ap.cfg,
		stopper: ap.stopper,
		logger:  log.NewSecondaryLogger(ctx, nil, "spotlight", true /*enableGc*/, false /*forceSyncWrite*/),
		auditCh: prompterAndSpotlightsToAuditionCh,
		termCh:  prompterToSpotlightsTermCh,
		errCh:   spotlightsToConductorErrCh,
	}

	auditionToConductorErrCh := make(chan error, 1)
	th.auErrCh = auditionToConductorErrCh
	th.au = audition{
		r:       ap,
		cfg:     ap.cfg,
		stopper: ap.stopper,

		logger: log.NewSecondaryLogger(ctx, nil, "audit", true /*enableGc*/, false /*forceSyncWrite*/),
		res:    &ap.auRes,
		st:     makeAuditionState(ap.cfg),

		eventCh: prompterAndSpotlightsToAuditionCh,
		collCh:  prompterAndAuditiontoCollectorCh,
		errCh:   auditionToConductorErrCh,
	}

	collectorToConductorErrCh := make(chan error, 1)
	th.colErrCh = collectorToConductorErrCh
	th.col = collector{
		r:       ap,
		cfg:     ap.cfg,
		stopper: ap.stopper,
		logger:  log.NewSecondaryLogger(ctx, nil, "collector", true /*enableGc*/, false /*forceSyncWrite*/),
		eventCh: prompterAndAuditiontoCollectorCh,
		errCh:   collectorToConductorErrCh,
	}

	return th
}

// startPrompter runs the prompter until completion.
func (pr *prompter) startPrompter(ctx context.Context, wg *sync.WaitGroup) func() {
	promptCtx, promptDone := context.WithCancel(ctx)
	promptCtx = logtags.AddTag(promptCtx, "prompter", nil)
	wg.Add(1)
	runWorker(promptCtx, pr.stopper, func(ctx context.Context) {
		defer func() {
			// Inform the spotlights to terminate.
			close(pr.termCh)
			// Indicate to the conductor that we are terminating.
			wg.Done()
			// Also indicate to the conductor there will be no further error
			// reported.
			close(pr.errCh)
			log.Info(ctx, "<exit>")
		}()
		log.Info(ctx, "<intrat>")
		pr.errCh <- errors.WithContextTags(pr.prompt(ctx), ctx)
	})
	return promptDone
}

// startAudition starts the audition in the background.
func (au *audition) startAudition(ctx context.Context, wg *sync.WaitGroup) (cancelFunc func()) {
	auCtx, auDone := context.WithCancel(ctx)
	auCtx = logtags.AddTag(auCtx, "audition", nil)
	wg.Add(1)
	runWorker(auCtx, au.stopper, func(ctx context.Context) {
		defer func() {
			// Indicate to the conductor that we are terminating.
			wg.Done()
			// Also indicate to the conductor there will be no further error
			// reported.
			close(au.errCh)
			log.Info(ctx, "<ends>")
		}()
		log.Info(ctx, "<begins>")
		au.errCh <- errors.WithContextTags(au.audit(ctx), ctx)
	})
	return auDone
}

// startCollector starts the collector in the background.
func (col *collector) startCollector(ctx context.Context, wg *sync.WaitGroup) (cancelFunc func()) {
	colCtx, colDone := context.WithCancel(ctx)
	colCtx = logtags.AddTag(colCtx, "collector", nil)
	wg.Add(1)
	runWorker(colCtx, col.stopper, func(ctx context.Context) {
		defer func() {
			// Indicate to the conductor that we are terminating.
			wg.Done()
			// Also indicate to the conductor there will be no further error
			// reported.
			close(col.errCh)
			log.Info(ctx, "<exit>")
		}()
		log.Info(ctx, "<intrat>")
		col.errCh <- errors.WithContextTags(col.collect(ctx), ctx)
	})
	return colDone
}

// startSpotlights starts all the spotlights in the background.
func (spm *spotMgr) startSpotlights(ctx context.Context, wg *sync.WaitGroup) (cancelFunc func()) {
	spotCtx, spotDone := context.WithCancel(ctx)
	spotCtx = logtags.AddTag(spotCtx, "spotlight-supervisor", nil)
	wg.Add(1)
	runWorker(spotCtx, spm.stopper, func(ctx context.Context) {
		defer func() {
			// Inform the audience to terminate.
			close(spm.auditCh)
			// Indicate to the conductor that we are terminating.
			wg.Done()
			// Also indicate to the conductor there will be no further error
			// reported.
			close(spm.errCh)
			log.Info(ctx, "<exit>")
		}()
		log.Info(ctx, "<intrat>")
		spm.errCh <- errors.WithContextTags(spm.manageSpotlights(ctx), ctx)
	})
	return spotDone
}

func (ap *app) runCleanup(ctx context.Context) error {
	return ap.runForAllActors(ctx, "cleanup", func(a *actor) cmd { return a.role.cleanupCmd })
}

func (ap *app) runForAllActors(
	ctx context.Context, prefix string, getCommand func(a *actor) cmd,
) (err error) {
	// errCh collects the errors from the concurrent actors.
	errCh := make(chan error, len(ap.cfg.actors)+1)

	defer func() {
		if r := recover(); r != nil {
			panic(r)
		}
		// At the end of the scene, make runScene() return the collected
		// errors.
		err = collectErrors(ctx, nil, errCh, prefix)
	}()

	var wg sync.WaitGroup
	defer func() { wg.Wait() }()

	actNums := 0
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
			defer func() {
				wg.Done()
				log.Info(ctx, "<done>")
			}()
			// Start one actor.
			log.Info(ctx, "<start>")
			_, _, err, _ := a.runActorCommand(ctx, ap.stopper, 10*time.Second, false /*interruptible*/, pCmd)
			errCh <- errors.WithContextTags(err, ctx)
		})
	}
	if actNums == 0 {
		// Nothing was launched, ensure that collectErrors terminates in any case.
		errCh <- nil
	}

	// errors are collected by the defer above.
	return nil
}

func collectErrors(
	ctx context.Context, closers []func(), errCh chan error, prefix string,
) (finalErr error) {
	// Wait on the first error return.
	select {
	case err := <-errCh:
		if err != nil {
			log.Errorf(ctx, "complaint during %s: %+v", prefix, err)
			finalErr = combineErrors(finalErr, err)
		}
	}
	// Signal all to terminate and wait for each of them.
	for _, closer := range closers {
		closer()
	}
	// At this point all have terminate. Ensure the loop below
	// terminates in all cases.
	close(errCh)

	for stErr := range errCh {
		if stErr == nil {
			continue
		}
		log.Errorf(ctx, "complaint during %s: %+v", prefix, stErr)
		finalErr = combineErrors(finalErr, stErr)
	}

	return errors.WithContextTags(errors.Wrap(finalErr, prefix), ctx)
}

type terminate struct{}

func ignCancel(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
