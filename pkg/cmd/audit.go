package cmd

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Knetic/govaluate"
	"github.com/cockroachdb/errors"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

type moodChange struct {
	ts      time.Time
	newMood string
}

type auditableEvent struct {
	observation
}

func (ap *app) audit(
	ctx context.Context,
	auChan <-chan auditableEvent,
	actionCh chan<- actionReport,
	moodChan <-chan moodChange,
) error {
	for {
		select {
		case <-ap.stopper.ShouldStop():
			log.Info(ctx, "terminated")
			return nil

		case <-ctx.Done():
			log.Info(ctx, "interrupted")
			return errors.WithStack(ctx.Err())

		case ev := <-moodChan:
			sinceBeginning := ev.ts.Sub(ap.au.epoch).Seconds()
			if err := ap.au.collectAndAuditMood(ctx, sinceBeginning, ev.newMood); err != nil {
				return err
			}

		case ev := <-auChan:
			evVar := exprVar{actorName: ev.actorName, sigName: ev.sigName}
			if err := ap.checkEvent(ctx, actionCh,
				ev.ts, evVar, append(ap.au.alwaysAudit, ev.auditors...), ev.typ, ev.val); err != nil {
				return err
			}

		}
	}
	return nil
}

func (a *audienceMember) checkExpr(cfg *config) error {
	compiledExp, err := govaluate.NewEvaluableExpression(a.auditor.expr)
	if err != nil {
		return err
	}
	a.auditor.compiledExp = compiledExp

	defVars := make(map[string]struct{})
	for _, v := range a.auditor.compiledExp.Vars() {
		if _, ok := predefVars[v]; ok {
			continue
		}
		defVars[v] = struct{}{}
	}
	foundTrigger := false
	for v := range defVars {
		parts := strings.Split(v, ".")
		if len(parts) != 2 {
			return errors.Newf("invalid signal reference: %q", v)
		}
		actorName := parts[0]
		sigName := parts[1]
		actor, ok := cfg.actors[actorName]
		if !ok {
			return errors.Newf("unknown actor %q", actorName)
		}
		a.addOrUpdateSignalSource(actor.role, sigName, actorName)
		actor.addAuditor(sigName, a.name)
		foundTrigger = true
	}
	if !foundTrigger {
		a.auditor.alwaysSensitive = true
	}
	return nil
}

var predefVars = map[string]struct{}{
	"t":     struct{}{},
	"mood":  struct{}{},
	"moodt": struct{}{},
}

func newAudition(cfg *config) *audition {
	au := &audition{
		cfg:           cfg,
		curMood:       "clear",
		curMoodStart:  math.Inf(-1),
		auditorStates: make(map[string]*auditorState, len(cfg.audience)),
		curVals:       make(map[string]interface{}),
		activations:   make(map[exprVar]struct{}),
	}
	for aName, a := range cfg.audience {
		if a.auditor.when != nil && a.auditor.when.name == "always" {
			au.auditorStates[aName] = &auditorState{}
			if a.auditor.alwaysSensitive {
				au.alwaysAudit = append(au.alwaysAudit, aName)
			}
		}
	}
	for actName, act := range cfg.actors {
		for sigName, sink := range act.sinks {
			if len(sink.auditors) > 0 {
				vName := exprVar{actorName: actName, sigName: sigName}
				au.activations[vName] = struct{}{}
			}
		}
	}
	for aVar := range au.activations {
		au.curVals[aVar.Name()] = nil
	}
	au.curVals["mood"] = "clear"
	au.curVals["t"] = float64(0)
	au.curVals["moodt"] = float64(0)
	return au
}

func (au *audition) start(ctx context.Context) {
	au.epoch = timeutil.Now()
}

func (au *audition) checkFinal(ctx context.Context) error {
	// Close the mood chapter, if one was open.
	now := timeutil.Now()
	elapsed := now.Sub(au.epoch).Seconds()
	if au.curMood != "clear" {
		au.moodPeriods = append(au.moodPeriods, moodPeriod{
			startTime: au.curMoodStart,
			endTime:   elapsed,
			mood:      au.curMood,
		})
		return au.collectAndAuditMood(ctx, elapsed, au.curMood)
	}
	return nil
}

func (au *audition) collectAndAuditMood(ctx context.Context, evtTime float64, mood string) error {
	if mood == au.curMood {
		// No mood change - do nothing.
		return nil
	}
	log.Infof(ctx, "the mood changes to %s", mood)
	if au.curMood != "clear" {
		au.moodPeriods = append(au.moodPeriods, moodPeriod{
			startTime: au.curMoodStart,
			endTime:   evtTime,
			mood:      au.curMood,
		})
	}
	au.curMoodStart = evtTime
	au.curMood = mood
	au.curVals["mood"] = mood
	au.curVals["moodt"] = float64(0)
	return nil
}

func (ap *app) checkEvent(
	ctx context.Context,
	actionCh chan<- actionReport,
	evTime time.Time,
	evVar exprVar,
	auditNames []string,
	valTyp parserType,
	val string,
) error {
	elapsed := evTime.Sub(ap.au.epoch).Seconds()
	ap.au.curVals["t"] = elapsed
	varName := evVar.Name()
	if valTyp == parseEvent {
		ap.au.curVals[varName] = val
	} else {
		// scalar or delta
		fVal, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return errors.Wrapf(err, "event for signal %s encounted invalid value %q", varName, val)
		}
		ap.au.curVals[varName] = fVal
	}

	// Update the mood time/duration.
	var moodt float64
	if math.IsInf(ap.au.curMoodStart, 0) {
		moodt = elapsed
	} else {
		moodt = elapsed - ap.au.curMoodStart
	}
	ap.au.curVals["moodt"] = moodt

	for _, auditorName := range auditNames {
		a, ok := ap.au.cfg.audience[auditorName]
		if !ok || a.auditor.when == nil {
			return errors.Newf("event for signal %s triggers non-existent auditor %q", varName, auditorName)
		}

		ev := actionReport{
			typ:       reportAuditViolation,
			startTime: evTime,
			actor:     auditorName,
			success:   true,
		}

		// Make a copy so that an auditor-specific err does not leak to
		// subsequent evaluations.
		value, err := a.auditor.compiledExp.Eval(govaluate.MapParameters(ap.au.curVals))
		log.Infof(ctx, "auditor %s: %s => %v (%v)", auditorName, a.auditor.expr, value, err)
		if b, ok := value.(bool); (!ok || !b) && err == nil {
			ev.success = false
			ap.woops(ctx, "😿  %s (%v)", auditorName, value)
		}
		if err != nil {
			ev.success = false
			ev.output = html.EscapeString(fmt.Sprintf("%v", err))
			ap.woops(ctx, "😿  %s: %v", auditorName, err)
		}

		// Here the point where we could suspend (not terminate!)
		// the program if there is a violation.
		// Note: we terminate the program in the collector,
		// to ensure that the violation is also saved
		// to the output (eg csv) files.

		// Send the report.
		select {
		case <-ctx.Done():
			log.Info(ctx, "interrupted")
			return errors.WithStack(ctx.Err())
		case <-ap.stopper.ShouldStop():
			log.Info(ctx, "terminated")
			return nil
		case actionCh <- ev:
			// ok
		}
	}

	return nil
}

var errAuditViolation = errors.New("audit violation")

func (ap *app) checkAuditViolations() error {
	if len(ap.au.violations) == 0 {
		// No violation, nothing to do.
		return nil
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%d audit violations:\n", len(ap.au.violations))
	comma := ""
	for _, v := range ap.au.violations {
		buf.WriteString(comma)
		comma = "\n"
		fmt.Fprintf(&buf, "😿  %s (at ~%.2fs", v.auditorName, v.ts)
		if v.output != "" {
			fmt.Fprintf(&buf, ", %s", v.output)
		}
		buf.WriteByte(')')
	}
	return errors.Mark(errors.New(buf.String()), errAuditViolation)
}
