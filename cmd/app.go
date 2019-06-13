package main

import (
	"math"

	"github.com/knz/shakespeare/cmd/stop"
)

type app struct {
	cfg     *config
	stopper *stop.Stopper

	au *audition

	// minTime and maxTime are used to compute the x range of plots.
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
func (a *app) expandTimeRange(instant float64) {
	if instant > a.maxTime {
		a.maxTime = instant
	}
	if instant < a.minTime {
		a.minTime = instant
	}
}
