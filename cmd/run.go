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

	"github.com/cockroachdb/errors"
	"github.com/knz/shakespeare/cmd/log"
	"github.com/knz/shakespeare/cmd/log/logflags"
	"github.com/knz/shakespeare/cmd/log/logtags"
	"github.com/knz/shakespeare/cmd/stop"
	"github.com/knz/shakespeare/cmd/sysutil"
	"github.com/spf13/pflag"
	"golang.org/x/sys/unix"
)

func main() {
	ctx := context.Background()

	// Load the configuration.
	cfg := newConfig()
	if err := cfg.initArgs(ctx); err != nil {
		log.Errorf(ctx, "arg error: %+v", err)
		os.Exit(1)
	}

	// Initialize the logging sub-system.
	if err := cfg.setupLogging(ctx); err != nil {
		log.Errorf(ctx, "init error: %+v", err)
		os.Exit(1)
	}

	// Read the scenario.
	rd := bufio.NewReader(os.Stdin)
	if err := cfg.parseCfg(rd); err != nil {
		log.Errorf(ctx, "parse error: %+v", err)
		os.Exit(1)
	}
	if cfg.doPrint {
		cfg.printCfg()
	}

	// Generate the steps.
	if err := cfg.compile(); err != nil {
		log.Errorf(ctx, "compile error: %+v", err)
		os.Exit(1)
	}
	if cfg.doPrint {
		cfg.printSteps()
	}

	if cfg.parseOnly {
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
			errChan <- err
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

		case <-ap.stopper.ShouldStop():
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
				fmt.Fprintln(os.Stdout, msgDouble)
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
				log.Info(shutdownCtx, showRunning(ap.stopper))
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

func (cfg *config) initArgs(ctx context.Context) error {
	cfg.shellPath = os.Getenv("SHELL")
	pflag.StringVar(&cfg.dataDir, "output-dir", ".", "output data directory")
	pflag.BoolVarP(&cfg.doPrint, "print-cfg", "p", false, "print out the parsed configuration")
	pflag.BoolVarP(&cfg.parseOnly, "dry-run", "n", false, "do not execute anything, just check the configuration")
	// pflag.BoolVarP(&cfg.quiet, "quiet", "q", false, "do not emit logs to stderr (equivalent to -logtostderr=NONE)")

	// Load the go flag settings from the log package into pflag.
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	// We'll revisit this value in setupLogging().
	pflag.Lookup(logflags.LogToStderrName).NoOptDefVal = log.Severity_DEFAULT.String()

	// Parse the command-line.
	pflag.Parse()

	// Derive the artifacts directory.
	cfg.artifactsDir = filepath.Join(cfg.dataDir, "artifacts")

	// Ensure the output directory and artifacts dir exist.
	if err := os.MkdirAll(cfg.artifactsDir, 0755); err != nil {
		return err
	}
	return nil
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
