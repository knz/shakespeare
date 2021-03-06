package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
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
		// log.Errorf(ctx, "arg error: %+v", err)
		return errors.WithDetail(err, "(while parsing the command line)")
	}

	// Read the configuration(s).
	files := []string{"-"}
	if pflag.NArg() < 1 {
		if !cfg.quiet {
			fmt.Println("no configuration specified, reading from standard input...")
		}
	} else {
		files = make([]string, pflag.NArg())
		for i := 0; i < pflag.NArg(); i++ {
			files[i] = pflag.Arg(i)
		}
	}

	// diffs contain the "git diff" of input configuration files.
	collectDiffs := func(newDiffs map[string]string) {
		if cfg.diffs == nil {
			cfg.diffs = newDiffs
		} else {
			for k, v := range newDiffs {
				cfg.diffs[k] = v
			}
		}
	}

	for _, file := range files {
		if err := func() error {
			// We use a closure to close the reader directly after each file.
			rd, err := newReader(ctx, file, cfg.includePath)
			if err != nil {
				// log.Errorf(ctx, "open error: %+v", err)
				return errors.Wrapf(err, "reading configuration from %s", file)
			}
			defer rd.close()

			err = cfg.parseCfg(ctx, rd)
			collectDiffs(rd.diffs)
			return err
		}(); err != nil {
			return err
		}
	}

	// Process additional configuration passed via -s.
	if len(cfg.extraScript) > 0 {
		var extraScript bytes.Buffer
		fmt.Fprintln(&extraScript, "script")
		for _, s := range cfg.extraScript {
			fmt.Fprintln(&extraScript, s)
		}
		fmt.Fprintln(&extraScript, "end")
		if err := func() error {
			rd, _ := newReaderFromString("<command line>", extraScript.String())
			defer rd.close()
			err := cfg.parseCfg(ctx, rd)
			collectDiffs(rd.diffs)
			return err
		}(); err != nil {
			return err
		}
	}

	// Process additional configuration passed via -r.
	if len(cfg.extraInterpretation) > 0 {
		var extraInterpretation bytes.Buffer
		fmt.Fprintln(&extraInterpretation, "interpretation")
		for _, s := range cfg.extraInterpretation {
			fmt.Fprintln(&extraInterpretation, s)
		}
		fmt.Fprintln(&extraInterpretation, "end")
		rd, _ := newReaderFromString("<command line>", extraInterpretation.String())
		if err = cfg.parseCfg(ctx, rd); err != nil {
			log.Errorf(ctx, "parse error: %+v", err)
			return err
		}
	}

	if cfg.doPrint {
		cfg.printCfg(os.Stdout, false /*skipComs*/, false /*skipVer*/, false /*annot*/)
		fmt.Println()
	}

	// Generate the steps.
	if err = cfg.compileV2(); err != nil {
		log.Errorf(ctx, "compile error: %+v", err)
		return err
	}

	if cfg.doPrint {
		cfg.printSteps(os.Stdout, false /*annot*/)
	}

	if cfg.parseOnly {
		// No execution: stop before anything gets actually executed.
		return nil
	}

	return cfg.run(ctx)
}

func (cfg *config) run(ctx context.Context) (err error) {
	// Prepare the directories. This is needed before logging starts.
	if err := cfg.prepareDirs(ctx); err != nil {
		return errors.WithDetail(err, "(while setting up directories)")
	}

	// Initialize the logging sub-system.
	if err = cfg.setupLogging(ctx); err != nil {
		return errors.WithDetail(err, "(while initializing the logging subsystem)")
	}

	// After this point, if a panic occurs on the main thread it will be hidden
	// when logging to file is enabled and no log messages are configured
	// to appear on stderr.
	// Ensure we reveal the panic on stderr too.
	// (This is not needed for workers -- the stopper does this reporting already)
	defer func() {
		if r := recover(); r != nil {
			log.ReportPanic(ctx, r)
			panic(r)
		}
	}()

	// Create the app.
	ap := newApp(ctx, cfg)
	defer ap.close()
	ap.intro()

	defer func() {
		if err != nil {
			ap.narrate(E, "😱", "an error has occurred!")
		} else {
			if cfg.removeAll {
				ap.narrate(I, "🧹", "removing data directory: %s", cfg.dataDir)
				err = os.RemoveAll(cfg.dataDir)
			}
			ap.narrate(I, "😘", "good day! come again soon.")
		}
	}()

	defer func() {
		// Upload produced files if completing without being interrupted
		// by a signal (e.g. Ctrl+C) and there was no panic.
		if err == nil || !isError(err, errInterrupted) {
			// Ensure logs are flushed prior to uploading.
			log.Flush()

			// Remove stray files and non-uploadable files.
			ap.removeNonUploadableFiles()

			uploadErr := ap.tryUpload(ctx)
			err = combineErrors(err, uploadErr)
		}
	}()

	// Run the script.
	err = ap.runConduct(ctx)
	if err != nil {
		log.Errorf(ctx, "play error: %+v", err)
		// We'll exit with the error later below.
	}

	result := ap.assemble(ctx, err)

	defer func() {
		// No error - remove artifacts unless -k was specified.
		if err == nil && !cfg.removeAll && !cfg.keepArtifacts {
			ap.narrate(I, "🧹", "no foul, removing artifacts: %s", cfg.artifactsDir())
			err = os.RemoveAll(cfg.artifactsDir())
		}

		// Collect the tree of result files.
		ap.collectArtifacts(result)
		if len(result.Artifacts) > 0 {
			ap.narrate(I, "📁", "result files in %s", ap.cfg.dataDir)
			ap.showArtifacts(result.Artifacts)
		}

		// Write result.js.
		if errR := ap.writeResult(ctx, result); errR != nil {
			log.Errorf(ctx, "error writing result file: %+v", errR)
			err = combineErrors(err, errR)
		}

		// Write index.html.
		if errH := ap.writeHtml(ctx); errH != nil {
			log.Errorf(ctx, "error writing index file: %+v", errH)
			err = combineErrors(err, errH)
		}
	}()

	if !cfg.skipPlot {
		// Generate the plots.
		plotErr := ap.plot(ctx, result)
		if plotErr != nil {
			log.Errorf(ctx, "plot error: %+v", plotErr)
		}
		err = combineErrors(err, plotErr)
	}

	return err
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
		errChan <- errors.WithContextTags(ap.conduct(ctx), ctx)
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
		case thisErr := <-errChan:
			returnErr = combineErrors(returnErr, thisErr)
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
				returnErr = combineErrors(returnErr, errInterrupted)
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

	return combineErrors(returnErr, <-errChan)
}

var errInterrupted = errors.New("interrupted")

func (cfg *config) getLogDir() string {
	dirFlag := pflag.Lookup(logflags.LogDirName)
	return dirFlag.Value.String()
}

func (cfg *config) setupLogging(ctx context.Context) error {
	if cfg.skipLoggingInit {
		// Skip log init in tests.
		return nil
	}

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
		// Make sure the path exists.
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return errors.Wrap(err, "unable to create log directory")
		}

		if !ls.Changed {
			// Unless the settings were overridden by the user, silence
			// logging to stderr because the messages will go to a log file.
			if err := ls.Value.Set(log.Severity_NONE.String()); err != nil {
				return err
			}
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

func (ap *app) removeNonUploadableFiles() {
	_ = filepath.Walk(ap.cfg.dataDir,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}

			if !info.Mode().IsRegular() && (info.Mode()&os.ModeType != os.ModeSymlink) {
				ap.narrate(I, "", "removing non-uploadable file: %s", path)
				_ = os.Remove(path)
				return nil
			}

			name := filepath.Base(path)
			if (strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#")) || strings.HasSuffix(name, "~") {
				// Editor temp files. Remove.
				_ = os.Remove(path)
				return nil
			}

			return nil
		})
}
