package cmd

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/stop"
	"golang.org/x/crypto/ssh/terminal"
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

var narratorCtx = logtags.AddTag(context.Background(), "narrator", nil)

func (ap *app) narrate(format string, args ...interface{}) {
	log.Infof(narratorCtx, format, args...)
	if ap.cfg.quiet {
		return
	}
	fmt.Printf("📢 "+format, args...)
	fmt.Println()
}

func (ap *app) witness(ctx context.Context, format string, args ...interface{}) {
	log.Infof(ctx, format, args...)
	if ap.cfg.quiet {
		return
	}
	s := fmt.Sprintf(format, args...)
	width, _, err := terminal.GetSize(1 /*stdout*/)
	if width == 0 || err != nil {
		fmt.Println(s)
	} else {
		if len(s) > 2*width/3-3 {
			s = s[:2*width/3-3] + "..."
		}
		fmt.Printf("%*s👀%s\n", width/3-2, " ", s)
		/*		pad := width - len(s) - 2
				if pad < 0 {
					pad = 0
				}
				fmt.Printf("%*s%s\n", pad, " ", s)*/
	}
}

func (ap *app) woops(ctx context.Context, format string, args ...interface{}) {
	log.Infof(ctx, format, args...)
	if ap.cfg.quiet {
		return
	}
	s := fmt.Sprintf(format, args...)
	width, _, err := terminal.GetSize(1 /*stdout*/)
	if width == 0 || err != nil {
		fmt.Println(s)
	} else {
		if len(s) > width/3-3 {
			s = s[:width/3-3] + "..."
		}
		fmt.Printf("%*s%s\n", 2*width/3-1, " ", s)
		/*		pad := width - len(s) - 2
				if pad < 0 {
					pad = 0
				}
				fmt.Printf("%*s%s\n", pad, " ", s)*/
	}
}

func (ap *app) intro() {
	playedRoles := make(map[string]struct{})
	for _, a := range ap.cfg.actors {
		playedRoles[a.role.name] = struct{}{}
	}
	ap.narrate("🎭 welcome a cast of %d actors, playing %d roles",
		len(ap.cfg.actors), len(playedRoles))
	ap.narrate("🎶 the play is starting; expected duration: %s", ap.cfg.tempo*time.Duration(len(ap.cfg.play)))
}
