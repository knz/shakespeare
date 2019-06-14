package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/knz/shakespeare/cmd/log"
	"github.com/knz/shakespeare/cmd/log/logtags"
)

func (ap *app) plot(ctx context.Context) error {
	ctx = logtags.AddTag(ctx, "plotter", nil)
	log.Info(ctx, "generating scripts")

	// Ensure the x axis always start at zero, even if no
	// event was received until later on the time line.
	if ap.minTime > 0 {
		ap.minTime = 0
	}
	// Sanity check.
	if ap.maxTime < 0 {
		ap.maxTime = 1
	}
	ap.narrate("the timeline extends from %.2fs to %.2fs, relative to %s",
		ap.minTime, ap.maxTime, ap.au.epoch)

	// Give some breathing room to action labels.
	ap.minTime -= 1.0
	ap.maxTime += 1.0

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
	for _, a := range ap.cfg.audiences {
		if !a.hasData {
			// No data for this audience, nothing to do.
			continue
		}
		// This audience has at least one plot. Prepare the group.
		group := plotgroup{
			ylabel: a.ylabel,
			title:  fmt.Sprintf("audience %s", a.name),
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
					title: fmt.Sprintf("%s %s", actName, sigName),
				}
				ap.narrate("observer %s found data for %s's %s: %s",
					a.name, actName, sigName, filepath.Join(ap.cfg.dataDir, fName))

				if as.drawEvents {
					// Indicate the value used in the title.
					pl.title = fmt.Sprintf("%s (around y=%d)", pl.title, sigNum)
					// If the signal is an event source, we'll plot points on
					// a horizontal line (with some jitter).
					pl.opts = fmt.Sprintf("using 1:(%d+$3):2 with labels hypertext point pt 6 ps .5", sigNum)
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

	// We'll write to two file named "plot.gp" and "runme.gp".
	// The user will be responsible for running gnuplot on the latter.
	fName := filepath.Join(ap.cfg.dataDir, "plot.gp")
	f, err := os.Create(fName)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
	}()
	ap.narrate("per-plot script: %s", fName)

	fmt.Fprintf(f, "# auto-generated file.\n# See 'runme.gp' to actually generate plots.\n")

	// Common plot definitions.

	// Our text labels may contain underscores, we don't want to have
	// them handled as subscripts (math notation).
	fmt.Fprintf(f, "set termoption noenhanced\n")

	// We want multiple plots sharing the same objects (overlays).
	fmt.Fprintf(f, "set multiplot layout %d,1\n", len(plots)+1)

	// We force the x range to be the same for all the plots.
	// If we did not do that, each plot may get a different x range
	// (adjusted automatically based on the data collected for that plot).
	fmt.Fprintf(f, "set xrange [%f:%f]\n", ap.minTime, ap.maxTime)
	if ap.maxTime < 10 {
		fmt.Fprintf(f, "set xtics out 1\n")
		fmt.Fprintf(f, "set mxtics 2\n")
	} else {
		fmt.Fprintf(f, "set xtics out 5\n")
		fmt.Fprintf(f, "set mxtics 5\n")
	}
	// Generate the action plot. we do this before generating the mood
	// overlays, since these are part of the "audience" observations.
	numActiveActors := 0
	for _, a := range ap.cfg.actors {
		if a.hasData {
			numActiveActors++
		}
	}
	fmt.Fprintf(f, "set title 'actions'\n")
	fmt.Fprintf(f, "set yrange [0:%d]\n", numActiveActors+1)
	fmt.Fprintf(f, "set ytics 1\n")
	fmt.Fprintf(f, "set ylabel ''\n")
	fmt.Fprintf(f, "set grid ytics\n")
	fmt.Fprintf(f, "plot \\\n")
	plotNum := 1
	for actorName, a := range ap.cfg.actors {
		if !a.hasData {
			continue
		}
		fmt.Fprintf(f, "  '%s.csv' using 1:(%d):1:($1+$2):(%d-0.25):(%d+0.25):(65536*($4 > 0 ? 255 : 0)+256*($4 > 0 ? 0 : 255)) "+
			"with boxxyerror notitle fs solid 1.0 fc rgbcolor variable, \\\n",
			actorName, plotNum, plotNum, plotNum)
		fmt.Fprintf(f, "  '%s.csv' using 1:(%d+0.25):3 with labels t '%s events (at y=%d)', \\\n",
			actorName, plotNum, actorName, plotNum)
		fmt.Fprintf(f, "  '%s.csv' using ($1+$2):(%d-0.25):5 with labels hypertext point pt 6 ps .5 notitle",
			actorName, plotNum)
		if plotNum < numActiveActors {
			fmt.Fprintf(f, ", \\")
		}
		fmt.Fprintln(f)
		plotNum++
	}

	// For event plots, ensure that the event dots do not overlap.
	// Note: this is disabled for now, because "labels hypertext" does not
	// implement the jitter option. Instead, we use a shuffle value
	// to move the event points randomly along the y axis.
	//
	// fmt.Fprintln(f, "set jitter overlap 1 spread .25 vertical")

	// Generate the mood overlays.
	for i, amb := range ap.au.moodPeriods {
		xstart := "graph 0"
		if !math.IsInf(amb.startTime, 0) {
			xstart = fmt.Sprintf("first %f", amb.startTime)
		}
		xend := "graph 1"
		if !math.IsInf(amb.endTime, 0) {
			xend = fmt.Sprintf("first %f", amb.endTime)
		}
		fmt.Fprintf(f, "set object %d rectangle from %s, graph 0 to %s, graph 1 fs solid 0.3 fc \"%s\"\n", i+1, xstart, xend, amb.mood)
	}

	// Plot the curves.
	for _, p := range plots {
		fmt.Fprintf(f, "set title %q\n", p.title)
		if p.numEvents > 0 {
			fmt.Fprintf(f, "set yrange [%d:%d]\n", 0, p.numEvents+1)
			fmt.Fprintf(f, "set grid ytics\n")
			fmt.Fprintf(f, "set ytics 1\n")
		} else {
			fmt.Fprintf(f, "set yrange [*:*]\n")
			fmt.Fprintf(f, "set grid noytics\n")
			fmt.Fprintf(f, "set ytics auto\n")
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
	for i := range ap.au.moodPeriods {
		fmt.Fprintf(f, "unset object %d\n", i+1)
	}
	fmt.Fprintf(f, "unset multiplot\n")

	fName = filepath.Join(ap.cfg.dataDir, "runme.gp")
	f2, err := os.Create(fName)
	if err != nil {
		return err
	}
	defer func() {
		_ = f2.Close()
	}()
	ap.narrate("plot-all script: %s", fName)

	// We'll generate PDF.
	fmt.Fprintf(f2, "# auto-generated file.\n# Run 'gnuplot runme.gp' to actually generate plots.\n")
	fmt.Fprintf(f2, "set term pdf color size 7,%d font \",6\"\n", 2*(len(plots)+1))
	fmt.Fprintf(f2, "set output 'plot.pdf'\n")
	fmt.Fprintf(f2, "load 'plot.gp'\n")
	fmt.Fprintf(f2, "set term svg mouse standalone size 600,%d dynamic font \",6\"\n", 200*(len(plots)+1))
	fmt.Fprintf(f2, "set output 'plot.svg'\n")
	fmt.Fprintf(f2, "load 'plot.gp'\n")

	log.Info(ctx, "done")

	return nil
}
