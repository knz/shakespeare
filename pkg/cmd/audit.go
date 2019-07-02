package cmd

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"reflect"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

type audition struct {
	// curMood is the current mood.
	curMood string
	// curMoodStart is the time at which the current mode started,
	// relative to the start of the play.
	curMoodStart float64
	// moodPeriod describes the starts/end times of previous mood
	// periods. This is used during plotting.
	moodPeriods []moodPeriod
	// actChanges describes the start times of each act. This is
	// used during plotting.
	actChanges []actChange

	// auditorStates is the current state for each auditor.
	auditorStates map[string]*auditorState

	// curActivated indicates, for each variable, whether it was
	// activated (written to) in the current round. Used
	// during dependency analysis to determine whether expressions
	// can be evaluated.
	curActivated map[varName]bool

	// curVals is the current value for every variable.
	// Morally this should be keyed by varName, but the
	// upstream "govaluate" package prefers string keys.
	curVals map[string]interface{}

	// auditViolations retains audit violation events during the audition;
	// this is used to compute final error returns.
	auditViolations []auditViolation
}

// auditViolation describes one audit failure.
type auditViolation struct {
	// Time at which the failure was detected.
	ts float64
	// Type of failure.
	result result
	// Name of the auditor that detected the failure.
	auditorName string
	// failureState is the FSM state at the point the failure was
	// detected.
	failureState string
}

// moodChange describes a mood transition. Produced by the prompter
// and processed in the audit loop.
type moodChange struct {
	ts      float64
	newMood string
}

// actChange describes an act transition. Produced by the prompter and
// processed in the audit loop.
type actChange struct {
	ts float64
	// act number of starting act
	actNum int
}

// auditableEvent is produced by the spotlight upon detecting signals
// by an actor. These events are processed in the audit loop.
// If multiple signals are detected simultaneously, they
// are reported together.
// An empty `values` signal the end of the spotlights (produced
// when the channel from spotlights to auditors is closed).
type auditableEvent struct {
	ts     float64
	values []auditableValue
}

type auditableValue struct {
	typ     sigType
	varName varName
	val     interface{}
}

func newAudition(cfg *config) *audition {
	au := &audition{
		curMood:       "clear",
		curMoodStart:  math.Inf(-1),
		auditorStates: make(map[string]*auditorState, len(cfg.audience)),
		curActivated:  make(map[varName]bool),
		curVals:       make(map[string]interface{}),
	}
	for aName, a := range cfg.audience {
		a.auditor.name = aName
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

func (ap *app) resetSigVars() {
	for v := range ap.cfg.vars {
		if v.actorName != "" {
			ap.theater.au.curActivated[v] = false
		}
	}
}

func (au *audition) resetAuditors() {
	for _, as := range au.auditorStates {
		as.activated = false
	}
}

// audit implements the top level audit loop.
func (ap *app) audit(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	auChan <-chan auditableEvent,
	actionCh chan<- actionReport,
	collectorCh chan<- observation,
	moodChan <-chan moodChange,
	actChan <-chan actChange,
) (err error) {
	// Initialize the auditors that depend on the mood and
	// perhaps time.
	auLog.Logf(ctx, "at start")
	if err := ap.processMoodChange(ctx, auLog, actionCh, collectorCh, true /*atBegin*/, false /*atEnd*/, 0, "clear"); err != nil {
		return err
	}

	defer func() {
		ctx = logtags.AddTag(ctx, "audit-end", nil)
		auLog.Logf(ctx, "at end")
		if finalErr := ap.checkFinal(ctx, auLog, actionCh, collectorCh); finalErr != nil {
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

		case ev := <-actChan:
			if err = ap.collectAndAuditActChange(ctx, auLog, ev); err != nil {
				return err
			}

		case ev := <-moodChan:
			if err = ap.collectAndAuditMood(ctx, auLog, actionCh, collectorCh, ev.ts, ev.newMood); err != nil {
				return err
			}

		case ev := <-auChan:
			if len(ev.values) == 0 {
				// The spotlights have terminated, and are signalling us the
				// end of the audition.
				log.Info(ctx, "terminated by prompter, spotlights shut off")
				return nil
			}
			if err = ap.checkEvent(ctx, auLog, actionCh, collectorCh, false /*final*/, ev); err != nil {
				return err
			}
		}
	}
	// unreachable
}

// checkFinal is called at the end of the audition, and performs the
// final iteration of the audit loop.
// FIXME: this will *not* be called in most cases:
// usually when the prompter terminates the audit loop is canceled
// and so this point is never reached. This should be fixed
// by injecting a "final check" event from the prompter into
// the audit channel.
func (ap *app) checkFinal(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	actionCh chan<- actionReport,
	collectorCh chan<- observation,
) error {
	au := ap.theater.au
	// Close the mood chapter, if one was open.
	now := timeutil.Now()
	elapsed := now.Sub(ap.epoch()).Seconds()
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

func (ap *app) collectAndAuditActChange(
	ctx context.Context, auLog *log.SecondaryLogger, ev actChange,
) error {
	ap.theater.au.actChanges = append(ap.theater.au.actChanges, ev)
	return nil
}

func (ap *app) collectAndAuditMood(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	actionCh chan<- actionReport,
	collectorCh chan<- observation,
	evtTime float64,
	mood string,
) error {
	au := ap.theater.au
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
		auLog.Logf(ctx, "the mood %q is ending", ap.theater.au.curMood)
		// Process the end of the current mood.
		if err := ap.checkEvent(ctx, auLog, actionCh, collectorCh, atEnd /*final*/, auditableEvent{ts: ts, values: nil}); err != nil {
			return err
		}
	}
	if atEnd {
		return nil
	}
	ap.theater.au.curMoodStart = ts
	ap.theater.au.curMood = newMood
	auLog.Logf(ctx, "the mood %q is starting", newMood)
	// Process the end of the current mood.
	return ap.checkEvent(ctx, auLog, actionCh, collectorCh, false /*final*/, auditableEvent{ts: ts, values: nil})
}

func (ap *app) checkEvent(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	actionCh chan<- actionReport,
	collectorCh chan<- observation,
	final bool,
	ev auditableEvent,
) error {
	// Mark all auditors as "dormant".
	ap.theater.au.resetAuditors()
	// Mark all signal (non-computed) variables as not activated.
	ap.resetSigVars()

	// Assign the two variables t and mood, this wakes up all auditors
	// that depend on them.
	if err := ap.setAndActivateVar(ctx, auLog, collectorCh, ev.ts, sigTypScalar, varName{sigName: "t"}, ev.ts, false); err != nil {
		return err
	}
	if err := ap.setAndActivateVar(ctx, auLog, collectorCh, ev.ts, sigTypEvent, varName{sigName: "mood"}, ap.theater.au.curMood, false); err != nil {
		return err
	}

	// Update moodt, the mood time/duration.
	var moodt float64
	if math.IsInf(ap.theater.au.curMoodStart, 0) {
		moodt = ev.ts
	} else {
		moodt = ev.ts - ap.theater.au.curMoodStart
	}
	if err := ap.setAndActivateVar(ctx, auLog, collectorCh, ev.ts, sigTypScalar, varName{sigName: "moodt"}, moodt, false); err != nil {
		return err
	}

	// Process all received signals.
	for _, value := range ev.values {
		if err := ap.setAndActivateVar(ctx, auLog, collectorCh, ev.ts, value.typ, value.varName, value.val, false); err != nil {
			return err
		}
		// Also forward the received signals to the collector.
		// FIXME: only collect for activated audiences.
		if err := ap.collectEvent(ctx, collectorCh, ev.ts, value.typ, value.varName, value.val); err != nil {
			return err
		}
	}

	// At this point we have performed all the signal assignments. Go
	// through the auditors. This is fundamentally sequential: an
	// auditor may perform some assignments that will wake up later
	// auditors in the loop.
	for _, audienceName := range ap.cfg.audienceNames {
		as, ok := ap.theater.au.auditorStates[audienceName]
		if !ok || !as.activated {
			// audience still dormant: not interested.
			continue
		}
		am := ap.cfg.audience[audienceName]
		auCtx := logtags.AddTag(ctx, "auditor", audienceName)
		if err := ap.checkEventForAuditor(auCtx, auLog, as, am, collectorCh, actionCh, final, ev); err != nil {
			return err
		}
	}

	return nil
}

// collectEvent forwards a data point to the collector.
func (ap *app) collectEvent(
	ctx context.Context,
	collectorCh chan<- observation,
	ts float64,
	typ sigType,
	v varName,
	val interface{},
) error {
	vS, ok := val.(string)
	if !ok {
		vS = fmt.Sprintf("%v", val)
	}

	obs := observation{
		typ:     typ,
		ts:      ts,
		varName: v,
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

func (ap *app) checkEventForAuditor(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	as *auditorState,
	am *audienceMember,
	collectorCh chan<- observation,
	actionCh chan<- actionReport,
	final bool,
	ev auditableEvent,
) error {
	activateCtx := logtags.AddTag(ctx, "activate", nil)

	var auditing bool
	if final {
		// When we are shutting down the audit, we do not consider what
		// they auditor thinks about whether they want to be active, and
		// we make them inactive no matter what.
		auditing = false
	} else {
		// First order of business is to check whether we are auditing at this moment.
		if !am.auditor.activeCond.hasDeps(activateCtx, auLog, ap.theater.au.curActivated) {
			// Dependencies not satisfied. Nothing to do.
			return nil
		}
		var err error
		auditing, err = ap.evalBool(activateCtx, auLog, am.name, am.auditor.activeCond, ap.theater.au.curVals)
		if err != nil {
			return err
		}
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

	// Process the "expects" clause.
	if err := ap.checkExpect(ctx, auLog, ev.ts, am.name, as, &am.auditor, actionCh); err != nil {
		return err
	}

	if atEnd {
		// Auditor finishing its activation period.
		ctx = logtags.AddTag(ctx, "act-period-end", nil)
		if err := ap.checkActivationPeriodEnd(ctx, auLog, ev.ts, am.name, as, &am.auditor, actionCh); err != nil {
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
		if !va.expr.hasDeps(ctx, auLog, ap.theater.au.curActivated) {
			// Dependencies not satisifed. Skip assignment.
			continue
		}
		value, err := ap.evalExpr(ctx, auLog, am.name, va.expr, ap.theater.au.curVals)
		if err != nil {
			// FIXME: action event for error
			return errors.Wrapf(err, "assigning variable %s", va.targetVar)
		}
		switch va.assignMode {
		case assignSingle:
			// Nothing to do.
		default:
			// We're aggregating.
			curVal := ap.theater.au.curVals[va.targetVar]
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
		typ := sigTypEvent
		if _, ok := value.(float64); ok {
			typ = sigTypScalar
		}
		ap.setAndActivateVar(ctx, auLog, collectorCh, evTs, typ, varName{sigName: va.targetVar}, value, true)
	}
	return nil
}

// checkActivationPeriodEnd is called when the activation period ends,
// to process the "end" transition in the expects fsm.
func (ap *app) checkActivationPeriodEnd(
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

// checkExpect runs one round of the "expects" clause for an auditor.
func (ap *app) checkExpect(
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

	if !am.expectExpr.hasDeps(ctx, auLog, ap.theater.au.curActivated) {
		// Dependencies not satisfied. Nothing to do.
		return nil
	}

	ev := actionReport{
		typ:       reportAuditViolation,
		startTime: ts,
		actor:     auditorName,
		result:    resErr,
	}

	checkVal, err := ap.evalBool(ctx, auLog, auditorName, am.expectExpr, ap.theater.au.curVals)
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

func (ap *app) setAndActivateVar(
	ctx context.Context,
	auLog *log.SecondaryLogger,
	collectorCh chan<- observation,
	evTs float64,
	typ sigType,
	vn varName,
	val interface{},
	report bool,
) error {
	if val == nil || val == interface{}(nil) {
		// No value: do nothing.
		return nil
	}
	auLog.Logf(ctx, "%s := %v", vn, val)
	varNameS := vn.String()
	prevV := ap.theater.au.curVals[varNameS]
	ap.theater.au.curVals[varNameS] = val
	ap.theater.au.curActivated[vn] = true
	v := ap.cfg.vars[vn]
	if len(v.watchers) > 0 {
		for w := range v.watchers {
			as, ok := ap.theater.au.auditorStates[w]
			if ok && !as.activated {
				auLog.Logf(ctx, "activating auditor %s", w)
				as.activated = true
			}
		}
		if !reflect.DeepEqual(val, prevV) {
			if report {
				ap.judge(ctx, I, "👉", "%s := %v", vn, val)
			}
			if err := ap.collectEvent(ctx, collectorCh, evTs, typ, vn, val); err != nil {
				return err
			}
		}
	}
	return nil
}

var errAuditViolation = errors.New("audit violation")

func (ap *app) checkAuditViolations() error {
	if len(ap.theater.au.auditViolations) == 0 {
		// No violation, nothing to do.
		return nil
	}

	if !ap.auditReported {
		// Avoid printing out the audit errors twice.
		ap.auditReported = true
		ap.narrate(E, "😞", "%d audit violations:", len(ap.theater.au.auditViolations))
		for _, v := range ap.theater.au.auditViolations {
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "%s (at ~%.2fs", v.auditorName, v.ts)
			if v.failureState != "" {
				fmt.Fprintf(&buf, ", %s", v.failureState)
			}
			buf.WriteByte(')')
			ap.narrate(E, "😿", "%s", buf.String())
		}
	}

	err := errors.Newf("%d audit violations", len(ap.theater.au.auditViolations))
	return errors.Mark(err, errAuditViolation)
}
