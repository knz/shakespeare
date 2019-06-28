package cmd

import (
	"context"
	"fmt"
	"html"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
)

func (ap *app) plot(ctx context.Context, foundFailure bool) error {
	ctx = logtags.AddTag(ctx, "plotter", nil)
	log.Info(ctx, "generating scripts")

	// Sanity checking.
	if ap.maxTime < ap.minTime {
		ap.minTime, ap.maxTime = ap.maxTime, ap.minTime
	}
	// Ensure the x axis always start at zero, even if no
	// event was received until later on the time line.
	if ap.minTime > 0 {
		ap.minTime = 0
	}
	// Sanity check.
	if ap.maxTime < 0 {
		ap.maxTime = 1
	}
	// More sanity check.
	if ap.maxTime < ap.minTime+1 {
		ap.maxTime = ap.minTime + 1
	}

	ap.narrate(I, "ℹ️ ", "the timeline extends from %.2fs to %.2fs, relative to %s",
		ap.minTime, ap.maxTime, ap.au.epoch)

	numPlots, err := ap.subPlots(ctx, "plot.gp", ap.minTime, ap.maxTime)
	if err != nil {
		return err
	}

	repeatTs := math.Inf(-1)
	beforeLastTs := repeatTs
	if ap.cfg.repeatActNum > 0 {
		// Find the timestamp where the last repetition started.
		for _, acn := range ap.au.actChanges {
			if acn.actNum == ap.cfg.repeatActNum {
				beforeLastTs = repeatTs
				repeatTs = acn.ts
				// No break here: we want to get the ts for the last occurrence.
			}
		}
	}
	if !math.IsInf(beforeLastTs, 0) {
		// Use the next-to-last iteration if available.
		repeatTs = beforeLastTs
	}
	hasRepeat := !math.IsInf(repeatTs, 0)
	if hasRepeat {
		if _, err := ap.subPlots(ctx, "lastplot.gp", repeatTs, ap.maxTime); err != nil {
			return err
		}
	}

	if err := func() error {
		fName := filepath.Join(ap.cfg.dataDir, "runme.gp")
		f, err := os.Create(fName)
		if err != nil {
			return err
		}
		defer f.Close()
		ap.narrate(I, "📜", "plot-all script: %s", fName)

		// We'll generate PDF.
		fmt.Fprintf(f, "# auto-generated file.\n# Run 'gnuplot runme.gp' to actually generate plots.\n")
		fmt.Fprintf(f, "set term pdf color size 7,%d font \",6\"\n", 2*numPlots)
		fmt.Fprintf(f, "set output 'plot.pdf'\n")
		fmt.Fprintf(f, "load 'plot.gp'\n")
		if hasRepeat {
			fmt.Fprintf(f, "set output 'lastplot.pdf'\n")
			fmt.Fprintf(f, "load 'lastplot.gp'\n")
		}
		fmt.Fprintf(f, "set term svg mouse standalone size 600,%d dynamic font \",6\"\n", 200*numPlots)
		fmt.Fprintf(f, "set output 'plot.svg'\n")
		fmt.Fprintf(f, "load 'plot.gp'\n")
		if hasRepeat {
			fmt.Fprintf(f, "set output 'lastplot.svg'\n")
			fmt.Fprintf(f, "load 'lastplot.gp'\n")
		}

		return nil
	}(); err != nil {
		return err
	}

	if err := func() error {
		fName := filepath.Join(ap.cfg.dataDir, "plot.html")
		f, err := os.Create(fName)
		if err != nil {
			return err
		}
		defer f.Close()
		ap.narrate(I, "📄", "HTML include for SVG plots: %s", fName)
		fmt.Fprintln(f, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"/>`)
		fmt.Fprint(f, "<title>shakespeare report")
		if len(ap.cfg.titleStrings) > 0 {
			fmt.Fprintf(f, ": a tale of %s", html.EscapeString(joinAnd(ap.cfg.titleStrings)))
		}
		fmt.Fprintln(f, `</title>`)
		if len(ap.cfg.authors) > 0 {
			fmt.Fprintf(f, "<meta name='author' content='%s' />\n", html.EscapeString(joinAnd(ap.cfg.authors)))
		}
		fmt.Fprintln(f, `<link href="https://fonts.googleapis.com/css?family=Nova+Mono&display=swap" rel="stylesheet">`)
		fmt.Fprintln(f, `<link href="https://fonts.googleapis.com/css?family=Pinyon+Script&display=swap" rel="stylesheet">`)
		fmt.Fprintln(f, `<style type='text/css'>
h1,h3,p,ul{text-align: center;}
p,li{font-family:'Pinyon Script',cursive;}
pre,code{font-family: 'Nova Mono', monospace;}
.kw{font-weight:bold;}
.rn{color:blue;font-style:italic;}
.acn{color:blue;font-style:italic;font-weight:bold;}
.sn{color:darkgreen;font-style:italic;}
.an{color:purple;font-style:italic;}
.ann{color:orange;font-style:italic;}
.sh{color:#444;}
.re{color:green;}
.mod{font-style:italic;}
</style></head><body>`)
		if len(ap.cfg.titleStrings) > 0 {
			fmt.Fprintf(f, "<h1>A tale of %s</h1>\n", html.EscapeString(joinAnd(ap.cfg.titleStrings)))
		}
		if len(ap.cfg.authors) > 0 {
			fmt.Fprintf(f, "<h3>Written by %s</h3>\n", html.EscapeString(joinAnd(ap.cfg.authors)))
		}
		fmt.Fprintf(f, "<p>%s<p>\n", formatDatePretty(ap.au.epoch))
		if foundFailure {
			fmt.Fprintln(f, "<p>Avert your eyes! For this tale, alas, does not end well.<p>")
		} else {
			fmt.Fprintln(f, "<p>Rejoice! This tale ends well.<p>")
		}
		const divider = `<p style="font-family:serif; margin-top:3em; margin-bottom:3em;">⊱ ────────────────── {.⋅ ♫ ⋅.} ───────────────── ⊰</p>`
		fmt.Fprintln(f, divider)
		fmt.Fprintln(f, `<div style="margin-left: auto; margin-right: auto; max-width: 1024px"><embed id="E" src="plot.svg"/></div>`)
		if hasRepeat {
			fmt.Fprintln(f, divider)
			fmt.Fprintf(f, "<p>For your delicate eyes, the last %.1f seconds of the play:</p>\n", ap.maxTime-repeatTs)
			fmt.Fprintln(f, `<div style="margin-left: auto; margin-right: auto; max-width: 1024px"><embed id="E" src="lastplot.svg"/></div>`)
		}
		if len(ap.cfg.seeAlso) > 0 {
			fmt.Fprintln(f, divider)
			fmt.Fprintln(f, "<p>Attention! You may want to know:</p><ul>")
			for _, seeAlso := range ap.cfg.seeAlso {
				h := html.EscapeString(seeAlso)
				if strings.Contains(seeAlso, "://") {
					fmt.Fprintf(f, `<li><a href='%s'><small><code>%s</code></small></a></li>`, h, h)
				} else {
					fmt.Fprintf(f, `<li>%s</li>`, h)
				}
			}
			fmt.Fprintln(f, "</ul>")
		}
		fmt.Fprintln(f, divider)
		fmt.Fprintln(f, "<p>For your curious eyes, the full book for this play:</p>")
		fmt.Fprintln(f, "<div style='margin-left: auto; margin-right: auto; max-width: 800px'><pre style='font-size: small'>")
		ap.cfg.printCfg(f, true /*skipComs*/, true /*annot*/)
		ap.cfg.printSteps(f, true /*annot*/)
		fmt.Fprintln(f, "</pre></div>")
		fmt.Fprintln(f, "<p><em><small>a report produced by <a href='https://github.com/knz/shakespeare'>Shakespeare</a></small></em></p>")
		fmt.Fprintln(f, `</body></html>`)
		return nil
	}(); err != nil {
		return err
	}

	ap.maybeRunGnuplot(ctx, hasRepeat)
	log.Info(ctx, "done")
	return nil
}

func (ap *app) subPlots(
	ctx context.Context, outGpFileName string, minTime, maxTime float64,
) (int, error) {
	// Give some breathing room to action labels.
	visibleDuration := maxTime - minTime
	minTime -= .05 * visibleDuration
	maxTime += .05 * visibleDuration

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
	// plotGroups is the list of all audience graphs.
	var plotGroups []plotgroup

	// Analyze the collected data, and prepare the plot specifications.
	for _, audienceName := range ap.cfg.audienceNames {
		a := ap.cfg.audience[audienceName]
		if !a.observer.hasData || a.observer.disablePlot {
			// No data for this audience, or plot disabled, nothing to do.
			continue
		}
		// This audience has at least one plot. Prepare the group.
		group := plotgroup{
			ylabel: a.observer.ylabel,
			title:  fmt.Sprintf("observer %s", a.name),
		}

		if a.auditor.expectFsm != nil || len(a.auditor.assignments) > 0 {
			if a.auditor.activeCond.src != "" && a.auditor.activeCond.src != "true" {
				group.title += fmt.Sprintf("\naudits, only when %s", a.auditor.activeCond.src)
			} else {
				group.title += "\naudits, throughout"
			}
			if a.auditor.expectFsm != nil {
				group.title += fmt.Sprintf("\nexpects %s: %s", a.auditor.expectFsm.name, a.auditor.expectExpr.src)
			}
		}

		// Find the timeseries to plot for the audience.
		sigNum := 1
		for _, varName := range a.observer.obsVarNames {
			obsVar := a.observer.obsVars[varName]
			if !obsVar.hasData {
				// Only look at the actors watched by the audience where
				// there was actual data received.
				continue
			}
			actName := varName.actorName
			fName := csvFileName(a.name, actName, varName.sigName)
			pl := plot{
				fName: fName,
				title: fmt.Sprintf("%s %s", actName, varName.sigName),
			}
			ap.narrate(I, "📈", "observer %s found data for %s's %s: %s",
				a.name, actName, varName.sigName, filepath.Join(ap.cfg.dataDir, fName))

			if obsVar.drawEvents {
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

		// Find the timeserie(s) to plot for the auditor.
		if as, ok := ap.au.auditorStates[a.name]; ok && as.hasData {
			fName := fmt.Sprintf("audit-%s.csv", a.name)

			ap.narrate(I, "📈", "observer %s found audit data: %s",
				a.name, filepath.Join(ap.cfg.dataDir, fName))

			pl := plot{fName: fName}

			pl.opts = "using 1:(.87):(faces[$2+1]) with labels font ',14'  axes x1y2"
			group.plots = append(group.plots, pl)
			pl.opts = "using 1:(.8):3 with labels hypertext point pt 17 axes x1y2"
			group.plots = append(group.plots, pl)
		}

		plotGroups = append(plotGroups, group)
	}

	// We'll write to two file named "plot.gp" and "runme.gp".
	// The user will be responsible for running gnuplot on the latter.

	if err := func() error {
		fName := filepath.Join(ap.cfg.dataDir, outGpFileName)
		f, err := os.Create(fName)
		if err != nil {
			return err
		}
		defer f.Close()
		ap.narrate(I, "📜", "per-plot script: %s", fName)

		fmt.Fprintf(f, "# auto-generated file.\n# See 'runme.gp' to actually generate plots.\n")

		// Common plot definitions.

		// Our text labels may contain underscores, we don't want to have
		// them handled as subscripts (math notation).
		fmt.Fprintln(f, "set termoption noenhanced")

		// We want multiple plots sharing the same objects (overlays).
		fmt.Fprintf(f, "set multiplot layout %d,1\n", len(plotGroups)+1)

		// Ensure all the x-axes are aligned.
		fmt.Fprintln(f, "set lmargin at screen 0.05\nset rmargin at screen 0.98")

		// Auditor faces.
		fmt.Fprintf(f, `array faces[4]
faces[1] = "😺"
faces[2] = "🙀"
faces[3] = "😿"
faces[4] = ""
`)

		// We force the x range to be the same for all the plots.
		// If we did not do that, each plot may get a different x range
		// (adjusted automatically based on the data collected for that plot).
		fmt.Fprintf(f, "set xrange [%f:%f]\n", minTime, maxTime)
		if ap.maxTime < 10 {
			fmt.Fprintf(f, "set xtics out 1\n")
			fmt.Fprintf(f, "set mxtics 2\n")
		} else {
			fmt.Fprintf(f, "set xtics out 5\n")
			fmt.Fprintf(f, "set mxtics 5\n")
		}

		// Generate the act boundaries.
		for i := 1; i < len(ap.au.actChanges); i++ {
			ts := ap.au.actChanges[i].ts
			fmt.Fprintf(f, "set arrow from %f, graph 0 to %f, graph 1 front nohead lc 'blue'\n", ts, ts)
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
		fmt.Fprintf(f, "set y2range [0:1]\n")
		fmt.Fprintf(f, "set grid ytics\n")
		if numActiveActors == 0 {
			fmt.Fprintf(f, "plot .5 t 'nothingness!'\n")
		} else {
			fmt.Fprintf(f, "plot \\\n")
			plotNum := 1
			for _, actorName := range ap.cfg.actorNames {
				a := ap.cfg.actors[actorName]
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
		for _, pg := range plotGroups {
			fmt.Fprintf(f, "set title %q\n", pg.title)
			if pg.numEvents > 0 {
				fmt.Fprintf(f, "set yrange [%d:%d]\n", 0, pg.numEvents+1)
				fmt.Fprintf(f, "set grid ytics\n")
				fmt.Fprintf(f, "set ytics 1\n")
			} else {
				fmt.Fprintf(f, "set yrange [*:*]\n")
				fmt.Fprintf(f, "set grid noytics\n")
				fmt.Fprintf(f, "set ytics auto\n")
			}
			fmt.Fprintf(f, "set ylabel %q\n", pg.ylabel)
			fmt.Fprintln(f, `plot \`)
			for i, pl := range pg.plots {
				fmt.Fprintf(f, "   '%s' %s t %q", pl.fName, pl.opts, pl.title)
				if i < len(pg.plots)-1 {
					fmt.Fprint(f, `, \`)
				}
				fmt.Fprintln(f)
			}
		}

		// End the plot set.
		for i := range ap.au.moodPeriods {
			fmt.Fprintf(f, "unset object %d\n", i+1)
		}
		fmt.Fprintln(f, "unset arrow")
		fmt.Fprintln(f, "unset multiplot")
		return nil
	}(); err != nil {
		return 0, err
	}

	return len(plotGroups) + 1, nil
}

func (ap *app) maybeRunGnuplot(ctx context.Context, hasRepeat bool) {
	cmd := exec.CommandContext(ctx, ap.cfg.gnuplotPath, "runme.gp")
	cmd.Dir = ap.cfg.dataDir
	res, err := cmd.CombinedOutput()
	log.Infof(ctx, "gnuplot:\n%s\n-- %v / %s", string(res), err, cmd.ProcessState)
	if err != nil {
		ap.narrate(W, "⚠️", "you will need to run gnuplot manually: cd %s; %s runme.gp", ap.cfg.dataDir, ap.cfg.gnuplotPath)
	} else {
		ap.narrate(I, "📄", "SVG plot: %s", filepath.Join(ap.cfg.dataDir, "plot.svg"))
		ap.narrate(I, "📄", "PDF plot: %s", filepath.Join(ap.cfg.dataDir, "plot.pdf"))
		if hasRepeat {
			ap.narrate(I, "📄", "SVG plot: %s", filepath.Join(ap.cfg.dataDir, "lastplot.svg"))
			ap.narrate(I, "📄", "PDF plot: %s", filepath.Join(ap.cfg.dataDir, "lastplot.pdf"))
		}
	}
}

func formatDatePretty(t time.Time) string {
	day := t.Format("2")
	switch {
	case strings.HasSuffix(day, "1"):
		day += "st"
	case strings.HasSuffix(day, "2"):
		day += "nd"
	case strings.HasSuffix(day, "3"):
		day += "rd"
	default:
		day += "th"
	}

	hour := t.Format("<a title='UTC'>15:06</a>")

	return fmt.Sprintf("The day was a %s, %s the %s in the glorious year of %s; at %s on that fateful day, the story began...",
		t.Format("Monday"),
		t.Format("January"),
		day,
		t.Format("2006"),
		hour)
}
