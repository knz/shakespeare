package cmd

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"reflect"
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
) (err error) {
	// Initialize the auditors that depend on the mood and
	// perhaps time.
	auLog.Logf(ctx, "at start")
	if err := ap.processMoodChange(ctx, auLog, actionCh, collectorCh, true /*atBegin*/, false /*atEnd*/, 0, "clear"); err != nil {
		return err
	}

	defer func() {
		auLog.Logf(ctx, "at end")
		if finalErr := ap.checkFinal(ctx, auLog, actionCh, collectorCh); err != nil {
			err = combineErrors(err, finalErr)
		}
	}()

	for {
		select {
		case <-ap.stopper.ShouldQuiesce():
			log.Info(ctx, "terminated")
			return nil

		case <-ctx.Done():
			log.Info(ctx, "interrupted")
			return wrapCtxErr(ctx)

		case ev := <-moodChan:
			if err = ap.collectAndAuditMood(ctx, auLog, actionCh, collectorCh, ev.ts, ev.newMood); err != nil {
				return err
			}

		case ev := <-auChan:
			if err = ap.checkEvent(ctx, auLog, actionCh, collectorCh, ev); err != nil {
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
		deps:     make(map[exprVar]struct{}),
	}

	depVars := make(map[string]struct{})

	for _, v := range e.compiled.Vars() {
		depVars[v] = struct{}{}
	}
	for v := range depVars {
		parts := strings.Split(v, " ")
		if len(parts) == 1 {
			if strings.Contains(v, ".") {
				return expr{}, errors.WithHintf(errors.Newf("invalid syntax: %q", v),
					"try [%s]", strings.ReplaceAll(v, ".", " "))
			}
			if err := checkIdent(v); err != nil {
				return expr{}, err
			}

			varName := exprVar{actorName: "", sigName: v}
			if _, ok := cfg.vars[varName]; !ok {
				return expr{}, explainAlternatives(errors.Newf("variable not defined: %q", varName.String()), "variables", cfg.vars)
			}
			if err := cfg.maybeAddVar(a, varName, true /* dupOk */); err != nil {
				return expr{}, err
			}
			e.deps[varName] = struct{}{}
			continue
		}
		if len(parts) != 2 {
			return expr{}, errors.Newf("invalid signal reference: %q", v)
		}
		if err := checkIdents(parts[0], parts[1]); err != nil {
			return expr{}, err
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
		e.deps[varName] = struct{}{}
	}
	return e, nil
}

func newAudition(cfg *config) *audition {
	au := &audition{
		cfg:           cfg,
		curMood:       "clear",
		curMoodStart:  math.Inf(-1),
		auditorStates: make(map[string]*auditorState, len(cfg.audience)),
		curActivated:  make(map[exprVar]bool),
		curVals:       make(map[string]interface{}),
	}
	for aName, a := range cfg.audience {
		if a.auditor.expectFsm != nil || len(a.auditor.assignments) > 0 {
			au.auditorStates[aName] = &auditorState{}
		}
	}
	for _, v := range cfg.vars {
		au.curActivated[v.name] = false
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

func (ap *app) checkFinal(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	actionCh chan<- actionReport,
	collectorCh chan<- observation,
) error {
	au := ap.au
	// Close the mood chapter, if one was open.
	now := timeutil.Now()
	elapsed := now.Sub(au.epoch).Seconds()
	if au.curMood != "clear" {
		au.moodPeriods = append(au.moodPeriods, moodPeriod{
			startTime: au.curMoodStart,
			endTime:   elapsed,
			mood:      au.curMood,
		})
	}
	atBegin := math.IsInf(au.curMoodStart, 0)
	return ap.processMoodChange(ctx, auLog, actionCh, collectorCh, atBegin, true /*atEnd*/, elapsed, "clear")
}

func (ap *app) collectAndAuditMood(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	actionCh chan<- actionReport,
	collectorCh chan<- observation,
	evtTime float64,
	mood string,
) error {
	au := ap.au
	if mood == au.curMood {
		// No mood change - do nothing.
		return nil
	}
	if au.curMood != "clear" {
		au.moodPeriods = append(au.moodPeriods, moodPeriod{
			startTime: au.curMoodStart,
			endTime:   evtTime,
			mood:      au.curMood,
		})
	}
	atBegin := math.IsInf(au.curMoodStart, 0)
	return ap.processMoodChange(ctx, auLog, actionCh, collectorCh, atBegin, false /*atEnd*/, evtTime, mood)
}

func (ap *app) processMoodChange(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	actionCh chan<- actionReport,
	collectorCh chan<- observation,
	atBegin, atEnd bool,
	ts float64,
	newMood string,
) error {
	if !atBegin {
		auLog.Logf(ctx, "the mood %q is ending", ap.au.curMood)
		// Process the end of the current mood.
		if err := ap.checkEvent(ctx, auLog, actionCh, collectorCh, auditableEvent{ts: ts, values: nil}); err != nil {
			return err
		}
	}
	if atEnd {
		return nil
	}
	ap.au.curMoodStart = ts
	ap.au.curMood = newMood
	auLog.Logf(ctx, "the mood %q is starting", newMood)
	// Process the end of the current mood.
	return ap.checkEvent(ctx, auLog, actionCh, collectorCh, auditableEvent{ts: ts, values: nil})
}

func (ap *app) checkEvent(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	actionCh chan<- actionReport,
	collectorCh chan<- observation,
	ev auditableEvent,
) error {
	ap.au.resetAuditors()
	ap.resetSigVars()
	if err := ap.setAndActivateVar(ctx, auLog, collectorCh, ev.ts, parseScalar, exprVar{sigName: "t"}, ev.ts, false); err != nil {
		return err
	}
	if err := ap.setAndActivateVar(ctx, auLog, collectorCh, ev.ts, parseEvent, exprVar{sigName: "mood"}, ap.au.curMood, false); err != nil {
		return err
	}

	// Update the mood time/duration.
	var moodt float64
	if math.IsInf(ap.au.curMoodStart, 0) {
		moodt = ev.ts
	} else {
		moodt = ev.ts - ap.au.curMoodStart
	}
	if err := ap.setAndActivateVar(ctx, auLog, collectorCh, ev.ts, parseScalar, exprVar{sigName: "moodt"}, moodt, false); err != nil {
		return err
	}

	for _, value := range ev.values {
		if err := ap.setAndActivateVar(ctx, auLog, collectorCh, ev.ts, value.typ, value.varName, value.val, false); err != nil {
			return err
		}
		// FIXME: only collect for activated audiences.
		if err := ap.collectEvent(ctx, collectorCh, ev.ts, value.typ, value.varName, value.val); err != nil {
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
		if err := ap.checkEventForAudience(auCtx, auLog, as, am, collectorCh, actionCh, ev); err != nil {
			return err
		}
	}

	return nil
}

func (ap *app) collectEvent(
	ctx context.Context,
	collectorCh chan<- observation,
	ts float64,
	typ parserType,
	varName exprVar,
	val interface{},
) error {
	vS, ok := val.(string)
	if !ok {
		vS = fmt.Sprintf("%v", val)
	}

	obs := observation{
		typ:     typ,
		ts:      ts,
		varName: varName,
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
	ctx context.Context, auLog *log.SecondaryLogger, curActivated map[exprVar]bool,
) bool {
	for v := range e.deps {
		if !curActivated[v] {
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
	collectorCh chan<- observation,
	actionCh chan<- actionReport,
	ev auditableEvent,
) error {
	// First order of business is to check whether we are auditing at this moment.
	activateCtx := logtags.AddTag(ctx, "activate", nil)
	if !am.auditor.activeCond.hasDeps(activateCtx, auLog, ap.au.curActivated) {
		// Dependencies not satisfied. Nothing to do.
		return nil
	}
	auditing, err := ap.evalBool(activateCtx, auLog, am.name, am.auditor.activeCond, ap.au.curVals)
	if err != nil {
		return err
	}

	// atEnd will change the type of boolean event injected in the check
	// FSM below.
	atEnd := false
	if auditing != as.auditing {
		if auditing {
			ap.judge(activateCtx, I, "", "%s starts auditing", am.name)
			ap.startOfAuditPeriod(activateCtx, auLog, am.name, as, &am.auditor)
		} else {
			// We're on the ending event.
			// We'll mark .auditing to false only at the end below.
			atEnd = true
		}
	}
	if !as.auditing {
		// ap.judge(activateCtx, I, "🙈", "(%s: not auditing)", am.name)
		return nil
	}
	// Now process the assignments.
	if err := ap.processAssignments(ctx, auLog, collectorCh, ev.ts, as, &am.auditor); err != nil {
		return err
	}

	if err := ap.auditCheck(ctx, auLog, ev.ts, am.name, as, &am.auditor, actionCh); err != nil {
		return err
	}

	if atEnd {
		ctx = logtags.AddTag(ctx, "atend", nil)
		if err := ap.auditCheckEnd(ctx, auLog, ev.ts, am.name, as, &am.auditor, actionCh); err != nil {
			return err
		}
		ap.judge(ctx, I, "🙈", "%s stops auditing", am.name)
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
		auLog.Logf(ctx, "start state: %s", as.eval.state())
	}
	// Activate the auditor.
	as.auditing = true
	return nil
}

func (ap *app) processAssignments(
	baseCtx context.Context,
	auLog *log.SecondaryLogger,
	collectorCh chan<- observation,
	evTs float64,
	as *auditorState,
	am *auditor,
) error {
	for _, va := range am.assignments {
		ctx := logtags.AddTag(baseCtx, "assign", va.targetVar)
		if !va.expr.hasDeps(ctx, auLog, ap.au.curActivated) {
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
		typ := parseEvent
		if _, ok := value.(float64); ok {
			typ = parseScalar
		}
		ap.setAndActivateVar(ctx, auLog, collectorCh, evTs, typ, exprVar{sigName: va.targetVar}, value, true)
	}
	return nil
}

func (ap *app) auditCheckEnd(
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

	ev := actionReport{
		typ:       reportAuditViolation,
		startTime: ts,
		actor:     auditorName,
		result:    resErr,
	}
	return ap.processFsmStateChange(ctx, auLog, actionCh, auditorName, as, ev, "end")
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

	ctx = logtags.AddTag(ctx, "expect", nil)

	if !am.expectExpr.hasDeps(ctx, auLog, ap.au.curActivated) {
		// Dependencies not satisfied. Nothing to do.
		return nil
	}

	ev := actionReport{
		typ:       reportAuditViolation,
		startTime: ts,
		actor:     auditorName,
		result:    resErr,
	}

	checkVal, err := ap.evalBool(ctx, auLog, auditorName, am.expectExpr, ap.au.curVals)
	if err != nil {
		ev.output = fmt.Sprintf("%v", err)
		return ap.sendActionReport(ctx, actionCh, ev)
	}

	// We got a result. Determine its truthiness.
	label := "f"
	if checkVal {
		label = "t"
	}

	return ap.processFsmStateChange(ctx, auLog, actionCh, auditorName, as, ev, label)
}

func (ap *app) processFsmStateChange(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	actionCh chan<- actionReport,
	auditorName string,
	as *auditorState,
	ev actionReport,
	label string,
) error {
	// Advance the fsm.
	prevState := as.eval.state()
	as.eval.advance(label)
	newState := as.eval.state()
	auLog.Logf(ctx, "state: %s --%s--> %s", prevState, label, newState)
	ev.output = newState
	switch newState {
	case "good":
		ev.result = resOk
	case "bad":
		ev.result = resFailure
		// Reset the auditor for the next attempt.
		as.eval.advance("reset")
		auLog.Logf(ctx, "state reset: %s", as.eval.state())
	default:
		ev.result = resInfo
	}

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
	}

	// Here the point where we could suspend (not terminate!)
	// the program if there is a violation.
	// Note: we terminate the program in the collector,
	// to ensure that the violation is also saved
	// to the output (eg csv) files.
	return ap.sendActionReport(ctx, actionCh, ev)
}

func (ap *app) sendActionReport(
	ctx context.Context, actionCh chan<- actionReport, ev actionReport,
) error {
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

func (ap *app) resetSigVars() {
	for v := range ap.cfg.vars {
		if v.actorName != "" {
			ap.au.curActivated[v] = false
		}
	}
}

func (au *audition) resetAuditors() {
	for _, as := range au.auditorStates {
		as.activated = false
	}
}

func (ap *app) setAndActivateVar(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	collectorCh chan<- observation,
	evTs float64,
	typ parserType,
	varName exprVar,
	val interface{},
	report bool,
) error {
	if val == nil {
		// No value: do nothing.
	}
	auLog.Logf(ctx, "%s := %v", varName, val)
	varNameS := varName.String()
	prevV := ap.au.curVals[varNameS]
	ap.au.curVals[varNameS] = val
	ap.au.curActivated[varName] = true
	v := ap.cfg.vars[varName]
	if len(v.watchers) > 0 {
		for w := range v.watchers {
			as, ok := ap.au.auditorStates[w]
			if ok && !as.activated {
				auLog.Logf(ctx, "activating auditor %s", w)
				as.activated = true
			}
		}
		if !reflect.DeepEqual(val, prevV) {
			if report {
				ap.judge(ctx, I, "👉", "%s := %v", varName, val)
			}
			if err := ap.collectEvent(ctx, collectorCh, evTs, typ, varName, val); err != nil {
				return err
			}
		}
	}
	return nil
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
