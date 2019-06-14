package cmd

import (
	"context"
	"fmt"
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
	ctx context.Context, auChan <-chan auditableEvent, moodChan <-chan moodChange,
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
			sinceBeginning := ev.ts.Sub(ap.au.epoch).Seconds()

			evVar := exprVar{actorName: ev.actorName, sigName: ev.sigName}
			if err := ap.au.checkEvent(ctx, sinceBeginning, evVar, ev.auditors, ev.typ, ev.val); err != nil {
				return err
			}

		}
	}
	return nil
}

func (a *auditor) checkExpr(cfg *config) error {
	compiledExp, err := govaluate.NewEvaluableExpression(a.expr)
	if err != nil {
		return err
	}
	a.compiledExp = compiledExp

	for _, v := range a.compiledExp.Vars() {
		if _, ok := predefVars[v]; ok {
			continue
		}
		parts := strings.Split(v, ".")
		if len(parts) != 2 {
			return fmt.Errorf("invalid signal reference: %q", v)
		}
		actor, ok := cfg.actors[parts[0]]
		if !ok {
			return fmt.Errorf("unknown actor %q", parts[0])
		}
		found := false
		for _, r := range actor.role.resParsers {
			if r.name == parts[1] {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("actor %q's role %q has no signal named %q", parts[0], actor.role.name, parts[1])
		}
		actor.addAuditor(parts[1], a.name)
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
		auditorStates: make(map[string]*auditorState, len(cfg.auditors)),
		curVals:       make(map[string]interface{}),
		activations:   make(map[exprVar]struct{}),
	}
	for aName := range cfg.auditors {
		au.auditorStates[aName] = &auditorState{}
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

func (au *audition) checkEvent(
	ctx context.Context,
	evTime float64,
	evVar exprVar,
	auditNames []string,
	valTyp parserType,
	val string,
) error {
	ev := auditorEvent{evTime: evTime}

	au.curVals["t"] = evTime
	varName := evVar.Name()
	if valTyp == parseEvent {
		au.curVals[varName] = val
	} else {
		// scalar or delta
		fVal, err := strconv.ParseFloat(val, 64)
		if err != nil {
			ev.err = errors.Newf("event for signal %s encounted invalid value %q: %v", varName, val, err)
		} else {
			au.curVals[varName] = fVal
		}
	}

	for _, auditorName := range auditNames {
		a, ok := au.cfg.auditors[auditorName]
		if !ok {
			return errors.Newf("event for signal %s triggers non-existent auditor %q", varName, auditorName)
		}
		as := au.auditorStates[a.name]

		// Make a copy so that an auditor-specific err does not leak to
		// subsequent evaluations.
		auEv := ev
		if auEv.err == nil {
			auEv.value, auEv.err = a.compiledExp.Eval(govaluate.MapParameters(au.curVals))
			log.Infof(ctx, "auditor %s: %s => %v (%v)", auditorName, a.expr, auEv.value, auEv.err)
		}
		as.history = append(as.history, auEv)
	}

	return nil
}
