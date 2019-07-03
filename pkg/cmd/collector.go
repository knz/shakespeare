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
	"github.com/knz/shakespeare/pkg/crdb/stop"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

// The workspace for the data collector
type collector struct {
	r       reporter
	cfg     *config
	stopper *stop.Stopper

	// Where to log collected data.
	logger *log.SecondaryLogger

	// Where to receive events from.
	// The prompter and auditors populate this.
	eventCh <-chan collectorEvent

	// Where the collector should return errors.
	// It also closes this when it terminates.
	errCh chan<- error

	// auditViolations retains audit violation events during the audition;
	// this is used to compute final error returns.
	auditViolations []auditViolation

	// auditReported is set to true after a violation was reported
	// through the reporter at least once. This is to avoid duplicate
	// outputs.
	auditReported bool
}

type collectorEvent interface {
	collectorEvent()
}

var _ collectorEvent = terminate{}
var _ collectorEvent = (*observation)(nil)
var _ collectorEvent = (*actionReport)(nil)
var _ collectorEvent = (*moodChange)(nil)
var _ collectorEvent = (*auditionReport)(nil)

func (*moodChange) collectorEvent() {}
func (terminate) collectorEvent()   {}

type observation struct {
	ts      float64
	typ     sigType
	varName varName
	val     string
}

func (*observation) collectorEvent() {}

type actionReport struct {
	startTime float64
	// duration is the duration of the command (in seconds).
	duration float64
	// actor is the actor that ran the action.
	actor string
	// action is the executed action.
	action string
	// result indicates whether:
	// - resOk - the action succeeded
	// - resErr - the action could not run
	// - resFailure - the action exited with non-zero
	result result
	// output encodes the one line process exit status.
	output string
	// extOutput is stdout/stderr for commands.
	extOutput string
	// failOk is set if the script tolerates a failure for this action.
	failOk bool
}

func (*actionReport) collectorEvent() {}

type auditionReport struct {
	ts      float64
	auditor string
	// result indicates whether:
	// - resOk - auditor was happy
	// - resErr - auditor failed to evaluate
	// - resFailure - auditor was unhappy
	// - resInfo - the auditor was activated but did not report anything special
	result result
	// output indicates:
	// - for resErr, the detail of the evaluation error
	// - otherwise, the new FSM state
	output string
}

func (*auditionReport) collectorEvent() {}

type result int

const (
	// The following values come back in generated CSV files.
	resOk      result = 0
	resErr            = 1
	resFailure        = 2
	resInfo           = 3
)

func csvFileName(observerName, actorName, sigName string) string {
	return fmt.Sprintf("%s.%s.%s.csv", observerName, actorName, sigName)
}

func (col *collector) collect(ctx context.Context) (err error) {
	defer func() {
		auditErr := col.checkAuditViolations()
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
		case <-col.stopper.ShouldQuiesce():
			log.Info(ctx, "interrupted")
			return nil

		case <-ctx.Done():
			log.Info(ctx, "canceled")
			return wrapCtxErr(ctx)

		case <-t.C:
			t.Read = true
			t.Reset(time.Second)
			of.Flush()

		case cev := <-col.eventCh:
			switch ev := cev.(type) {
			case terminate:
				// The audit loop has completed. No more work to do.
				log.Info(ctx, "terminated by audience, auditors have exited")
				return nil

			case *actionReport:
				if err := col.collectActionReport(ctx, of, ev); err != nil {
					return err
				}

			case *auditionReport:
				if earlyExit, err := col.collectAuditionReport(ctx, of, ev); earlyExit || err != nil {
					return err
				}

			case *moodChange:
				if err := col.collectMoodChange(ctx, of, ev); err != nil {
					return err
				}

			case *observation:
				if err := col.collectObservation(ctx, of, ev); err != nil {
					return err
				}
			}
		}
	}
	// unreachable
}

func (col *collector) collectObservation(
	ctx context.Context, of *outputFiles, ev *observation,
) error {
	col.r.expandTimeRange(ev.ts)

	col.logger.Logf(ctx, "%.2f %s %v", ev.ts, ev.varName.String(), ev.val)

	vr, ok := col.cfg.vars[ev.varName]
	if !ok {
		return errors.Newf("event received for non-existent variable %q: %+v", ev.varName.String(), ev)
	}

	for _, obsName := range vr.watcherNames {
		a := vr.watchers[obsName]
		if ov, ok := a.observer.obsVars[ev.varName]; ok {
			a.observer.hasData = true
			ov.hasData = true
		}

		fName := filepath.Join(col.cfg.dataDir,
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
	return nil
}

func (col *collector) collectMoodChange(
	ctx context.Context, of *outputFiles, ev *moodChange,
) error {
	col.r.expandTimeRange(ev.ts)
	col.logger.Logf(ctx, "%.2f mood set: %s", ev.ts, ev.newMood)
	return nil
}

func (col *collector) collectAuditionReport(
	ctx context.Context, of *outputFiles, ev *auditionReport,
) (earlyExit bool, err error) {
	col.r.expandTimeRange(ev.ts)

	ac, ok := col.cfg.audience[ev.auditor]
	if !ok {
		return true, errors.Newf("event received for non-existent audience %q: %+v", ev.auditor, ev)
	}
	ac.observer.hasData = true

	ac.auditor.hasData = true

	status := int(ev.result)

	col.logger.Logf(ctx, "%.2f audit check by %s: %v (%q)", ev.ts, ev.auditor, ev.result, ev.output)
	fName := filepath.Join(col.cfg.dataDir, fmt.Sprintf("audit-%s.csv", ev.auditor))
	w, err := of.getWriter(fName)
	if err != nil {
		return true, errors.Wrapf(err, "opening %q", fName)
	}

	if ev.output != "" {
		t := html.EscapeString(ev.output)
		fmt.Fprintf(w, "%.4f %d %q\n", ev.ts, status, fmt.Sprintf("%s: %s", ev.auditor, t))
	} else {
		fmt.Fprintf(w, "%.4f %d\n", ev.ts, status)
	}

	if ev.result == resErr || ev.result == resFailure {
		col.auditViolations = append(col.auditViolations,
			auditViolation{
				ts:           ev.ts,
				result:       ev.result,
				auditorName:  ev.auditor,
				failureState: ev.output,
			})
		if col.cfg.earlyExit {
			// the defer above will catch the violation.
			return true, nil
		}
	}

	return false, nil
}

func (col *collector) collectActionReport(
	ctx context.Context, of *outputFiles, ev *actionReport,
) error {
	sinceBeginning := ev.startTime
	col.r.expandTimeRange(sinceBeginning)

	a, ok := col.cfg.actors[ev.actor]
	if !ok {
		return errors.Newf("event received for non-existent actor: %+v", ev)
	}
	a.hasData = true

	status := int(ev.result)

	col.logger.Logf(ctx, "%.2f action %s:%s (%.4fs)", sinceBeginning, ev.actor, ev.action, ev.duration)

	fName := filepath.Join(col.cfg.dataDir, fmt.Sprintf("%s.csv", ev.actor))
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
		col.r.narrate(level, sym,
			"action %s:%s failed %s", ev.actor, ev.action, ref)
	}

	return nil
}
