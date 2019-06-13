package main

import (
	"bufio"
	"context"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/knz/shakespeare/cmd/log"
	"github.com/knz/shakespeare/cmd/log/logflags"
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

	bctx := context.Background()

	// Read the configuration.
	rd := bufio.NewReader(os.Stdin)
	if err := parseCfg(rd); err != nil {
		log.Errorf(bctx, "parse error: %+v", err)
		os.Exit(1)
	}
	if *doPrint {
		printCfg()
	}

	// Generate the steps.
	if err := compile(); err != nil {
		log.Errorf(bctx, "compile error: %+v", err)
		os.Exit(1)
	}
	if *doPrint {
		printSteps()
	}

	if *parseOnly {
		// No execution: stop before anything gets actually executed.
		return
	}

	// All the asynchronous activities will check their provided
	// context for asynchronous termination.
	ctx, cancel := context.WithCancel(bctx)

	// Upon receiving a signal, terminate all the tasks.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		_ = <-sigCh
		cancel()
	}()

	defer func() {
		// Upon the end of execution, terminate all the tasks.
		// Also terminate the signal checker.
		close(sigCh)
		cancel()
	}()

	// Create the audition.
	au := newAudition()

	// Run the script.
	if err := conduct(ctx, au); err != nil {
		log.Errorf(ctx, "play error: %+v", err)
		os.Exit(1)
	}

	if err := au.checkFinal(ctx); err != nil {
		log.Errorf(ctx, "audit error: %+v", err)
		os.Exit(1)
	}

	// Generate the plots.
	if err := plot(ctx, au); err != nil {
		log.Errorf(ctx, "plot error: %+v", err)
		os.Exit(1)
	}
}
