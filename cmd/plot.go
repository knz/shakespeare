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
		title string
		fName string
		opts  string
	}
	type plotgroup struct {
		name      string
		jitter    bool
		plots     []plot
		ylabel    string
		numEvents int
	}
	var plots []plotgroup
	for _, a := range audiences {
		if !a.hasData {
			continue
		}
		group := plotgroup{
			ylabel: a.ylabel,
			name:   fmt.Sprintf("audience %s", a.name),
		}
		sigNum := 1
		for sigName, as := range a.signals {
			for actName := range as.hasData {
				fName := csvFileName(a.name, actName, sigName)
				pl := plot{
					fName: fName,
					title: fmt.Sprintf("%s %s", actName, sigName),
				}
				if as.drawEvents {
					pl.opts = fmt.Sprintf("using 1:(%d) with points pt 'o'", sigNum)
					sigNum++
					group.numEvents++
				} else {
					pl.opts = "using 1:2 with linespoints"
				}
				group.plots = append(group.plots, pl)
			}
		}
		plots = append(plots, group)
	}

	if minTime > 0 {
		minTime = 0
	}

	fmt.Fprintf(f, "set term pdf enhanced color size 7,10 font \",6\"\n")
	fmt.Fprintf(f, "set output 'plot.pdf'\n")
	fmt.Fprintf(f, "set multiplot layout %d,1\n", len(plots))
	fmt.Fprintf(f, "set xrange [%f:%f]\n", minTime, maxTime)
	fmt.Fprintf(f, "set xlabel 'time since start (s)'\n")
	fmt.Fprintln(f, "set jitter overlap 1 spread .25 vertical")

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
		fmt.Fprintf(f, "set title %q\n", p.name)
		if p.numEvents > 0 {
			fmt.Fprintf(f, "set yrange [%d:%d]\n", 0, p.numEvents+1)
		} else {
			fmt.Fprintf(f, "set yrange [*:*]\n")
		}
		fmt.Fprintf(f, "set ylabel %q\n", p.ylabel)
		fmt.Fprintln(f, `plot \`)
		for i, pl := range p.plots {
			fmt.Fprintf(f, "   '%s' %s t %q", pl.fName, pl.opts, pl.title)
			if i < len(p.plots)-1 {
				fmt.Fprint(f, `, \`)
			}
			fmt.Fprintln(f)
		}
	}
	fmt.Fprintf(f, "unset multiplot\n")

	return nil
}
