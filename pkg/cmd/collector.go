package cmd

import (
	"context"
	"fmt"
	"html"
	"math/rand"
	"path/filepath"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

type observation struct {
	ts      float64
	typ     sigType
	varName varName
	val     string
}

type actionReport struct {
	typ       actReportType
	startTime float64
	// duration is the duration of the command (in seconds) for regular
	// action reports.
	duration float64
	// actor is the observed actor for regular action reports,
	// or the auditor name for violation reports.
	actor string
	// action is the executed action for regular action reports,
	// unused otherwise.
	action string
	// result indicates whether:
	// - resOk - the action succeeded, or auditor was happy
	// - resErr - the action could not run, or auditor failed to evaluate
	// - resFailure - the action exited with non-zero, or auditor was unhappy
	// - resInfo - the auditor was activated but did not report anything special
	result result
	// output represents
	// - for regular action reports, the one line process exit status
	// - for mood changes, the new mood
	// it is short and oneline so it is suitable for printing out in event graphs
	output string
	// extOutput is stdout/stderr for commands.
	extOutput string
	// failOk is set if the script tolerates a failure for this action.
	failOk bool
}

type result int

const (
	// The following values come back in generated CSV files.
	resOk      result = 0
	resErr            = 1
	resFailure        = 2
	resInfo           = 3
)

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
) (err error) {
	defer func() {
		auditErr := ap.checkAuditViolations()
		// The order of the two arguments here matter: the error
		// collection returns the last error as its cause. We
		// want to keep the original error object as cause.
		err = combineErrors(auditErr, err)
	}()

	of := newOutputFiles()
	defer func() {
		of.CloseAll()
	}()

	t := timeutil.NewTimer()
	t.Reset(time.Second)

	for {
		select {
		case <-ap.stopper.ShouldQuiesce():
			log.Info(ctx, "interrupted")
			return nil

		case <-ctx.Done():
			log.Info(ctx, "canceled")
			return wrapCtxErr(ctx)

		case <-t.C:
			t.Read = true
			t.Reset(time.Second)
			of.Flush()

		case ev := <-actionChan:
			sinceBeginning := ev.startTime
			ap.expandTimeRange(sinceBeginning)

			switch ev.typ {
			case reportMoodChange:
				dataLogger.Logf(ctx, "%.2f mood set: %s", sinceBeginning, ev.output)

			case reportAuditViolation:
				ac, ok := ap.cfg.audience[ev.actor]
				if !ok {
					return errors.Newf("event received for non-existent audience %q: %+v", ev.actor, ev)
				}
				ac.observer.hasData = true

				a, ok := ap.au.auditorStates[ev.actor]
				if !ok {
					return errors.Newf("event received for non-existent auditor: %+v", ev)
				}
				a.hasData = true

				status := int(ev.result)

				dataLogger.Logf(ctx, "%.2f audit check by %s: %v (%q)", sinceBeginning, ev.actor, ev.result, ev.output)
				fName := filepath.Join(ap.cfg.dataDir, fmt.Sprintf("audit-%s.csv", ev.actor))
				w, err := of.getWriter(fName)
				if err != nil {
					return errors.Wrapf(err, "opening %q", fName)
				}

				if ev.output != "" {
					t := html.EscapeString(ev.output)
					fmt.Fprintf(w, "%.4f %d %q\n", sinceBeginning, status, fmt.Sprintf("%s: %s", ev.actor, t))
				} else {
					fmt.Fprintf(w, "%.4f %d\n", sinceBeginning, status)
				}

				if ev.result == resErr || ev.result == resFailure {
					ap.au.auditViolations = append(ap.au.auditViolations,
						auditViolation{
							ts:           sinceBeginning,
							result:       ev.result,
							auditorName:  ev.actor,
							failureState: ev.output,
						})
					if ap.cfg.earlyExit {
						// the defer above will catch the violation.
						return nil
					}
				}

			case reportActionExec:
				a, ok := ap.cfg.actors[ev.actor]
				if !ok {
					return errors.Newf("event received for non-existent actor: %+v", ev)
				}
				a.hasData = true

				status := int(ev.result)

				dataLogger.Logf(ctx, "%.2f action %s:%s (%.4fs)", sinceBeginning, ev.actor, ev.action, ev.duration)

				fName := filepath.Join(ap.cfg.dataDir, fmt.Sprintf("%s.csv", ev.actor))
				w, err := of.getWriter(fName)
				if err != nil {
					return errors.Wrapf(err, "opening %q", fName)
				}

				fmt.Fprintf(w, "%.4f %.4f %s %d %q\n",
					sinceBeginning, ev.duration, ev.action, status, ev.output)

				if ev.result != resOk {
					level := E
					sym := "😞"
					ref := "(see below for details)"
					if ev.failOk {
						level = W
						sym = "🤨"
						ref = "(see log for details)"
					}
					ap.narrate(level, sym,
						"action %s:%s failed %s", ev.actor, ev.action, ref)
				}
			}

		case ev := <-collectorChan:
			ap.expandTimeRange(ev.ts)

			dataLogger.Logf(ctx, "%.2f %s %v", ev.ts, ev.varName.String(), ev.val)

			vr, ok := ap.cfg.vars[ev.varName]
			if !ok {
				return errors.Newf("event received for non-existent variable %q: %+v", ev.varName.String(), ev)
			}

			for _, obsName := range vr.watcherNames {
				a := vr.watchers[obsName]
				a.observer.hasData = true
				if ov, ok := a.observer.obsVars[ev.varName]; ok {
					ov.hasData = true
				}

				fName := filepath.Join(ap.cfg.dataDir,
					csvFileName(obsName, ev.varName.actorName, ev.varName.sigName))

				w, err := of.getWriter(fName)
				if err != nil {
					return errors.Wrapf(err, "opening %q", fName)
				}
				// shuffle is a random value between [-.25, +.25] used to randomize event plots.
				shuffle := (.5 * rand.Float64()) - .25
				if ev.typ == sigTypEvent {
					fmt.Fprintf(w, "%.4f %q %.3f\n", ev.ts, html.EscapeString(ev.val), shuffle)
				} else {
					fmt.Fprintf(w, "%.4f %v %.3f\n", ev.ts, ev.val, shuffle)
				}
			}
		}
	}
	// unreachable
}
