package main

import (
	"fmt"
	"math"
	"time"

	"github.com/knz/shakespeare/cmd/stop"
)

type app struct {
	cfg     *config
	stopper *stop.Stopper

	au *audition

	// minTime and maxTime are used to compute the x range of plots.
	// They are updated by the collector. Either can become negative if
	// the observed system has a clock running in the past relative to the
	// conductor.
	minTime float64
	maxTime float64
}

func newApp(cfg *config) *app {
	r := &app{
		cfg:     cfg,
		au:      newAudition(cfg),
		minTime: math.Inf(1),
		maxTime: math.Inf(-1),
	}
	return r
}

// expandTimeRange should be called for each processed event time stamp.
func (ap *app) expandTimeRange(instant float64) {
	if instant > ap.maxTime {
		ap.maxTime = instant
	}
	if instant < ap.minTime {
		ap.minTime = instant
	}
}

func (ap *app) report(format string, args ...interface{}) {
	if ap.cfg.quiet {
		return
	}
	fmt.Printf(format, args...)
	fmt.Println()
}

func (ap *app) intro() {
	playedRoles := make(map[string]struct{})
	for _, a := range ap.cfg.actors {
		playedRoles[a.role.name] = struct{}{}
	}
	ap.report("welcome a cast of %d actors, playing %d roles",
		len(ap.cfg.actors), len(playedRoles))
	ap.report("the play is starting; expected duration: %s", ap.cfg.tempo*time.Duration(len(ap.cfg.play)))
}
