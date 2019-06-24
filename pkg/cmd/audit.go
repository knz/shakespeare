package cmd

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/Knetic/govaluate"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

type moodChange struct {
	ts      float64
	newMood string
}

type auditableEvent struct {
	ts     float64
	values []auditableValue
}

type auditableValue struct {
	typ     parserType
	varName exprVar
	val     interface{}
}

func (ap *app) audit(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	auChan <-chan auditableEvent,
	actionCh chan<- actionReport,
	collectorCh chan<- observation,
	moodChan <-chan moodChange,
) error {
	for {
		select {
		case <-ap.stopper.ShouldQuiesce():
			log.Info(ctx, "terminated")
			return nil

		case <-ctx.Done():
			log.Info(ctx, "interrupted")
			return wrapCtxErr(ctx)

		case ev := <-moodChan:
			if err := ap.au.collectAndAuditMood(ctx, ev.ts, ev.newMood); err != nil {
				return err
			}

		case ev := <-auChan:
			if err := ap.checkEvent(ctx, auLog, actionCh, collectorCh, ev); err != nil {
				return err
			}
		}
	}
	// unreachable
}

func (a *audienceMember) checkExpr(cfg *config, expSrc string) (expr, error) {
	compiledExp, err := govaluate.NewEvaluableExpressionWithFunctions(expSrc, evalFunctions)
	if err != nil {
		return expr{}, err
	}
	e := expr{
		src:      expSrc,
		compiled: compiledExp,
		deps:     make(map[string]struct{}),
	}

	for _, v := range e.compiled.Vars() {
		e.deps[v] = struct{}{}
	}
	for v := range e.deps {
		parts := strings.Split(v, ".")
		if len(parts) == 1 {
			varName := exprVar{actorName: "", sigName: v}
			if _, ok := cfg.vars[varName]; !ok {
				return expr{}, explainAlternatives(errors.Newf("variable not defined: %q", varName.String()), "variables", cfg.vars)
			}
			if err := cfg.maybeAddVar(a, varName, true /* dupOk */); err != nil {
				return expr{}, err
			}
			continue
		}
		if len(parts) != 2 {
			return expr{}, errors.Newf("invalid signal reference: %q", v)
		}
		varName := exprVar{actorName: parts[0], sigName: parts[1]}
		actor, ok := cfg.actors[varName.actorName]
		if !ok {
			return expr{}, explainAlternatives(errors.Newf("unknown actor %q", varName.actorName), "actors", cfg.actors)
		}
		if err := a.addOrUpdateSignalSource(actor.role, varName); err != nil {
			return expr{}, err
		}
		if err := cfg.maybeAddVar(a, varName, true /* dupOk */); err != nil {
			return expr{}, err
		}
		actor.addObserver(varName.sigName, a.name)
	}
	return e, nil
}

func newAudition(cfg *config) *audition {
	au := &audition{
		cfg:           cfg,
		curMood:       "clear",
		curMoodStart:  math.Inf(-1),
		auditorStates: make(map[string]*auditorState, len(cfg.audience)),
		curVals:       make(map[string]interface{}),
	}
	for aName, a := range cfg.audience {
		if a.auditor.expectFsm != nil {
			au.auditorStates[aName] = &auditorState{}
		}
	}
	au.curVals["mood"] = "clear"
	au.curVals["t"] = float64(0)
	au.curVals["moodt"] = float64(0)
	for _, v := range cfg.vars {
		varNameS := v.name.String()
		if _, ok := au.curVals[varNameS]; ok {
			// Already has a value. Do nothing.
			continue
		}
		if v.isArray {
			au.curVals[varNameS] = []interface{}{}
		} else {
			au.curVals[varNameS] = nil
		}
	}
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
	// FIXME: also inject final event in FSMs
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
	auLog *log.SecondaryLogger,
	actionCh chan<- actionReport,
	collectorCh chan<- observation,
	ev auditableEvent,
) error {
	ap.au.resetAuditors()
	elapsed := ev.ts
	ap.setAndActivateVar(ctx, auLog, exprVar{sigName: "t"}, elapsed)

	// Update the mood time/duration.
	var moodt float64
	if math.IsInf(ap.au.curMoodStart, 0) {
		moodt = elapsed
	} else {
		moodt = elapsed - ap.au.curMoodStart
	}
	ap.setAndActivateVar(ctx, auLog, exprVar{sigName: "moodt"}, moodt)

	for _, value := range ev.values {
		ap.setAndActivateVar(ctx, auLog, value.varName, value.val)
		// FIXME: only collect for activated audiences.
		if err := ap.collectEvent(ctx, collectorCh, ev.ts, value); err != nil {
			return err
		}
	}

	for _, audienceName := range ap.cfg.audienceNames {
		as, ok := ap.au.auditorStates[audienceName]
		if !ok || !as.activated {
			// audience not interested.
			continue
		}
		am := ap.cfg.audience[audienceName]
		auCtx := logtags.AddTag(ctx, "auditor", audienceName)
		if err := ap.checkEventForAudience(auCtx, auLog, as, am, actionCh, ev); err != nil {
			return err
		}
	}

	return nil
}

func (ap *app) collectEvent(
	ctx context.Context, collectorCh chan<- observation, ts float64, value auditableValue,
) error {
	vS, ok := value.val.(string)
	if !ok {
		vS = fmt.Sprintf("%v", value.val)
	}

	obs := observation{
		typ:     value.typ,
		ts:      ts,
		varName: value.varName,
		val:     vS,
	}

	select {
	case <-ctx.Done():
		log.Info(ctx, "interrupted")
		return wrapCtxErr(ctx)
	case <-ap.stopper.ShouldQuiesce():
		log.Info(ctx, "terminated")
		return context.Canceled
	case collectorCh <- obs:
		// ok
	}
	return nil
}

func (e *expr) hasDeps(
	ctx context.Context, auLog *log.SecondaryLogger, curVals map[string]interface{},
) bool {
	for v := range e.deps {
		d := curVals[v]
		if d == nil {
			auLog.Logf(ctx, "%s: dependency not satisfied: %s", e.src, v)
			return false
		}
	}
	return true
}

func (ap *app) checkEventForAudience(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	as *auditorState,
	am *audienceMember,
	actionCh chan<- actionReport,
	ev auditableEvent,
) error {
	// First order of business is to check whether we are auditing at this moment.
	if !am.auditor.activeCond.hasDeps(ctx, auLog, ap.au.curVals) {
		// Dependencies not satisfied. Nothing to do.
		return nil
	}
	auditing, err := ap.evalBool(ctx, auLog, am.name, am.auditor.activeCond, ap.au.curVals)
	if err != nil {
		return err
	}

	// atEnd will change the type of boolean event injected in the check
	// FSM below.
	atEnd := false
	if auditing != as.auditing {
		if auditing {
			ap.judge(ctx, I, "", "(%s: auditing)", am.name)
			ap.startOfAuditPeriod(ctx, auLog, am.name, as, &am.auditor)
		} else {
			// We're on the ending event.
			// We'll mark .auditing to false only at the end below.
			atEnd = true
		}
	}
	if !as.auditing {
		ap.judge(ctx, I, "🙈", "(%s: not auditing)", am.name)
		return nil
	}
	// Now process the assignments.
	if err := ap.processAssignments(ctx, auLog, as, &am.auditor); err != nil {
		return err
	}

	// FIXME: produce end events.
	if err := ap.auditCheck(ctx, auLog, ev.ts, am.name, as, &am.auditor, actionCh); err != nil {
		return err
	}

	if atEnd {
		ap.judge(ctx, I, "🙈", "(%s: stops auditing)", am.name)
		auLog.Logf(ctx, "audit stopped")
		as.auditing = false
	}
	return nil
}

func (ap *app) startOfAuditPeriod(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	auditorName string,
	as *auditorState,
	am *auditor,
) error {
	auLog.Logf(ctx, "audit started")
	// Reset the check FSM, if specified.
	if am.expectFsm != nil {
		as.eval = makeFsmEval(am.expectFsm)
	}
	// Activate the auditor.
	as.auditing = true
	return nil
}

func (ap *app) processAssignments(
	ctx context.Context, auLog *log.SecondaryLogger, as *auditorState, am *auditor,
) error {
	for _, va := range am.assignments {
		if !va.expr.hasDeps(ctx, auLog, ap.au.curVals) {
			// Dependencies not satisifed. Skip assignment.
			continue
		}
		value, err := evalExpr(ctx, auLog, va.expr, ap.au.curVals)
		if err != nil {
			// FIXME: action event for error
			return errors.Wrapf(err, "assigning variable %s", va.targetVar)
		}
		switch va.assignMode {
		case assignSingle:
			// Nothing to do.
		default:
			// We're aggregating.
			curVal := ap.au.curVals[va.targetVar]
			curArray, ok := curVal.([]interface{})
			if !ok {
				if curVal != nil {
					curArray = append(curArray, curVal)
				}
			}
			collectFn := collectFns[va.assignMode]
			newA, err := collectFn(curArray, va.N, value)
			if err != nil {
				return errors.Wrapf(err, "aggregating variable %s", va.targetVar)
			}
			value = newA
		}
		ap.setAndActivateVar(ctx, auLog, exprVar{sigName: va.targetVar}, value)
	}
	return nil
}

func (ap *app) auditCheck(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	ts float64,
	auditorName string,
	as *auditorState,
	am *auditor,
	actionCh chan<- actionReport,
) error {
	if am.expectFsm == nil {
		// No check expr. Nothing to do.
		return nil
	}

	if !am.expectExpr.hasDeps(ctx, auLog, ap.au.curVals) {
		// Dependencies not satisfied. Nothing to do.
		return nil
	}

	ev := actionReport{
		typ:       reportAuditViolation,
		startTime: ts,
		actor:     auditorName,
		result:    resErr,
	}
	skipReport := false

	checkVal, err := ap.evalBool(ctx, auLog, auditorName, am.expectExpr, ap.au.curVals)
	if err != nil {
		ev.output = fmt.Sprintf("%v", err)
	} else {
		// We got a result. Determine its truthiness.
		label := "f"
		if checkVal {
			label = "t"
		}

		// Advance the fsm.
		prevState := as.eval.state()
		as.eval.advance(label)
		newState := as.eval.state()
		ev.output = newState
		switch newState {
		case "good":
			ev.result = resOk
		case "bad":
			ev.result = resFailure
			// Reset the auditor for the next attempt.
			as.eval.advance("reset")
		default:
			ev.result = resInfo
		}
		auLog.Logf(ctx, "state -> %s", auditorName, newState)

		// Report the change to the user,
		if prevState != newState {
			var res urgency
			var sym string
			switch newState {
			case "good":
				res, sym = G, "😺"
			case "bad":
				res, sym = E, "😿"
			default:
				res = I
			}
			ap.judge(ctx, res, sym, "%s (%v: %s -> %s)",
				auditorName, label, prevState, newState)
		} else {
			skipReport = true
		}
	}

	// Here the point where we could suspend (not terminate!)
	// the program if there is a violation.
	// Note: we terminate the program in the collector,
	// to ensure that the violation is also saved
	// to the output (eg csv) files.

	// If there was an error, or we changed state, report
	// the change.
	if !skipReport {
		select {
		case <-ctx.Done():
			log.Info(ctx, "interrupted")
			return wrapCtxErr(ctx)
		case <-ap.stopper.ShouldQuiesce():
			log.Info(ctx, "terminated")
			return context.Canceled
		case actionCh <- ev:
			// ok
		}
	}
	return nil
}

func (ap *app) evalBool(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	auditorName string,
	expr expr,
	vars map[string]interface{},
) (bool, error) {
	value, err := evalExpr(ctx, auLog, expr, ap.au.curVals)
	if err != nil {
		ap.judge(ctx, E, "🙀", "%s: %v", auditorName, err)
		return false, err
	}
	if b, ok := value.(bool); ok && b {
		return true, nil
	}
	return false, nil
}

func evalExpr(
	ctx context.Context, auLog *log.SecondaryLogger, expr expr, vars map[string]interface{},
) (interface{}, error) {
	value, err := expr.compiled.Eval(govaluate.MapParameters(vars))
	auLog.Logf(ctx, "variables %+v; %s => %v (%v)", vars, expr.src, value, err)
	return value, errors.WithStack(err)
}

func (au *audition) resetAuditors() {
	for _, as := range au.auditorStates {
		as.activated = false
	}
}

func (ap *app) setAndActivateVar(
	ctx context.Context, auLog *log.SecondaryLogger, varName exprVar, val interface{},
) {
	auLog.Logf(ctx, "%s := %v", varName, val)
	ap.au.curVals[varName.String()] = val
	v := ap.cfg.vars[varName]
	for w := range v.watchers {
		auLog.Logf(ctx, "activating auditor %s", w)
		ap.au.auditorStates[w].activated = true
	}
}

var errAuditViolation = errors.New("audit violation")

func (ap *app) checkAuditViolations() error {
	if len(ap.au.auditViolations) == 0 {
		// No violation, nothing to do.
		return nil
	}

	if !ap.auditReported {
		// Avoid printing out the audit errors twice.
		ap.auditReported = true
		ap.narrate(E, "😞", "%d audit violations:", len(ap.au.auditViolations))
		for _, v := range ap.au.auditViolations {
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "%s (at ~%.2fs", v.auditorName, v.ts)
			if v.output != "" {
				fmt.Fprintf(&buf, ", %s", v.output)
			}
			buf.WriteByte(')')
			ap.narrate(E, "😿", "%s", buf.String())
		}
	}

	err := errors.Newf("%d audit violations", len(ap.au.auditViolations))
	return errors.Mark(err, errAuditViolation)
}
