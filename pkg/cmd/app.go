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
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
	isatty "github.com/mattn/go-isatty"
	"golang.org/x/crypto/ssh/terminal"
)

type reporter interface {
	// start of the test.
	epoch() time.Time
	// update the time boundaries.
	expandTimeRange(float64)
	// retrieve the current time boundaries.
	getTimeRange() (float64, float64)

	// narrate reports messages in the left column.
	narrate(_ urgency, _, _ string, _ ...interface{})
	// witness reports messages in the middle column.
	witness(_ context.Context, _ string, _ ...interface{})
	// judge reports message in the right column.
	judge(_ context.Context, _ urgency, _, _ string, _ ...interface{})
}

type app struct {
	cfg     *config
	stopper *stop.Stopper

	theater theater

	// startTime is the instant at which the conductor initiates the play.
	// measurements are relative to this moment.
	startTime time.Time

	// minTime and maxTime are used to compute the x range of plots.
	// They are updated by the collector. Either can become negative if
	// the observed system has a clock running in the past relative to the
	// conductor.
	minTime float64
	maxTime float64

	isTerminal    bool
	terminalWidth int32
	endCh         chan struct{}
}

var _ reporter = (*app)(nil)

func newApp(cfg *config) *app {
	r := &app{
		cfg: cfg,
		theater: theater{
			au: newAudition(cfg),
		},
		minTime: math.Inf(1),
		maxTime: math.Inf(-1),
		endCh:   make(chan struct{}),
	}
	r.prepareTerm()
	return r
}

func (ap *app) epoch() time.Time { return ap.startTime }

func (ap *app) openDoors(ctx context.Context) {
	ap.startTime = timeutil.Now()
}

func (ap *app) prepareTerm() {
	f, ok := ap.cfg.narration.(*os.File)
	if !ok {
		return
	}
	stdout := f.Fd()
	ap.isTerminal = isatty.IsTerminal(stdout)
	ap.setTerminalSize(stdout)
	if ap.isTerminal {
		go ap.handleResize(stdout)
	}
}

func (ap *app) handleResize(fd uintptr) {
	sigCh := make(chan os.Signal)
	signal.Notify(sigCh, syscall.SIGWINCH)
	for {
		select {
		case <-sigCh:
			ap.setTerminalSize(fd)
		case <-ap.endCh:
			signal.Stop(sigCh)
			return
		}
	}
}

func (ap *app) setTerminalSize(fd uintptr) {
	width, _, err := terminal.GetSize(int(fd))
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

func (ap *app) getTimeRange() (float64, float64) {
	return ap.minTime, ap.maxTime
}

type urgency ttycolor.Code

const (
	I urgency = urgency(ttycolor.Reset)
	W urgency = urgency(ttycolor.Yellow)
	E urgency = urgency(ttycolor.Red)
	G urgency = urgency(ttycolor.Green)
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
	if ap.isTerminal && urgency != I {
		fmt.Fprintf(ap.cfg.narration, "%s%s%s\n", ttycolor.StdoutProfile[ttycolor.Code(urgency)], s, ttycolor.StdoutProfile[ttycolor.Reset])
	} else {
		fmt.Fprintln(ap.cfg.narration, s)
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
	if width == 0 || !ap.isTerminal {
		fmt.Fprintln(ap.cfg.narration, s)
	} else {
		if len(s) > 2*width/3-3 {
			s = s[:2*width/3-3] + "..."
		}
		fmt.Fprintf(ap.cfg.narration, "%*s%s\n", width/3-3, " ", s)
	}
}

func (ap *app) judge(ctx context.Context, u urgency, sym, format string, args ...interface{}) {
	log.Infof(ctx, format, args...)
	if ap.cfg.quiet {
		return
	}
	s := fmt.Sprintf(format, args...)
	if !ap.cfg.asciiOnly {
		s = sym + s
	}
	width := int(atomic.LoadInt32(&ap.terminalWidth))
	if width == 0 || !ap.isTerminal {
		fmt.Fprintln(ap.cfg.narration, s)
	} else {
		if len(s) > width/3-3 {
			s = s[:width/3-3] + "..."
		}
		fmt.Fprintf(ap.cfg.narration, "%*s%s%s%s\n", 2*width/3-1, " ",
			ttycolor.StdoutProfile[ttycolor.Code(u)],
			s,
			ttycolor.StdoutProfile[ttycolor.Reset])
	}
}

func (ap *app) intro() {
	if len(ap.cfg.titleStrings) > 0 {
		ap.narrate(I, "❦", "hear, hear, a tale of %s", joinAnd(ap.cfg.titleStrings))
	}
	if len(ap.cfg.authors) > 0 {
		ap.narrate(I, "🧙", "brought to you by %s", joinAnd(ap.cfg.authors))
	}
	for _, seeAlso := range ap.cfg.seeAlso {
		ap.narrate(I, "🛎️", "attention! %s", seeAlso)
	}
	if len(ap.cfg.actorNames) > 0 {
		playedRoles := make(map[string]struct{})
		for _, a := range ap.cfg.actors {
			playedRoles[a.role.name] = struct{}{}
		}
		ap.narrate(I, "👏", "please welcome a cast of %d actors, playing %d roles",
			len(ap.cfg.actors), len(playedRoles))
	}
	if len(ap.cfg.actorNames) > 0 {
		ap.narrate(I, "🎭", "dramatis personæ: %s", strings.Join(ap.cfg.actorNames, ", "))
	}
	ap.narrate(I, "🎶", "the play is starting; expected duration: %s", ap.cfg.tempo*time.Duration(len(ap.cfg.play)))
}

func joinAnd(s []string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	default:
		return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
	}
}
