package cmd

import (
	"context"
	"strings"

	"github.com/Knetic/govaluate"
	"github.com/cockroachdb/errors"
)

// expr is a boolean or scalar expression used in
// audits/computes/collects/expects clauses.
type expr struct {
	src      string
	compiled *govaluate.EvaluableExpression
	deps     map[varName]struct{}
}

// checkExpr is used during configuration parsing, to analyze,
// validate and compile a govaluate expression. It also extracts the
// variable dependencies of the expression.
func (a *audienceMember) checkExpr(cfg *config, expSrc string) (expr, error) {
	var err error
	expSrc, err = cfg.preprocReplace(expSrc)
	if err != nil {
		return expr{}, err
	}

	compiledExp, err := govaluate.NewEvaluableExpressionWithFunctions(expSrc, evalFunctions)
	if err != nil {
		return expr{}, err
	}
	e := expr{
		src:      expSrc,
		compiled: compiledExp,
		deps:     make(map[varName]struct{}),
	}

	depVars := make(map[string]struct{})

	for _, v := range e.compiled.Vars() {
		depVars[v] = struct{}{}
	}
	for v := range depVars {
		parts := strings.Split(v, " ")
		if len(parts) == 1 {
			// A single identifier.
			if strings.Contains(v, ".") {
				// Common mistake: a.b  when [a b] was meant. Suggest an alternative as hint.
				return expr{}, errors.WithHintf(
					errors.Newf("invalid syntax: %q", v),
					"try [%s]", strings.ReplaceAll(v, ".", " "))
			}
			if err := checkIdent(v); err != nil {
				return expr{}, err
			}

			vn := varName{actorName: "", sigName: v}
			if _, ok := cfg.vars[vn]; !ok {
				return expr{}, explainAlternatives(
					errors.Newf("variable not defined: %q", vn.String()),
					"variables", cfg.vars)
			}
			if err := cfg.maybeAddVar(a, vn, true /* dupOk */); err != nil {
				return expr{}, err
			}
			e.deps[vn] = struct{}{}
			continue
		}

		// At this point we only support [a b].
		if len(parts) != 2 {
			return expr{}, errors.Newf("invalid signal reference: %q", v)
		}
		if err := checkIdents(parts[0], parts[1]); err != nil {
			return expr{}, err
		}

		vn := varName{actorName: parts[0], sigName: parts[1]}
		actor, ok := cfg.actors[vn.actorName]
		if !ok {
			return expr{}, explainAlternatives(
				errors.Newf("unknown actor %q", vn.actorName), "actors", cfg.actors)
		}
		if err := a.addOrUpdateSignalSource(actor.role, vn); err != nil {
			return expr{}, err
		}
		if err := cfg.maybeAddVar(a, vn, true /* dupOk */); err != nil {
			return expr{}, err
		}
		actor.addObserver(vn.sigName, a.name)
		e.deps[vn] = struct{}{}
	}
	return e, nil
}

// hasDeps return true when all the dependencies of an expression are satisified.
func (au *audition) hasDeps(ctx context.Context, e *expr) bool {
	for v := range e.deps {
		if !au.st.curActivated[v] {
			au.logger.Logf(ctx, "%s: dependency not satisfied: %s", e.src, v)
			return false
		}
	}
	return true
}

func (au *audition) evalBool(ctx context.Context, auditorName string, expr expr) (bool, error) {
	value, err := au.evalExpr(ctx, auditorName, expr)
	if err != nil {
		return false, err
	}
	if b, ok := value.(bool); ok && b {
		return true, nil
	}
	return false, nil
}

func (au *audition) evalExpr(
	ctx context.Context, auditorName string, expr expr,
) (interface{}, error) {
	value, err := expr.compiled.Eval(govaluate.MapParameters(au.st.curVals))
	au.logger.Logf(ctx, "variables %+v; %s => %v (%v)", au.st.curVals, expr.src, value, err)
	if err != nil {
		au.r.judge(ctx, E, "🙀", "%s: %v", auditorName, err)
	}
	return value, errors.WithStack(err)
}
