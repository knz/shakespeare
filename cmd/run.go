package main

import (
	"bufio"
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/cockroachdb/cockroach/pkg/util/log"
)

func main() {
	flag.Parse()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)

	bctx := context.Background()
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

	// Read the configuration.
	rd := bufio.NewReader(os.Stdin)
	if err := parseCfg(rd); err != nil {
		log.Errorf(ctx, "parse error: %+v", err)
		os.Exit(1)
	}
	printCfg()

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
