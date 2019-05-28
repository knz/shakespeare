package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

func plot(ctx context.Context) error {
	fName := filepath.Join(*dataDir, "plot.gp")
	f, err := os.Create(fName)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
	}()

	type plot struct {
		fName  string
		jitter bool
		opts   string
	}
	var plots []plot
	for _, a := range actors {
		for _, rp := range a.role.resParsers {
			fName := fmt.Sprintf("%s.%s.csv", a.name, rp.name)
			if !collectedData[fName] {
				continue
			}
			pl := plot{fName: fName}
			switch rp.typ {
			case parseEvent:
				pl.jitter = true
				pl.opts = "using 1:(1) with points"
			case parseScalar, parseDelta:
				pl.opts = "using 1:2 with linespoints"
			}
			plots = append(plots, pl)
		}
	}

	if minTime > 0 {
		minTime = 0
	}

	fmt.Fprintf(f, "set term pdf enhanced color size 7,10 font \",6\"\n")
	fmt.Fprintf(f, "set output 'plot.pdf'\n")
	fmt.Fprintf(f, "set multiplot layout %d,1\n", len(plots))
	fmt.Fprintf(f, "set xrange [%f:%f]\n", minTime, maxTime)

	// set object 1 rectangle from graph .5, graph 0 to graph 1, graph .5 fs solid 0.5 fc "red"
	for i, amb := range ambiances {
		xstart := "graph 0"
		if !math.IsInf(amb.startTime, 0) {
			xstart = fmt.Sprintf("first %f", amb.startTime)
		}
		xend := "graph 1"
		if !math.IsInf(amb.endTime, 0) {
			xend = fmt.Sprintf("first %f", amb.endTime)
		}
		fmt.Fprintf(f, "set object %d rectangle from %s, graph 0 to %s, graph 1 fs solid 0.3 fc \"%s\"\n", i+1, xstart, xend, amb.ambiance)
	}

	for _, p := range plots {
		if p.jitter {
			fmt.Fprintln(f, "set jitter overlap .1 spread .05 vertical")
		}
		fmt.Fprintf(f, "plot '%s' %s\n", p.fName, p.opts)
		if p.jitter {
			fmt.Fprintln(f, "unset jitter")
		}
	}
	fmt.Fprintf(f, "unset multiplot\n")

	return nil
}
