package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/knz/shakespeare/cmd/log"
	"github.com/knz/shakespeare/cmd/log/logflags"
	"github.com/knz/shakespeare/cmd/log/logtags"
	"github.com/knz/shakespeare/cmd/stop"
	"github.com/knz/shakespeare/cmd/sysutil"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

var shellPath = os.Getenv("SHELL")

var dataDir = flag.String("output-dir", ".", "output data directory")

var doPrint = flag.Bool("print-cfg", false, "print out the parsed configuration")

var parseOnly = flag.Bool("n", false, "do not execute anything, just check the configuration")

var quiet = flag.Bool("q", false, "do not emit logs to stderr (equivalent to -logtostderr=NONE)")

var artifactsDir string

func main() {
	flag.Parse()
	artifactsDir = filepath.Join(*dataDir, "artifacts")
	if *quiet {
		// Disable logging to the screen.
		flag.Lookup(logflags.LogToStderrName).Value.Set("NONE")
	}

	ctx := context.Background()

	// Instantiate the app.
	cfg := newConfig()

	// Read the configuration.
	rd := bufio.NewReader(os.Stdin)
	if err := cfg.parseCfg(rd); err != nil {
		log.Errorf(ctx, "parse error: %+v", err)
		os.Exit(1)
	}
	if *doPrint {
		cfg.printCfg()
	}

	// Generate the steps.
	if err := cfg.compile(); err != nil {
		log.Errorf(ctx, "compile error: %+v", err)
		os.Exit(1)
	}
	if *doPrint {
		cfg.printSteps()
	}

	if *parseOnly {
		// No execution: stop before anything gets actually executed.
		return
	}

	// Create the app.
	ap := newApp(cfg)

	// Run the script.
	if err := ap.runConduct(ctx); err != nil {
		log.Errorf(ctx, "play error: %+v", err)
		os.Exit(1)
	}

	if err := ap.au.checkFinal(ctx); err != nil {
		log.Errorf(ctx, "audit error: %+v", err)
		os.Exit(1)
	}

	// Generate the plots.
	if err := ap.plot(ctx); err != nil {
		log.Errorf(ctx, "plot error: %+v", err)
		os.Exit(1)
	}
}

func (ap *app) runConduct(bctx context.Context) error {
	// Set up the signal handlers.
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)

	// Set up a cancellable context for the entire start command.
	// The context will be canceled at the end.
	ctx, cancel := context.WithCancel(bctx)
	defer cancel()

	ap.stopper = stop.NewStopper()
	errChan := make(chan error, 1)
	go func() {
		defer log.Flush()
		if err := ap.conduct(ctx); err != nil {
			errChan <- err
		}
		ap.stopper.Stop(ctx)
		<-ap.stopper.IsStopped()
	}()

	// Log shutdown activity in a different context.
	shutdownCtx := logtags.AddTag(ctx, "shutdown", nil)

	// returnErr will be populated with the error to use to exit the
	// process (reported to the shell).
	var returnErr error

	// Block until one of the signals above is received or the stopper
	// is stopped externally (for example, via a debug action).
	select {
	case err := <-errChan:
		// SetSync both flushes and ensures that subsequent log writes are flushed too.
		log.SetSync(true)
		return err

	case <-ap.stopper.ShouldStop():
		// Server is being stopped externally and our job is finished
		// here since we don't know if it's a graceful shutdown or not.
		<-ap.stopper.IsStopped()
		// SetSync both flushes and ensures that subsequent log writes are flushed too.
		log.SetSync(true)
		return nil

	case sig := <-signalCh:
		// We start synchronizing log writes from here, because if a
		// signal was received there is a non-zero chance the sender of
		// this signal will follow up with SIGKILL if the shutdown is not
		// timely, and we don't want logs to be lost.
		log.SetSync(true)

		log.Infof(shutdownCtx, "received signal '%s'", sig)
		if sig == os.Interrupt {
			// Graceful shutdown after an interrupt should cause the process
			// to terminate with a non-zero exit code; however SIGTERM is
			// "legitimate" and should be acknowledged with a success exit
			// code. So we keep the error state here for later.
			returnErr = errors.New("interrupted")
			msgDouble := "Note: a second interrupt will skip graceful shutdown and terminate forcefully"
			fmt.Fprintln(os.Stdout, msgDouble)
		}

		// Start the draining process in a separate goroutine so that it
		// runs concurrently with the timeout check below.
		go func() {
			drainCtx := logtags.AddTag(ctx, "drain", nil)
			ap.stopper.Stop(drainCtx)
		}()

		// Don't return: we're shutting down gracefully.

	case <-log.FatalChan():
		ap.stopper.Stop(shutdownCtx)
		// The logging goroutine is now responsible for killing this
		// process, so just block this goroutine.
		select {}
	}

	// At this point, a signal has been received to shut down the
	// process, and a goroutine is busy telling the server to drain and
	// stop. From this point on, we just have to wait.

	const msgDrain = "initiating graceful shutdown"
	log.Info(shutdownCtx, msgDrain)
	fmt.Fprintln(os.Stdout, msgDrain)

	// Notify the user every 5 second of the shutdown progress.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				log.Infof(context.Background(), "%d running tasks", ap.stopper.NumTasks())
			case <-ap.stopper.ShouldStop():
				return
			}
		}
	}()

	// Meanwhile, we don't want to wait too long either, in case the
	// process is getting stuck and doesn't shut down in a timely manner.
	//
	select {
	case sig := <-signalCh:
		// This new signal is not welcome, as it interferes with the graceful
		// shutdown process.
		log.Shout(shutdownCtx, log.Severity_ERROR, fmt.Sprintf(
			"received signal '%s' during shutdown, initiating hard shutdown", sig))
		// Reraise the signal. os.Signal is always sysutil.Signal.
		signal.Reset(sig)
		if err := unix.Kill(unix.Getpid(), sig.(sysutil.Signal)); err != nil {
			log.Fatalf(context.Background(), "unable to forward signal %v: %v", sig, err)
		}
		// Block while we wait for the signal to be delivered.
		select {}

	case <-time.After(time.Minute):
		return errors.Errorf("time limit reached, initiating hard shutdown")

	case <-ap.stopper.IsStopped():
		const msgDone = "process drained and shutdown completed"
		log.Infof(shutdownCtx, msgDone)
		fmt.Fprintln(os.Stdout, msgDone)
	}

	return returnErr
}
