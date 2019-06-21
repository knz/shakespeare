package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/log/logflags"
	"github.com/knz/shakespeare/pkg/crdb/stop"
	"github.com/knz/shakespeare/pkg/crdb/sysutil"
	"github.com/spf13/pflag"
	"golang.org/x/sys/unix"
)

// Run runs the program.
func Run() (err error) {
	ctx := context.Background()

	// Load the configuration.
	cfg := newConfig()
	if err = cfg.initArgs(ctx); err != nil {
		log.Errorf(ctx, "arg error: %+v", err)
		return errors.WithDetail(err, "(while parsing the command line)")
	}

	file := "-"
	if pflag.NArg() < 1 {
		if !cfg.quiet {
			fmt.Println("no configuration specified, reading from standard input...")
		}
	} else {
		file = pflag.Arg(0)
	}

	// Initialize the logging sub-system.
	if err = cfg.setupLogging(ctx); err != nil {
		log.Errorf(ctx, "init error: %+v", err)
		return errors.WithDetail(err, "(while initializing the logging subsystem)")
	}

	// Read the scenario.
	rd, err := newReader(ctx, file, cfg.includePath)
	if err != nil {
		log.Errorf(ctx, "open error: %+v", err)
		return errors.WithDetail(err, "(while opening the specification)")
	}
	defer rd.close()

	if err = cfg.parseCfg(ctx, rd); err != nil {
		log.Errorf(ctx, "parse error: %+v", err)
		return errors.WithDetail(err, "(while parsing the specification)")
	}

	if cfg.doPrint {
		cfg.printCfg(os.Stdout)
		fmt.Println()
	}

	// Generate the steps.
	if err = cfg.compile(); err != nil {
		log.Errorf(ctx, "compile error: %+v", err)
		return errors.WithDetail(err, "(while compiling the script)")
	}

	if cfg.doPrint {
		cfg.printSteps(os.Stdout)
	}

	if cfg.parseOnly {
		// No execution: stop before anything gets actually executed.
		return nil
	}

	// Create the app.
	ap := newApp(cfg)
	defer ap.close()
	ap.intro()

	defer func() {
		if err != nil {
			ap.narrate(E, "😱", "an error has occurred!")
		} else {
			ap.narrate(I, "😘", "good day! come again soon.")
		}
	}()

	// Run the script.
	err = ap.runConduct(ctx)
	if err != nil {
		log.Errorf(ctx, "play error: %+v", err)
		// We'll exit with the error later below.
		err = errors.WithDetail(err, "(while conducting the play)")
	}

	finalErr := ap.au.checkFinal(ctx)
	if finalErr != nil {
		log.Errorf(ctx, "audit error: %+v", finalErr)
		finalErr = errors.WithDetail(finalErr, "(while finalizing the audit)")
	}
	err = combineErrors(err, finalErr)

	if !errors.Is(err, errAuditViolation) {
		// This happens in the common case when a play is left to
		// terminate without early failure on audit errors: in that case,
		// the collector's context is canceled, the cancel error overtakes
		// the audit failure, and then dismissed (we're not reporting
		// context cancellation as a process failure).
		// In that case, we still want to verify whether there
		// are failures remaining.
		err = combineErrors(err, ap.checkAuditViolations())
	}

	// Generate the plots.
	plotErr := ap.plot(ctx)
	if plotErr != nil {
		log.Errorf(ctx, "plot error: %+v", plotErr)
		plotErr = errors.WithDetail(plotErr, "(while plotting the data)")
	}
	return combineErrors(err, plotErr)
}

func (ap *app) runConduct(bctx context.Context) error {
	// Set up the signal handlers.
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	infoCh := make(chan os.Signal, 1)
	signal.Notify(infoCh, syscall.SIGUSR1)

	// Set up a cancellable context for the entire start command.
	// The context will be canceled at the end.
	ctx, cancel := context.WithCancel(bctx)
	defer cancel()

	ap.stopper = stop.NewStopper()
	errChan := make(chan error, 1)
	playCtx := logtags.AddTag(ctx, "play", nil)
	runWorker(playCtx, ap.stopper, func(ctx context.Context) {
		defer func() {
			log.Flush()
			close(errChan)
		}()
		if err := ap.conduct(ctx); err != nil {
			errChan <- errors.WithContextTags(err, ctx)
		}
	})

	// Log shutdown activity in a different context.
	shutdownCtx := logtags.AddTag(ctx, "shutdown", nil)

	requestTermination := func() {
		// We start synchronizing log writes from here, because if a
		// signal was received there is a non-zero chance the sender of
		// this signal will follow up with SIGKILL if the shutdown is not
		// timely, and we don't want logs to be lost.
		log.SetSync(true)

		log.Info(shutdownCtx, showRunning(ap.stopper))

		// Start the draining process in a separate goroutine so that it
		// runs concurrently with the timeout check below.
		go func() {
			drainCtx := logtags.AddTag(ctx, "drain", nil)
			ap.stopper.Stop(drainCtx)
		}()
	}

	// returnErr will be populated with the error to use to exit the
	// process (reported to the shell).
	var returnErr error

	exit := false
	for !exit {
		// Block until one of the signals above is received or the stopper
		// is stopped externally (for example, via a debug action).
		select {
		case returnErr = <-errChan:
			requestTermination()
			exit = true

		case <-infoCh:
			log.Info(ctx, showRunning(ap.stopper))
			buf := make([]byte, 16384)
			s := runtime.Stack(buf, true)
			log.Infof(ctx, "go stacks:\n%s", string(buf[:s]))

		case <-ap.stopper.ShouldQuiesce():
			requestTermination()
			exit = true

		case sig := <-signalCh:
			log.Infof(shutdownCtx, "received signal '%s'", sig)
			if sig == os.Interrupt {
				// Graceful shutdown after an interrupt should cause the process
				// to terminate with a non-zero exit code; however SIGTERM is
				// "legitimate" and should be acknowledged with a success exit
				// code. So we keep the error state here for later.
				returnErr = errors.New("interrupted")
				msgDouble := "Note: a second interrupt will skip graceful shutdown and terminate forcefully"
				ap.narrate(I, "🛑", msgDouble)
			}

			requestTermination()
			exit = true

		case <-log.FatalChan():
			ap.stopper.Stop(shutdownCtx)
			// The logging goroutine is now responsible for killing this
			// process, so just block this goroutine.
			select {}
		}
	}

	// At this point, a signal has been received to shut down the
	// process, and a goroutine is busy telling the server to drain and
	// stop. From this point on, we just have to wait.

	const msgDrain = "the play is terminating"
	log.Info(shutdownCtx, msgDrain)
	ap.narrate(I, "👏", msgDrain)

	// Notify the user every 2 second of the shutdown progress.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				log.Info(shutdownCtx, showRunning(ap.stopper))
			case <-infoCh:
				log.Info(ctx, showRunning(ap.stopper))
				buf := make([]byte, 16384)
				s := runtime.Stack(buf, true)
				log.Infof(ctx, "go stacks:\n%s", string(buf[:s]))
			case <-ctx.Done():
				return
			case <-ap.stopper.IsStopped():
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
		const msgDone = "the stage has been cleared"
		log.Infof(shutdownCtx, msgDone)
		ap.narrate(I, "🧹", msgDone)
	}

	return returnErr
}

func (cfg *config) setupLogging(ctx context.Context) error {
	dirFlag := pflag.Lookup(logflags.LogDirName)
	if !log.DirSet() && !dirFlag.Changed {
		// If the log directory was not specified with -log-dir, override it.
		newDir := filepath.Join(cfg.dataDir, "logs")
		if err := dirFlag.Value.Set(newDir); err != nil {
			return err
		}
	}

	ls := pflag.Lookup(logflags.LogToStderrName)
	if logDir := dirFlag.Value.String(); logDir != "" {
		if !ls.Changed {
			// Unless the settings were overridden by the user, silence
			// logging to stderr because the messages will go to a log file.
			if err := ls.Value.Set(log.Severity_NONE.String()); err != nil {
				return err
			}
		}
		// Make sure the path exists.
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return errors.Wrap(err, "unable to create log directory")
		}
		log.Infof(ctx, "logging to directory %s", logDir)
		log.StartGCDaemon(ctx)
	}

	// if `--logtostderr` was not specified and no log directory was
	// set, or `--logtostderr` was specified but without explicit level,
	// then set stderr logging to INFO.
	if (!ls.Changed && !log.DirSet()) ||
		(ls.Changed && ls.Value.String() == log.Severity_DEFAULT.String()) {
		if err := ls.Value.Set(log.Severity_INFO.String()); err != nil {
			return err
		}
	}

	return nil
}
