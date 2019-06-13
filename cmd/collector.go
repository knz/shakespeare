package main

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/knz/shakespeare/cmd/log"
	"github.com/knz/shakespeare/cmd/timeutil"
)

type dataEvent struct {
	typ       parserType
	audiences []string
	auditors  []string
	actorName string
	sigName   string
	ts        time.Time
	val       string
}

type actionEvent struct {
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

func collect(
	ctx context.Context,
	au *audition,
	dataLogger *log.SecondaryLogger,
	actionChan <-chan actionEvent,
	spotlightChan <-chan dataEvent,
) error {
	if err := os.MkdirAll(*dataDir, os.ModePerm); err != nil {
		return err
	}
	of := newOutputFiles()
	defer func() {
		of.CloseAll()
	}()

	t := timeutil.NewTimer()
	t.Reset(time.Second)

	au.epoch = timeutil.Now()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-t.C:
			t.Read = true
			t.Reset(time.Second)
			of.Flush()
			continue

		case ev := <-actionChan:
			sinceBeginning := ev.startTime.Sub(au.epoch).Seconds()
			expandTimeRange(sinceBeginning)

			switch ev.typ {
			case actEvtMood:
				dataLogger.Logf(ctx, "%.2f mood set: %s", sinceBeginning, ev.output)
				if err := au.collectAndAuditMood(ctx, sinceBeginning, ev.output); err != nil {
					return err
				}

			case actEvtExec:
				a, ok := actors[ev.actor]
				if !ok {
					return fmt.Errorf("event received for non-existent actor: %+v", ev)
				}
				a.hasData = true

				status := 0
				if !ev.success {
					status = 1
				}

				dataLogger.Logf(ctx, "%.2f action %s:%s (%.4fs)", sinceBeginning, ev.actor, ev.action, ev.duration)

				fName := filepath.Join(*dataDir, fmt.Sprintf("%s.csv", ev.actor))
				w, err := of.getWriter(fName)
				if err != nil {
					return fmt.Errorf("opening %q: %+v", fName, err)
				}

				fmt.Fprintf(w, "%.4f %.4f %s %d %q\n",
					sinceBeginning, ev.duration, ev.action, status, ev.output)
			}
			continue

		case ev := <-spotlightChan:
			sinceBeginning := ev.ts.Sub(au.epoch).Seconds()
			expandTimeRange(sinceBeginning)

			dataLogger.Logf(ctx, "%.2f %+v %q %q",
				sinceBeginning, ev.audiences, ev.sigName, ev.val)

			for _, audienceName := range ev.audiences {
				a, ok := audiences[audienceName]
				if !ok {
					return fmt.Errorf("event received for non-existent audience %q: %+v", audienceName, ev)
				}
				a.hasData = true
				a.signals[ev.sigName].hasData[ev.actorName] = true
				fName := filepath.Join(*dataDir,
					csvFileName(audienceName, ev.actorName, ev.sigName))

				w, err := of.getWriter(fName)
				if err != nil {
					return fmt.Errorf("opening %q: %+v", fName, err)
				}
				// shuffle is a random value between [-.25, +.25] used to randomize event plots.
				shuffle := (.5 * rand.Float64()) - .25
				fmt.Fprintf(w, "%.4f %s %.3f\n", sinceBeginning, ev.val, shuffle)
			}

			evVar := exprVar{actorName: ev.actorName, sigName: ev.sigName}
			if err := au.checkEvent(ctx, sinceBeginning, evVar, ev.auditors, ev.typ, ev.val); err != nil {
				return err
			}

			continue
		}
		break
	}
	return nil
}

// outputFiles manages a collection of open files and associated
// buffered writers.
type outputFiles struct {
	files   map[string]*os.File
	writers map[string]*bufio.Writer
}

func newOutputFiles() *outputFiles {
	of := &outputFiles{}
	of.files = make(map[string]*os.File)
	of.writers = make(map[string]*bufio.Writer)
	return of
}

func (o *outputFiles) CloseAll() {
	for fName, f := range o.files {
		_ = o.writers[fName].Flush()
		_ = f.Close()
	}
}

func (o *outputFiles) Flush() {
	for _, w := range o.writers {
		_ = w.Flush()
	}
}

func (o *outputFiles) getWriter(fName string) (*bufio.Writer, error) {
	w, ok := o.writers[fName]
	if !ok {
		f, err := os.OpenFile(fName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return nil, err
		}
		o.files[fName] = f
		w = bufio.NewWriter(f)
		o.writers[fName] = w
	}
	return w, nil
}
