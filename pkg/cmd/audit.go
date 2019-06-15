package cmd

import (
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
			log.Info(ctx, "interrupted")
			return nil

		case <-ctx.Done():
			log.Info(ctx, "canceled")
			return ctx.Err()

		case ev := <-moodChan:
			sinceBeginning := ev.ts.Sub(ap.au.epoch).Seconds()
			if err := ap.au.collectAndAuditMood(ctx, sinceBeginning, ev.newMood); err != nil {
				return err
			}

		case ev := <-auChan:
			evVar := exprVar{actorName: ev.actorName, sigName: ev.sigName}
			if err := ap.checkEvent(ctx, actionCh,
				ev.ts, evVar, ev.auditors, ev.typ, ev.val); err != nil {
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
	for v := range defVars {
		parts := strings.Split(v, ".")
		if len(parts) != 2 {
			return fmt.Errorf("invalid signal reference: %q", v)
		}
		actorName := parts[0]
		sigName := parts[1]
		actor, ok := cfg.actors[actorName]
		if !ok {
			return fmt.Errorf("unknown actor %q", actorName)
		}
		a.addOrUpdateSignalSource(actor.role, sigName, actorName)
		actor.addAuditor(sigName, a.name)
	}
	return nil
}

var predefVars = map[string]struct{}{
	"t":    struct{}{},
	"mood": struct{}{},
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
		if a.auditor.when != auditNone {
			au.auditorStates[aName] = &auditorState{}
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
	ap.au.curVals["t"] = evTime
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

	for _, auditorName := range auditNames {
		a, ok := ap.au.cfg.audience[auditorName]
		if !ok || a.auditor.when == auditNone {
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

		// Send the report.
		select {
		case <-ctx.Done():
			log.Infof(ctx, "interrupted")
			return ctx.Err()
		case <-ap.stopper.ShouldStop():
			log.Infof(ctx, "terminated")
			return nil
		case actionCh <- ev:
			// ok
		}
	}

	return nil
}
