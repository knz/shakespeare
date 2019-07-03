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
	"github.com/knz/shakespeare/pkg/crdb/stop"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

type audition struct {
	r       reporter
	cfg     *config
	stopper *stop.Stopper

	// Where to log audit activity.
	logger *log.SecondaryLogger

	// Where to collect the results used in the final plot.
	res *auditionResults

	// Working area where to maintain the state of the audition.
	st auditionState

	// Where to receive mood changes from.
	// The prompter populates this.
	moodChangeCh <-chan moodChange

	// Where to receive act changes from.
	// The prompter populate this.
	actChangeCh <-chan actChange

	// Where to receive signal data from.
	// The spotlights populate this.
	// This is also closed after all the spotlights are turned off, to
	// indicate the audition should stop gracefully.
	auditCh <-chan auditableEvent

	// Where to send audit results.
	// The collector uses this.
	actionCh chan<- actionReport

	// Where to send observations to.
	// The collector uses this.
	obsCh chan<- observation

	// The audition closes this when
	// the end of the audition is reached.
	// This informs the collector to terminate.
	termCh chan<- struct{}

	// Where the audition should return errors.
	// It also closes this when it terminates.
	errCh chan<- error
}

type auditionState struct {
	// curMood is the current mood.
	curMood string
	// curMoodStart is the time at which the current mode started,
	// relative to the start of the play.
	curMoodStart float64

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
}

type auditionResults struct {
	// moodPeriod describes the starts/end times of previous mood
	// periods. This is used during plotting.
	moodPeriods []moodPeriod
	// actChanges describes the start times of each act. This is
	// used during plotting.
	actChanges []actChange
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

func makeAuditionState(cfg *config) auditionState {
	au := auditionState{
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

func (au *audition) resetSigVars() {
	for v := range au.cfg.vars {
		if v.actorName != "" {
			au.st.curActivated[v] = false
		}
	}
}

func (au *audition) resetAuditors() {
	for _, as := range au.st.auditorStates {
		as.activated = false
	}
}

// audit implements the top level audit loop.
func (au *audition) audit(ctx context.Context) (err error) {
	// Initialize the auditors that depend on the mood and
	// perhaps time.
	au.logger.Logf(ctx, "at start")
	if err := au.processMoodChange(ctx, true /*atBegin*/, false /*atEnd*/, 0, "clear"); err != nil {
		return err
	}

	defer func() {
		ctx = logtags.AddTag(ctx, "audit-end", nil)
		au.logger.Logf(ctx, "at end")
		if finalErr := au.checkFinal(ctx); finalErr != nil {
			err = combineErrors(err, finalErr)
		}
	}()

	for {
		select {
		case <-au.stopper.ShouldQuiesce():
			log.Info(ctx, "terminated")
			return nil

		case <-ctx.Done():
			log.Info(ctx, "interrupted")
			return wrapCtxErr(ctx)

		case ev := <-au.actChangeCh:
			if err = au.collectAndAuditActChange(ctx, ev); err != nil {
				return err
			}

		case ev := <-au.moodChangeCh:
			if err = au.collectAndAuditMood(ctx, ev.ts, ev.newMood); err != nil {
				return err
			}

		case ev := <-au.auditCh:
			if len(ev.values) == 0 {
				// The spotlights have terminated, and are signalling us the
				// end of the audition.
				log.Info(ctx, "terminated by prompter, spotlights shut off")
				return nil
			}
			if err = au.checkEvent(ctx, false /*final*/, ev); err != nil {
				return err
			}
		}
	}
	// unreachable
}

// checkFinal is called at the end of the audition, and performs the
// final iteration of the audit loop.
func (au *audition) checkFinal(ctx context.Context) error {
	// Close the mood chapter, if one was open.
	now := timeutil.Now()
	elapsed := now.Sub(au.r.epoch()).Seconds()
	if au.st.curMood != "clear" {
		au.res.moodPeriods = append(au.res.moodPeriods, moodPeriod{
			startTime: au.st.curMoodStart,
			endTime:   elapsed,
			mood:      au.st.curMood,
		})
	}
	atBegin := math.IsInf(au.st.curMoodStart, 0)
	return au.processMoodChange(ctx, atBegin, true /*atEnd*/, elapsed, "clear")
}

func (au *audition) collectAndAuditActChange(ctx context.Context, ev actChange) error {
	au.res.actChanges = append(au.res.actChanges, ev)
	return nil
}

func (au *audition) collectAndAuditMood(ctx context.Context, evtTime float64, mood string) error {
	if mood == au.st.curMood {
		// No mood change - do nothing.
		return nil
	}
	if au.st.curMood != "clear" {
		au.res.moodPeriods = append(au.res.moodPeriods, moodPeriod{
			startTime: au.st.curMoodStart,
			endTime:   evtTime,
			mood:      au.st.curMood,
		})
	}
	atBegin := math.IsInf(au.st.curMoodStart, 0)
	return au.processMoodChange(ctx, atBegin, false /*atEnd*/, evtTime, mood)
}

func (au *audition) processMoodChange(
	ctx context.Context, atBegin, atEnd bool, ts float64, newMood string,
) error {
	if !atBegin {
		au.logger.Logf(ctx, "the mood %q is ending", au.st.curMood)
		// Process the end of the current mood.
		if err := au.checkEvent(ctx, atEnd, /*final*/
			auditableEvent{ts: ts, values: nil}); err != nil {
			return err
		}
	}
	if atEnd {
		return nil
	}
	au.st.curMoodStart = ts
	au.st.curMood = newMood
	au.logger.Logf(ctx, "the mood %q is starting", newMood)
	// Process the end of the current mood.
	return au.checkEvent(ctx, false, /*final*/
		auditableEvent{ts: ts, values: nil})
}

func (au *audition) checkEvent(ctx context.Context, final bool, ev auditableEvent) error {
	// Mark all auditors as "dormant".
	au.resetAuditors()
	// Mark all signal (non-computed) variables as not activated.
	au.resetSigVars()

	// Assign the two variables t and mood, this wakes up all auditors
	// that depend on them.
	if err := au.setAndActivateVar(ctx, ev.ts, sigTypScalar, varName{sigName: "t"}, ev.ts, false); err != nil {
		return err
	}
	if err := au.setAndActivateVar(ctx, ev.ts, sigTypEvent, varName{sigName: "mood"}, au.st.curMood, false); err != nil {
		return err
	}

	// Update moodt, the mood time/duration.
	var moodt float64
	if math.IsInf(au.st.curMoodStart, 0) {
		moodt = ev.ts
	} else {
		moodt = ev.ts - au.st.curMoodStart
	}
	if err := au.setAndActivateVar(ctx, ev.ts, sigTypScalar, varName{sigName: "moodt"}, moodt, false); err != nil {
		return err
	}

	// Process all received signals.
	for _, value := range ev.values {
		if err := au.setAndActivateVar(ctx, ev.ts, value.typ, value.varName, value.val, false); err != nil {
			return err
		}
		// Also forward the received signals to the collector.
		// FIXME: only collect for activated audiences.
		if err := au.collectEvent(ctx, ev.ts, value.typ, value.varName, value.val); err != nil {
			return err
		}
	}

	// At this point we have performed all the signal assignments. Go
	// through the auditors. This is fundamentally sequential: an
	// auditor may perform some assignments that will wake up later
	// auditors in the loop.
	for _, audienceName := range au.cfg.audienceNames {
		as, ok := au.st.auditorStates[audienceName]
		if !ok || !as.activated {
			// audience still dormant: not interested.
			continue
		}
		am := au.cfg.audience[audienceName]
		auCtx := logtags.AddTag(ctx, "auditor", audienceName)
		if err := au.checkEventForAuditor(auCtx, as, am, final, ev); err != nil {
			return err
		}
	}

	return nil
}

// collectEvent forwards a data point to the collector.
func (au *audition) collectEvent(
	ctx context.Context, ts float64, typ sigType, v varName, val interface{},
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
	case <-au.stopper.ShouldQuiesce():
		log.Info(ctx, "terminated")
		return context.Canceled
	case au.obsCh <- obs:
		// ok
	}
	return nil
}

func (au *audition) checkEventForAuditor(
	ctx context.Context, as *auditorState, am *audienceMember, final bool, ev auditableEvent,
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
		if !au.hasDeps(activateCtx, &am.auditor.activeCond) {
			// Dependencies not satisfied. Nothing to do.
			return nil
		}
		var err error
		auditing, err = au.evalBool(activateCtx, am.name, am.auditor.activeCond)
		if err != nil {
			return err
		}
	}

	// atEnd will change the type of boolean event injected in the check
	// FSM below.
	atEnd := false
	if auditing != as.auditing {
		if auditing {
			au.r.judge(activateCtx, I, "", "%s starts auditing", am.name)
			au.startOfAuditPeriod(activateCtx, am.name, as, &am.auditor)
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
	if err := au.processAssignments(ctx, ev.ts, as, &am.auditor); err != nil {
		return err
	}

	// Process the "expects" clause.
	if err := au.checkExpect(ctx, ev.ts, am.name, as, &am.auditor); err != nil {
		return err
	}

	if atEnd {
		// Auditor finishing its activation period.
		ctx = logtags.AddTag(ctx, "act-period-end", nil)
		if err := au.checkActivationPeriodEnd(ctx, ev.ts, am.name, as, &am.auditor); err != nil {
			return err
		}
		au.r.judge(ctx, I, "🙈", "%s stops auditing", am.name)
		au.logger.Logf(ctx, "audit stopped")
		as.auditing = false
	}
	return nil
}

func (au *audition) startOfAuditPeriod(
	ctx context.Context, auditorName string, as *auditorState, am *auditor,
) error {
	au.logger.Logf(ctx, "audit started")
	// Reset the check FSM, if specified.
	if am.expectFsm != nil {
		as.eval = makeFsmEval(am.expectFsm)
		au.logger.Logf(ctx, "start state: %s", as.eval.state())
	}
	// Activate the auditor.
	as.auditing = true
	return nil
}

func (au *audition) processAssignments(
	baseCtx context.Context, evTs float64, as *auditorState, am *auditor,
) error {
	for _, va := range am.assignments {
		ctx := logtags.AddTag(baseCtx, "assign", va.targetVar)
		if !au.hasDeps(ctx, &va.expr) {
			// Dependencies not satisifed. Skip assignment.
			continue
		}
		value, err := au.evalExpr(ctx, am.name, va.expr)
		if err != nil {
			// FIXME: action event for error
			return errors.Wrapf(err, "assigning variable %s", va.targetVar)
		}
		switch va.assignMode {
		case assignSingle:
			// Nothing to do.
		default:
			// We're aggregating.
			curVal := au.st.curVals[va.targetVar]
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
		au.setAndActivateVar(ctx, evTs, typ, varName{sigName: va.targetVar}, value, true)
	}
	return nil
}

// checkActivationPeriodEnd is called when the activation period ends,
// to process the "end" transition in the expects fsm.
func (au *audition) checkActivationPeriodEnd(
	ctx context.Context, ts float64, auditorName string, as *auditorState, am *auditor,
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
	return au.processFsmStateChange(ctx, auditorName, as, ev, "end")
}

// checkExpect runs one round of the "expects" clause for an auditor.
func (au *audition) checkExpect(
	ctx context.Context, ts float64, auditorName string, as *auditorState, am *auditor,
) error {
	if am.expectFsm == nil {
		// No check expr. Nothing to do.
		return nil
	}

	ctx = logtags.AddTag(ctx, "expect", nil)

	if !au.hasDeps(ctx, &am.expectExpr) {
		// Dependencies not satisfied. Nothing to do.
		return nil
	}

	ev := actionReport{
		typ:       reportAuditViolation,
		startTime: ts,
		actor:     auditorName,
		result:    resErr,
	}

	checkVal, err := au.evalBool(ctx, auditorName, am.expectExpr)
	if err != nil {
		ev.output = fmt.Sprintf("%v", err)
		return au.sendActionReport(ctx, ev)
	}

	// We got a result. Determine its truthiness.
	label := "f"
	if checkVal {
		label = "t"
	}

	return au.processFsmStateChange(ctx, auditorName, as, ev, label)
}

func (au *audition) processFsmStateChange(
	ctx context.Context, auditorName string, as *auditorState, ev actionReport, label string,
) error {
	// Advance the fsm.
	prevState := as.eval.state()
	as.eval.advance(label)
	newState := as.eval.state()
	au.logger.Logf(ctx, "state: %s --%s--> %s", prevState, label, newState)
	ev.output = newState
	switch newState {
	case "good":
		ev.result = resOk
	case "bad":
		ev.result = resFailure
		// Reset the auditor for the next attempt.
		as.eval.advance("reset")
		au.logger.Logf(ctx, "state reset: %s", as.eval.state())
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
		au.r.judge(ctx, res, sym, "%s (%v: %s -> %s)",
			auditorName, label, prevState, newState)
	}

	// Here the point where we could suspend (not terminate!)
	// the program if there is a violation.
	// Note: we terminate the program in the collector,
	// to ensure that the violation is also saved
	// to the output (eg csv) files.
	return au.sendActionReport(ctx, ev)
}

func (au *audition) sendActionReport(ctx context.Context, ev actionReport) error {
	select {
	case <-ctx.Done():
		log.Info(ctx, "interrupted")
		return wrapCtxErr(ctx)
	case <-au.stopper.ShouldQuiesce():
		log.Info(ctx, "terminated")
		return context.Canceled
	case au.actionCh <- ev:
		// ok
	}
	return nil
}

func (au *audition) setAndActivateVar(
	ctx context.Context, evTs float64, typ sigType, vn varName, val interface{}, report bool,
) error {
	if val == nil || val == interface{}(nil) {
		// No value: do nothing.
		return nil
	}
	au.logger.Logf(ctx, "%s := %v", vn, val)
	varNameS := vn.String()
	prevV := au.st.curVals[varNameS]
	au.st.curVals[varNameS] = val
	au.st.curActivated[vn] = true
	v := au.cfg.vars[vn]
	if len(v.watchers) > 0 {
		for w := range v.watchers {
			as, ok := au.st.auditorStates[w]
			if ok && !as.activated {
				au.logger.Logf(ctx, "activating auditor %s", w)
				as.activated = true
			}
		}
		if !reflect.DeepEqual(val, prevV) {
			if report {
				au.r.judge(ctx, I, "👉", "%s := %v", vn, val)
			}
			if err := au.collectEvent(ctx, evTs, typ, vn, val); err != nil {
				return err
			}
		}
	}
	return nil
}

var errAuditViolation = errors.New("audit violation")

func (col *collector) checkAuditViolations() error {
	if len(col.auditViolations) == 0 {
		// No violation, nothing to do.
		return nil
	}

	if !col.auditReported {
		// Avoid printing out the audit errors twice.
		col.auditReported = true
		col.r.narrate(E, "😞", "%d audit violations:", len(col.auditViolations))
		for _, v := range col.auditViolations {
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "%s (at ~%.2fs", v.auditorName, v.ts)
			if v.failureState != "" {
				fmt.Fprintf(&buf, ", %s", v.failureState)
			}
			buf.WriteByte(')')
			col.r.narrate(E, "😿", "%s", buf.String())
		}
	}

	err := errors.Newf("%d audit violations", len(col.auditViolations))
	return errors.Mark(err, errAuditViolation)
}
