package cmd

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

type observation struct {
	typ       parserType
	observers []string
	auditors  []string
	actorName string
	sigName   string
	ts        time.Time
	val       string
}

type actionReport struct {
	typ       actReportType
	startTime time.Time
	// duration is the duration of the command for regular action
	// reports.
	duration float64
	// actor is the observed actor for regular action reports,
	// or the auditor name for violation reports.
	actor string
	// action is the executed action for regular action reports,
	// unused otherwise.
	action  string
	success bool
	// output represents
	// - for regular action reports, the stdout/stderr of the command
	// - for mood changes, the new mood
	output string
}

type actReportType int

const (
	reportActionExec actReportType = iota
	reportMoodChange
	reportAuditViolation
)

func csvFileName(observerName, actorName, sigName string) string {
	return fmt.Sprintf("%s.%s.%s.csv", observerName, actorName, sigName)
}

func (ap *app) collect(
	ctx context.Context,
	dataLogger *log.SecondaryLogger,
	actionChan <-chan actionReport,
	collectorChan <-chan observation,
) error {
	of := newOutputFiles()
	defer func() {
		of.CloseAll()
	}()

	t := timeutil.NewTimer()
	t.Reset(time.Second)

	for {
		select {
		case <-ap.stopper.ShouldStop():
			log.Info(ctx, "interrupted")
			return nil

		case <-ctx.Done():
			log.Info(ctx, "canceled")
			return ctx.Err()

		case <-t.C:
			t.Read = true
			t.Reset(time.Second)
			of.Flush()
			continue

		case ev := <-actionChan:
			sinceBeginning := ev.startTime.Sub(ap.au.epoch).Seconds()
			ap.expandTimeRange(sinceBeginning)

			switch ev.typ {
			case reportMoodChange:
				dataLogger.Logf(ctx, "%.2f mood set: %s", sinceBeginning, ev.output)

			case reportAuditViolation:
				a, ok := ap.au.auditorStates[ev.actor]
				if !ok {
					return errors.Newf("event received for non-existent auditor: %+v", ev)
				}
				a.hasData = true

				status := 0
				if !ev.success {
					status = 1
				}

				dataLogger.Logf(ctx, "%.2f audit check by %s: %v (%q)", sinceBeginning, ev.actor, ev.success, ev.output)
				fName := filepath.Join(ap.cfg.dataDir, fmt.Sprintf("audit-%s.csv", ev.actor))
				w, err := of.getWriter(fName)
				if err != nil {
					return errors.Wrapf(err, "opening %q", fName)
				}

				if ev.output != "" {
					fmt.Fprintf(w, "%.4f %d %q\n", sinceBeginning, status, ev.output)
				} else {
					fmt.Fprintf(w, "%.4f %d\n", sinceBeginning, status)
				}

			case reportActionExec:
				a, ok := ap.cfg.actors[ev.actor]
				if !ok {
					return errors.Newf("event received for non-existent actor: %+v", ev)
				}
				a.hasData = true

				status := 0
				if !ev.success {
					status = 1
				}

				dataLogger.Logf(ctx, "%.2f action %s:%s (%.4fs)", sinceBeginning, ev.actor, ev.action, ev.duration)

				fName := filepath.Join(ap.cfg.dataDir, fmt.Sprintf("%s.csv", ev.actor))
				w, err := of.getWriter(fName)
				if err != nil {
					return errors.Wrapf(err, "opening %q", fName)
				}

				fmt.Fprintf(w, "%.4f %.4f %s %d %q\n",
					sinceBeginning, ev.duration, ev.action, status, ev.output)
			}
			continue

		case ev := <-collectorChan:
			sinceBeginning := ev.ts.Sub(ap.au.epoch).Seconds()
			ap.expandTimeRange(sinceBeginning)

			dataLogger.Logf(ctx, "%.2f %+v %q %q",
				sinceBeginning, ev.observers, ev.sigName, ev.val)

			for _, obsName := range ev.observers {
				a, ok := ap.cfg.audience[obsName]
				if !ok {
					return errors.Newf("event received for non-existent audience %q: %+v", obsName, ev)
				}
				a.observer.hasData = true
				a.observer.signals[ev.sigName].hasData[ev.actorName] = true
				fName := filepath.Join(ap.cfg.dataDir,
					csvFileName(obsName, ev.actorName, ev.sigName))

				w, err := of.getWriter(fName)
				if err != nil {
					return errors.Wrapf(err, "opening %q: %+v", fName)
				}
				// shuffle is a random value between [-.25, +.25] used to randomize event plots.
				shuffle := (.5 * rand.Float64()) - .25
				fmt.Fprintf(w, "%.4f %s %.3f\n", sinceBeginning, ev.val, shuffle)
			}

			continue
		}
		break
	}
	return nil
}
