package cmd

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cockroachdb/logtags"
	"github.com/cockroachdb/ttycolor"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/stop"
	isatty "github.com/mattn/go-isatty"
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

	auditReported bool
	terminalWidth int32
	endCh         chan struct{}
}

func newApp(cfg *config) *app {
	r := &app{
		cfg:     cfg,
		au:      newAudition(cfg),
		minTime: math.Inf(1),
		maxTime: math.Inf(-1),
		endCh:   make(chan struct{}),
	}
	r.setTerminalSize()
	if isTerminal {
		go r.handleResize()
	}
	return r
}

func (ap *app) handleResize() {
	sigCh := make(chan os.Signal)
	signal.Notify(sigCh, syscall.SIGWINCH)
	for {
		select {
		case <-sigCh:
			ap.setTerminalSize()
		case <-ap.endCh:
			signal.Stop(sigCh)
			return
		}
	}
}

func (ap *app) setTerminalSize() {
	width, _, err := terminal.GetSize(1 /*stdout*/)
	if err == nil {
		atomic.StoreInt32(&ap.terminalWidth, int32(width))
	}
}

func (ap *app) close() {
	close(ap.endCh)
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

type urgency ttycolor.Code

const (
	I urgency = urgency(ttycolor.Reset)
	W urgency = urgency(ttycolor.Yellow)
	E urgency = urgency(ttycolor.Red)
)

var narratorCtx = logtags.AddTag(context.Background(), "narrator", nil)

func (ap *app) narrate(urgency urgency, symbol string, format string, args ...interface{}) {
	log.Infof(narratorCtx, format, args...)
	if ap.cfg.quiet {
		return
	}
	s := fmt.Sprintf(format, args...)
	if !ap.cfg.asciiOnly {
		if symbol == "" {
			symbol = "📢"
		}
		s = symbol + " " + s
	}
	if isTerminal && urgency != I {
		fmt.Printf("%s%s%s\n", ttycolor.StdoutProfile[ttycolor.Code(urgency)], s, ttycolor.StdoutProfile[ttycolor.Reset])
	} else {
		fmt.Println(s)
	}
}

func (ap *app) witness(ctx context.Context, format string, args ...interface{}) {
	log.Infof(ctx, format, args...)
	if ap.cfg.quiet {
		return
	}
	s := fmt.Sprintf(format, args...)
	if !ap.cfg.asciiOnly {
		s = "👀 " + s
	}
	width := int(atomic.LoadInt32(&ap.terminalWidth))
	if width == 0 || !isTerminal {
		fmt.Println(s)
	} else {
		if len(s) > 2*width/3-3 {
			s = s[:2*width/3-3] + "..."
		}
		fmt.Printf("%*s%s\n", width/3-3, " ", s)
	}
}

func (ap *app) woops(ctx context.Context, format string, args ...interface{}) {
	log.Infof(ctx, format, args...)
	if ap.cfg.quiet {
		return
	}
	s := fmt.Sprintf(format, args...)
	if !ap.cfg.asciiOnly {
		s = "😿 " + s
	}
	width := int(atomic.LoadInt32(&ap.terminalWidth))
	if width == 0 || !isTerminal {
		fmt.Println(s)
	} else {
		if len(s) > width/3-3 {
			s = s[:width/3-3] + "..."
		}
		fmt.Printf("%*s%s%s%s\n", 2*width/3-1, " ",
			ttycolor.StdoutProfile[ttycolor.Red],
			s,
			ttycolor.StdoutProfile[ttycolor.Reset])
	}
}

func (ap *app) intro() {
	playedRoles := make(map[string]struct{})
	for _, a := range ap.cfg.actors {
		playedRoles[a.role.name] = struct{}{}
	}
	ap.narrate(I, "👏", "welcome a cast of %d actors, playing %d roles",
		len(ap.cfg.actors), len(playedRoles))
	ap.narrate(I, "🎭", "dramatis personæ: %s", strings.Join(ap.cfg.actorNames, ", "))
	ap.narrate(I, "🎶", "the play is starting; expected duration: %s", ap.cfg.tempo*time.Duration(len(ap.cfg.play)))
}

var isTerminal = func() bool { return isatty.IsTerminal(os.Stdout.Fd()) }()
