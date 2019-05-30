package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

func plot(ctx context.Context) error {
	// plot describes one curve in a plot group.
	type plot struct {
		// title is the string spelled out in the legend for that curve.
		title string
		// fName is the name of the file the data is read from.
		fName string
		// opts is the plot style.
		opts string
	}
	// plotgroup describes one audience graph (a set of curves).
	type plotgroup struct {
		// title is the string displayed at the top of the graph.
		title string
		// ylabel is the label for the y-axis.
		ylabel string
		// plots is the list of curves in the graph.
		plots []plot
		// numEvents indicates the number of event lines in the graph.
		// This is used to extend the y range to include all the event lines.
		numEvents int
	}
	// plots is the list of all audience graphs.
	var plots []plotgroup

	// Analyze the collected data, and prepare the plot specifications.
	for _, a := range audiences {
		if !a.hasData {
			// No data for this audience, nothign to do.
			continue
		}
		// This audience has at least one plot. Prepare the group.
		group := plotgroup{
			ylabel: a.ylabel,
			title:  fmt.Sprintf("audience %s", strings.Replace(a.name, "_", " ", -1)),
		}

		// Find the timeseries to plot for the audience.
		sigNum := 1
		for sigName, as := range a.signals {
			// Only look at the actors watched by the audience where
			// there was actual data received.
			for actName := range as.hasData {
				fName := csvFileName(a.name, actName, sigName)
				pl := plot{
					fName: fName,
					title: fmt.Sprintf("%s %s",
						strings.Replace(actName, "_", " ", -1),
						strings.Replace(sigName, "_", " ", -1)),
				}

				if as.drawEvents {
					// If the signal is an event source, we'll plot points on
					// a horizontal line (with some jitter).
					pl.opts = fmt.Sprintf("using 1:(%d) with points pt 'o'", sigNum)
					sigNum++
					group.numEvents++
				} else {
					// In the common case, we plot a line with points.
					pl.opts = "using 1:2 with linespoints"
				}
				group.plots = append(group.plots, pl)
			}
		}
		plots = append(plots, group)
	}

	// We'll write to a file named "plot.gp".
	// The user will be responsible for running gnuplot on it.
	fName := filepath.Join(*dataDir, "plot.gp")
	f, err := os.Create(fName)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
	}()

	// Common plot definitions.

	// We'll generate PDF.
	fmt.Fprintf(f, "set term pdf enhanced color size 7,10 font \",6\"\n")
	fmt.Fprintf(f, "set output 'plot.pdf'\n")

	// We want multiple plots sharing the same objects (overlays).
	fmt.Fprintf(f, "set multiplot layout %d,1\n", len(plots))

	// Ensure the x axis always start at zero, even if no
	// event was received until later on the time line.
	if minTime > 0 {
		minTime = 0
	}
	fmt.Fprintf(f, "set xrange [%f:%f]\n", minTime, maxTime)
	fmt.Fprintf(f, "set xtics out 5\n")
	fmt.Fprintf(f, "set mxtics 5\n")

	// For event plots, ensure that the event dots do not overlap.
	fmt.Fprintln(f, "set jitter overlap 1 spread .25 vertical")

	// Generate the ambiance overlays.
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

	// Plot the curves.
	for _, p := range plots {
		fmt.Fprintf(f, "set title %q\n", p.title)
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

	// End the plot set.
	fmt.Fprintf(f, "unset multiplot\n")

	return nil
}
