package main

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

type dataEvent struct {
	typ       parserType
	actorName string
	eventName string
	ts        time.Time
	val       string
}

var collectedData = make(map[string]bool)

var minTime = math.Inf(1)
var maxTime = math.Inf(-1)

func collect(
	ctx context.Context, dataLogger *log.SecondaryLogger, collector <-chan dataEvent,
) error {
	if err := os.MkdirAll(*dataDir, os.ModePerm); err != nil {
		return err
	}
	files := make(map[string]*os.File)
	writers := make(map[string]*bufio.Writer)
	defer func() {
		for fName, f := range files {
			_ = writers[fName].Flush()
			_ = f.Close()
		}
	}()

	lastVals := make(map[string]float64)

	t := timeutil.NewTimer()
	t.Reset(time.Second)

	epoch := time.Now().UTC()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-t.C:
			t.Read = true
			t.Reset(time.Second)
			for _, w := range writers {
				_ = w.Flush()
			}
			continue

		case ev := <-collector:
			sinceBeginning := ev.ts.Sub(epoch).Seconds()

			if sinceBeginning > maxTime {
				maxTime = sinceBeginning
			}
			if sinceBeginning < minTime {
				minTime = sinceBeginning
			}

			baseName := fmt.Sprintf("%s.%s.csv", ev.actorName, ev.eventName)

			if ev.typ == parseDelta {
				curVal, err := strconv.ParseFloat(ev.val, 64)
				if err != nil {
					log.Warningf(ctx, "%s.%s: error parsing scalar: %+v", ev.actorName, ev.eventName, err)
					continue
				}
				prevVal := lastVals[baseName]
				ev.val = fmt.Sprintf("%f", curVal-prevVal)
				lastVals[baseName] = curVal
			}

			dataLogger.Logf(ctx, "%.2f %q %q %q",
				sinceBeginning, ev.actorName, ev.eventName, ev.val)

			fName := filepath.Join(*dataDir, baseName)
			w, ok := writers[fName]
			if !ok {
				f, err := os.OpenFile(fName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
				if err != nil {
					log.Errorf(ctx, "opening %q: %+v", fName, err)
					continue
				}
				files[fName] = f
				w = bufio.NewWriter(f)
				writers[fName] = w
				collectedData[baseName] = true
			}
			fmt.Fprintf(w, "%.4f %s\n", sinceBeginning, ev.val)
			continue
		}
		break
	}
	return nil
}
