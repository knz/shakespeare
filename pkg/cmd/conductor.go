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

	// We'll log all the monitored and extracted data to a secondary logger.
	auLogger := log.NewSecondaryLogger(ctx, nil, "audit", true /*enableGc*/, false /*forceSyncWrite*/)
	monLogger := log.NewSecondaryLogger(ctx, nil, "spotlight", true /*enableGc*/, false /*forceSyncWrite*/)
	dataLogger := log.NewSecondaryLogger(ctx, nil, "collector", true /*enableGc*/, false /*forceSyncWrite*/)
	defer func() { log.Flush() }()

	// Start the audition. This initializes the epoch, and thus needs to
	// happen before the collector and the auditors start.
	ap.au.start(ctx)

	// errCh collects errors.
	errCh := make(chan error, len(ap.cfg.actors)+2 /* +2 for audition, collector */)
	// closers contains the functions that terminate the workers.
	var closers []func()
	defer func() {
		if r := recover(); r != nil {
			panic(r)
		}
		err = collectErrors(ctx, closers, errCh, "play")
	}()

	collectorChan := make(chan observation, len(ap.cfg.actors))
	auChan := make(chan auditableEvent, len(ap.cfg.actors))
	actionChan := make(chan actionReport, len(ap.cfg.actors))
	moodCh := make(chan moodChange, 1)

	// Start the audition.
	var wgau sync.WaitGroup
	auDone := ap.startAudition(ctx, &wgau, auLogger, auChan, actionChan, collectorChan, moodCh, errCh)
	closers = append(closers, func() {
		log.Info(ctx, "requesting the audition to stop")
		auDone()
		wgau.Wait()
	})

	// Start the collector.
	var wgcol sync.WaitGroup
	colDone := ap.startCollector(ctx, &wgcol, dataLogger, actionChan, collectorChan, errCh)
	closers = append(closers, func() {
		log.Info(ctx, "requesting the collector to exit")
		colDone()
		wgcol.Wait()
	})

	// Start the spotlights.
	var wgspot sync.WaitGroup
	allSpotsDone := ap.startSpotlights(ctx, &wgspot, monLogger, auChan, errCh)
	closers = append(closers, func() {
		log.Info(ctx, "requesting spotlights to turn off")
		allSpotsDone()
		wgspot.Wait()
	})

	// Start the prompter.
	var wgPrompt sync.WaitGroup
	promptDone := ap.runPrompter(ctx, &wgPrompt, actionChan, moodCh, errCh)
	closers = append(closers, func() {
		log.Info(ctx, "requesting the prompter to exit")
		promptDone()
		wgPrompt.Wait()
	})

	// errors are collected by the defer above.
	return nil
}

// runPrompter runs the prompter until completion.
func (ap *app) runPrompter(
	ctx context.Context,
	wg *sync.WaitGroup,
	actionChan chan<- actionReport,
	moodCh chan<- moodChange,
	errCh chan<- error,
) func() {
	promptCtx, promptDone := context.WithCancel(ctx)
	promptCtx = logtags.AddTag(promptCtx, "prompter", nil)
	wg.Add(1)
	runWorker(promptCtx, ap.stopper, func(ctx context.Context) {
		defer wg.Done()
		log.Info(ctx, "<intrat>")
		err := errors.WithContextTags(ap.prompt(ctx, actionChan, moodCh), ctx)
		if errors.Is(err, context.Canceled) {
			// It's ok if the prompt is canceled.
			err = nil
		}
		errCh <- err
		log.Info(ctx, "<exit>")
	})
	return promptDone
}

// startAudition starts the audition in the background.
func (ap *app) startAudition(
	ctx context.Context,
	wg *sync.WaitGroup,
	auLogger *log.SecondaryLogger,
	auChan <-chan auditableEvent,
	actionCh chan<- actionReport,
	collectorCh chan<- observation,
	moodCh <-chan moodChange,
	errCh chan<- error,
) (cancelFunc func()) {
	auCtx, auDone := context.WithCancel(ctx)
	auCtx = logtags.AddTag(auCtx, "audition", nil)
	wg.Add(1)
	runWorker(auCtx, ap.stopper, func(ctx context.Context) {
		defer wg.Done()
		log.Info(ctx, "<begins>")
		err := errors.WithContextTags(ap.audit(ctx, auLogger, auChan, actionCh, collectorCh, moodCh), ctx)
		if errors.Is(err, context.Canceled) {
			// It's ok if the audition is canceled.
			err = nil
		}
		errCh <- err
		log.Info(ctx, "<ends>")
	})
	return auDone
}

// startCollector starts the collector in the background.
func (ap *app) startCollector(
	ctx context.Context,
	wg *sync.WaitGroup,
	dataLogger *log.SecondaryLogger,
	actionChan <-chan actionReport,
	collectorChan <-chan observation,
	errCh chan<- error,
) (cancelFunc func()) {
	colCtx, colDone := context.WithCancel(ctx)
	colCtx = logtags.AddTag(colCtx, "collector", nil)
	wg.Add(1)
	runWorker(colCtx, ap.stopper, func(ctx context.Context) {
		defer wg.Done()
		log.Info(ctx, "<intrat>")
		err := errors.WithContextTags(ap.collect(ctx, dataLogger, actionChan, collectorChan), ctx)
		if errors.Is(err, context.Canceled) {
			// It's ok if the collector is canceled.
			err = nil
		}
		errCh <- err
		log.Info(ctx, "<exit>")
	})
	return colDone
}

// startSpotlights starts all the spotlights in the background.
func (ap *app) startSpotlights(
	ctx context.Context,
	wg *sync.WaitGroup,
	monLogger *log.SecondaryLogger,
	auChan chan<- auditableEvent,
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
			err := errors.WithContextTags(ap.spotlight(ctx, a, monLogger, auChan), ctx)
			if errors.Is(err, context.Canceled) {
				// It's ok if a sportlight is canceled.
				err = nil
			}
			errCh <- err
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
			defer wg.Done()
			// Start one actor.
			log.Info(ctx, "<start>")
			_, _, err, _ := a.runActorCommand(ctx, ap.stopper, 10*time.Second, false /*interruptible*/, pCmd)
			errCh <- errors.WithContextTags(err, ctx)
			log.Info(ctx, "<done>")
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
) (err error) {
	// Wait on at least one error return.
	select {
	case err = <-errCh:
	}
	coll := &errorCollection{}
	if err != nil {
		log.Errorf(ctx, "complaint during %s: %+v", prefix, err)
		coll.errs = append(coll.errs, err)
	}
	// Signal all to terminate.
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
		coll.errs = append(coll.errs, stErr)
	}

	if len(coll.errs) > 0 {
		if len(coll.errs) == 1 {
			err = coll.errs[0]
		} else {
			err = coll
		}
		return errors.WithContextTags(errors.Wrap(err, prefix), ctx)
	}
	return nil
}
