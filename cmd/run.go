package main

import (
	"bufio"
	"context"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/log/logflags"
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
	if *parseOnly {
		return
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(bctx)
	go func() {
		_ = <-sigCh
		cancel()
	}()

	defer func() {
		// Terminate all the tasks.
		close(sigCh)
		cancel()
	}()

	// Generate the steps.
	compile()
	printSteps()

	// Run the scenario.
	conduct(ctx)

	// Generate the plot script.
	if err := plot(ctx); err != nil {
		log.Errorf(ctx, "plot error: %+v", err)
		os.Exit(1)
	}
}
