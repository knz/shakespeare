package cmd

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/log/logtags"
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
		log.Info(ctx, "requesting the collector to exit")
		colDone()
		wgcol.Wait()
	}()

	// Start the spotlights.
	// The spotlights are running in the background until canceled
	// via allSpotsDone().
	var wgspot sync.WaitGroup
	allSpotsDone := ap.startSpotlights(ctx, &wgspot, monLogger, spotlightChan, errCh)
	defer func() {
		log.Info(ctx, "requesting spotlights to turn off")
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
			if err := ap.spotlight(ctx, a, monLogger, spotlightChan); err != nil && err != context.Canceled {
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
			if _, _, err := a.runActorCommand(ctx, ap.stopper, 10*time.Second, false /*interruptible*/, pCmd); err != nil {
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
