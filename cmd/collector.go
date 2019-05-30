package main

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

type dataEvent struct {
	typ       parserType
	audiences []string
	actorName string
	eventName string
	ts        time.Time
	val       string
}

func csvFileName(audienceName, actorName, sigName string) string {
	return fmt.Sprintf("%s.%s.%s.csv", audienceName, actorName, sigName)
}

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

			dataLogger.Logf(ctx, "%.2f %+v %q %q",
				sinceBeginning, ev.audiences, ev.eventName, ev.val)

			for _, audienceName := range ev.audiences {
				a, ok := audiences[audienceName]
				if !ok {
					return fmt.Errorf("event received for non-existence audience %q: %+v", audienceName, ev)
				}
				a.hasData = true
				a.signals[ev.eventName].hasData[ev.actorName] = true
				fName := filepath.Join(*dataDir, csvFileName(audienceName, ev.actorName, ev.eventName))

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
				}
				fmt.Fprintf(w, "%.4f %s\n", sinceBeginning, ev.val)
			}
			continue
		}
		break
	}
	return nil
}
