package cmd

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"time"

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

type performedAction struct {
	typ       actEvtType
	startTime time.Time
	duration  float64
	actor     string
	action    string
	success   bool
	output    string
}

type actEvtType int

const (
	actEvtExec actEvtType = iota
	actEvtMood
)

func csvFileName(audienceName, actorName, sigName string) string {
	return fmt.Sprintf("%s.%s.%s.csv", audienceName, actorName, sigName)
}

func (ap *app) collect(
	ctx context.Context,
	dataLogger *log.SecondaryLogger,
	actionChan <-chan performedAction,
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
			case actEvtMood:
				dataLogger.Logf(ctx, "%.2f mood set: %s", sinceBeginning, ev.output)

			case actEvtExec:
				a, ok := ap.cfg.actors[ev.actor]
				if !ok {
					return fmt.Errorf("event received for non-existent actor: %+v", ev)
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
					return fmt.Errorf("opening %q: %+v", fName, err)
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
				a, ok := ap.cfg.observers[obsName]
				if !ok {
					return fmt.Errorf("event received for non-existent audience %q: %+v", obsName, ev)
				}
				a.hasData = true
				a.signals[ev.sigName].hasData[ev.actorName] = true
				fName := filepath.Join(ap.cfg.dataDir,
					csvFileName(obsName, ev.actorName, ev.sigName))

				w, err := of.getWriter(fName)
				if err != nil {
					return fmt.Errorf("opening %q: %+v", fName, err)
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
