package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/knz/shakespeare/pkg/crdb/log"
)

// parseCfg parses a configuration from the given buffered input.
func (cfg *config) parseCfg(ctx context.Context, rd *reader) error {
	topLevelParsers := []struct {
		headerRe *regexp.Regexp
		parseFn  func(line string) error
	}{
		{actorsRe, cfg.parseActors},
		{scriptRe, cfg.parseScript},
		{audienceRe, cfg.parseAudience},
	}
	for {
		line, pos, stop, skip, err := rd.readLine(ctx)
		if err != nil || stop {
			return err
		} else if skip {
			continue
		}

		if roleRe.MatchString(line) {
			// The "role" syntax is special because the role name
			// is listed in the heading line.
			roleName := roleRe.ReplaceAllString(line, "${rolename}")
			extends := strings.TrimSpace(roleRe.ReplaceAllString(line, "${extends}"))
			extends = strings.TrimSpace(strings.TrimPrefix(extends, "extends"))
			if err := cfg.parseRole(ctx, rd, roleName, extends); err != nil {
				// Error is already decoded by parseSection called by parseRole.
				return err
			}
		} else {
			// Every other section can be handled in a generic way.
			foundParser := false
			for _, parser := range topLevelParsers {
				if parser.headerRe.MatchString(line) {
					foundParser = true
					if err := parseSection(ctx, rd, parser.parseFn); err != nil {
						// Error is already decorated by parseSection.
						return err
					}
				}
			}
			if !foundParser {
				return pos.wrapErr(errors.Newf("unknown syntax"))
			}
		}
	}
}

func compileRe(re string) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf("(?s:%s)", re))
}

// parseSection applies the given lineParser to every line inside a section,
// and stops at the "end" keyword.
func parseSection(ctx context.Context, rd *reader, lineParser func(line string) error) error {
	for {
		line, pos, stop, skip, err := rd.readLine(ctx)
		if err != nil || stop {
			return err
		} else if skip {
			continue
		}
		if line == "end" {
			return nil
		}
		if err := lineParser(line); err != nil {
			return pos.wrapErr(err)
		}
	}
}

var audienceRe = compileRe(`^audience$`)
var watchRe = compileRe(`^(?P<name>\S+)\s+watches\s+(?P<target>every\s+\S+|\S+)\s+(?P<signal>\S+)\s*$`)
var watchVarRe = compileRe(`^(?P<name>\S+)\s+watches\s+(?P<varname>\S+)\s*$`)
var measuresRe = compileRe(`^(?P<name>\S+)\s+measures\s+(?P<ylabel>.*)$`)
var activeRe = compileRe(`^(?P<name>\S+)\s+audits\s+(?P<expr>only\s+(?:while|when)\s+.*|throughout)\s*$`)
var collectsRe = compileRe(`^(?P<name>\S+)\s+collects\s+(?P<var>\S+)\s+as\s+(?P<mode>\S+)\s+(?P<N>\d+)\s+(?P<expr>.*)$`)
var computesRe = compileRe(`^(?P<name>\S+)\s+computes\s+(?P<var>\S+)\s+as\s+(?P<expr>.*)$`)
var expectsRe = compileRe(`^(?P<name>\S+)\s+expects\s+(?P<when>[a-z]+[a-z ]*[a-z])\s*:\s*(?P<expr>.*)$`)

func (cfg *config) parseAudience(line string) error {
	if watchRe.MatchString(line) {
		aName := watchRe.ReplaceAllString(line, "${name}")
		target := watchRe.ReplaceAllString(line, "${target}")
		signal := watchRe.ReplaceAllString(line, "${signal}")

		r, foundActors, err := cfg.selectActors(target)
		if err != nil {
			return err
		}
		if len(foundActors) == 0 {
			log.Warningf(context.TODO(), "there is no actor playing role %q for audience %q to watch", r.name, aName)
			return nil
		}
		a := cfg.addOrGetAudienceMember(aName)

		for _, actor := range foundActors {
			actor.addObserver(signal, aName)

			varName := exprVar{actor.name, signal}
			if err := cfg.maybeAddVar(a, varName, true /* dupOk */); err != nil {
				return err
			}
			if err := a.addOrUpdateSignalSource(r, varName); err != nil {
				return err
			}
		}
	} else if watchVarRe.MatchString(line) {
		aName := watchVarRe.ReplaceAllString(line, "${name}")
		target := watchVarRe.ReplaceAllString(line, "${varname}")

		varName := exprVar{actorName: "", sigName: target}
		v, ok := cfg.vars[varName]
		if !ok {
			return explainAlternatives(errors.Newf("variable not defined: %q", varName), "variables", cfg.vars)
		}

		a := cfg.addOrGetAudienceMember(aName)
		v.maybeAddWatcher(a)
		if _, ok := a.observer.obsVars[varName]; !ok {
			a.observer.obsVars[varName] = &collectedSignal{}
			a.observer.obsVarNames = append(a.observer.obsVarNames, varName)
		}
	} else if measuresRe.MatchString(line) {
		aName := measuresRe.ReplaceAllString(line, "${name}")
		ylabel := strings.TrimSpace(measuresRe.ReplaceAllString(line, "${ylabel}"))
		a := cfg.addOrGetAudienceMember(aName)
		a.observer.ylabel = ylabel
	} else if expectsRe.MatchString(line) {
		aName := expectsRe.ReplaceAllString(line, "${name}")
		aWhen := expectsRe.ReplaceAllString(line, "${when}")
		aExpr := strings.TrimSpace(expectsRe.ReplaceAllString(line, "${expr}"))

		a := cfg.addOrGetAudienceMember(aName)

		if err := a.ensureAuditCond(cfg); err != nil {
			return err
		}

		if a.auditor.expectFsm != nil {
			return errors.New("only one 'expects' verb is supported at this point")
		}
		when, err := parseAuditWhen(aWhen)
		if err != nil {
			return err
		}
		a.auditor.expectFsm = when

		exp, err := a.checkExpr(cfg, aExpr)
		if err != nil {
			return err
		}
		a.auditor.expectExpr = exp
	} else if activeRe.MatchString(line) {
		aName := activeRe.ReplaceAllString(line, "${name}")
		aWhen := activeRe.ReplaceAllString(line, "${expr}")

		var expr string
		switch {
		case strings.HasPrefix(aWhen, "only"):
			expr = strings.TrimSpace(strings.TrimPrefix(aWhen, "only"))
			if strings.HasPrefix(expr, "while") {
				expr = strings.TrimSpace(strings.TrimPrefix(expr, "while"))
			} else {
				expr = strings.TrimSpace(strings.TrimPrefix(expr, "when"))
			}
		case strings.HasPrefix(aWhen, "throughout"):
			expr = "true"
		default:
			return errors.Newf("invalid activation period: %q", aWhen)
		}

		a := cfg.addOrGetAudienceMember(aName)
		if a.auditor.activeCond.src != "" {
			return errors.Newf("%q already has an activation period defined (%s)", aName, a.auditor.activeCond.src)
		}

		exp, err := a.checkExpr(cfg, expr)
		if err != nil {
			return err
		}
		a.auditor.activeCond = exp
	} else if collectsRe.MatchString(line) {
		aName := collectsRe.ReplaceAllString(line, "${name}")
		aVar := collectsRe.ReplaceAllString(line, "${var}")
		aMode := collectsRe.ReplaceAllString(line, "${mode}")
		aN := collectsRe.ReplaceAllString(line, "${N}")
		aExpr := collectsRe.ReplaceAllString(line, "${expr}")

		var mode assignMode
		switch aMode {
		case "last":
			mode = assignLastN
		case "first":
			mode = assignFirstN
		case "bottom":
			mode = assignBottomN
		case "top":
			mode = assignTopN
		default:
			return errors.Newf("unknown collection mode: %q", aMode)
		}
		N, err := strconv.Atoi(aN)
		if err != nil {
			return err
		}
		if N < 1 {
			return errors.New("too few values to collect")
		}

		a := cfg.addOrGetAudienceMember(aName)

		if err := a.ensureAuditCond(cfg); err != nil {
			return err
		}

		exp, err := a.checkExpr(cfg, aExpr)
		if err != nil {
			return err
		}

		// Add the variable. This must happen after the check,
		// so that we are not using the variable we are writing to.
		varName := exprVar{sigName: aVar}
		if err := cfg.maybeAddVar(nil, varName, false /*dupOk*/); err != nil {
			return err
		}
		cfg.vars[varName].isArray = true

		a.auditor.assignments = append(a.auditor.assignments, assignment{
			targetVar:  aVar,
			expr:       exp,
			assignMode: mode,
			N:          N,
		})
	} else if computesRe.MatchString(line) {
		aName := computesRe.ReplaceAllString(line, "${name}")
		aVar := computesRe.ReplaceAllString(line, "${var}")
		aExpr := computesRe.ReplaceAllString(line, "${expr}")

		a := cfg.addOrGetAudienceMember(aName)

		if err := a.ensureAuditCond(cfg); err != nil {
			return err
		}

		exp, err := a.checkExpr(cfg, aExpr)
		if err != nil {
			return err
		}

		// Add the variable. This must happen after the check,
		// so that we are not using the variable we are writing to.
		varName := exprVar{sigName: aVar}
		if err := cfg.maybeAddVar(nil, varName, false /*dupOk*/); err != nil {
			return err
		}

		a.auditor.assignments = append(a.auditor.assignments, assignment{
			targetVar:  aVar,
			expr:       exp,
			assignMode: assignSingle,
		})
	} else {
		return errors.New("unknown syntax")
	}

	return nil
}

func (a *audienceMember) ensureAuditCond(cfg *config) error {
	if a.auditor.activeCond.src == "" {
		// Synthetize "throughout".
		compiledExp, err := a.checkExpr(cfg, "true")
		if err != nil {
			return err
		}
		a.auditor.activeCond = compiledExp
	}
	return nil
}

var roleRe = compileRe(`^role\s+(?P<rolename>\w+)(?P<extends>(?:\s+extends\s+\w+)?)\s*$`)
var actionDefRe = compileRe(`^:(?P<actionname>\w+)\s+(?P<cmd>.*)$`)
var spotlightDefRe = compileRe(`^spotlight\s+(?P<cmd>.*)$`)
var cleanupDefRe = compileRe(`^cleanup\s+(?P<cmd>.*)$`)
var parseDefRe = compileRe(`^signal\s+(?P<name>\S+)\s+(?P<type>\S+)\s+at\s+(?P<re>.*)$`)

func (cfg *config) parseRole(
	ctx context.Context, rd *reader, roleName string, extends string,
) error {
	if _, ok := cfg.roles[roleName]; ok {
		return errors.Newf("duplicate role definition: %q", roleName)
	}

	var thisRole *role
	if extends != "" {
		parentRole, ok := cfg.roles[extends]
		if !ok {
			return explainAlternatives(errors.Newf("unknown role: %s", extends), "roles", cfg.roles)
		}
		thisRole = parentRole.clone(roleName)
	} else {
		thisRole = &role{name: roleName, actionCmds: make(map[string]cmd)}
	}

	parserNames := make(map[string]struct{})
	cfg.roles[roleName] = thisRole
	cfg.roleNames = append(cfg.roleNames, roleName)

	return parseSection(ctx, rd, func(line string) error {
		if actionDefRe.MatchString(line) {
			aName := actionDefRe.ReplaceAllString(line, "${actionname}")
			aCmd := actionDefRe.ReplaceAllString(line, "${cmd}")
			if _, ok := thisRole.actionCmds[aName]; ok {
				return errors.Newf("duplicate action name: %q", aName)
			} else {
				thisRole.actionNames = append(thisRole.actionNames, aName)
			}
			thisRole.actionCmds[aName] = cmd(aCmd)
		} else if spotlightDefRe.MatchString(line) {
			thisRole.spotlightCmd = cmd(spotlightDefRe.ReplaceAllString(line, "${cmd}"))
		} else if cleanupDefRe.MatchString(line) {
			thisRole.cleanupCmd = cmd(cleanupDefRe.ReplaceAllString(line, "${cmd}"))
		} else if parseDefRe.MatchString(line) {
			rp := resultParser{}
			reS := parseDefRe.ReplaceAllString(line, "${re}")

			// Expand commonly known time formats.
			reS = strings.Replace(reS, `(?P<ts_rfc3339>)`, `(?P<ts_rfc3339>\d\d\d\d-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)Z)`, 1)
			reS = strings.Replace(reS, `(?P<ts_log>)`, `(?P<ts_log>\d{6} \d\d:\d\d:\d\d\.\d{6})`, 1)

			re, err := regexp.Compile(reS)
			if err != nil {
				return errors.Wrapf(err, "compiling regexp %s", reS)
			}
			rp.re = re
			pname := parseDefRe.ReplaceAllString(line, "${name}")
			if _, ok := parserNames[pname]; ok {
				return errors.Newf("duplicate signal name: %q", pname)
			}
			parserNames[pname] = struct{}{}
			rp.name = pname

			ptype := parseDefRe.ReplaceAllString(line, "${type}")
			switch ptype {
			case "event":
				rp.typ = parseEvent
				if !hasSubexp(re, "event") {
					return errors.New("expected (?P<event>...) in regexp")
				}
			case "scalar":
				rp.typ = parseScalar
				if !hasSubexp(re, "scalar") {
					return errors.New("expected (?P<scalar>...) in regexp")
				}
			case "delta":
				rp.typ = parseDelta
				if !hasSubexp(re, "delta") {
					return errors.New("expected (?P<delta>...) in regexp")
				}
			default:
				return explainAlternativesList(errors.Newf("unknown signal type %q", ptype),
					"signals", "event", "scalar", "delta")
			}

			if hasSubexp(re, "ts_rfc3339") {
				rp.timeLayout = time.RFC3339Nano
				rp.reGroup = "ts_rfc3339"
			} else if hasSubexp(re, "ts_log") {
				const logTimeLayout = "060102 15:04:05.999999"
				rp.timeLayout = logTimeLayout
				rp.reGroup = "ts_log"
			} else if hasSubexp(re, "ts_now") {
				// Special "format": there is actually no timestamp. We'll auto-generate values.
				rp.reGroup = ""
			} else {
				return explainAlternativesList(
					errors.New("unknown or missing time stamp format (?P<ts_...>) in regexp"),
					"formats", "ts_now", "ts_log", "ts_rfc3339")
			}

			thisRole.sigParsers = append(thisRole.sigParsers, &rp)
			thisRole.sigNames = append(thisRole.sigNames, rp.name)
		} else {
			return errors.New("unknown syntax")
		}
		return nil
	})
}

func hasSubexp(re *regexp.Regexp, n string) bool {
	for _, s := range re.SubexpNames() {
		if s == n {
			return true
		}
	}
	return false
}

var actorsRe = compileRe(`^cast$`)
var actorDefRe = compileRe(`^(?P<actorname>\S+)\s+plays\s+(?P<rolename>\w+)\s*(?P<extraenv>\(.*\)|)\s*$`)

func (cfg *config) parseActors(line string) error {
	if actorDefRe.MatchString(line) {
		roleName := actorDefRe.ReplaceAllString(line, "${rolename}")
		actorName := actorDefRe.ReplaceAllString(line, "${actorname}")
		extraEnv := actorDefRe.ReplaceAllString(line, "${extraenv}")

		r, ok := cfg.roles[roleName]
		if !ok {
			return explainAlternatives(errors.Newf("unknown role: %s", roleName), "roles", cfg.roles)
		}

		actorName = strings.TrimSpace(actorName)
		if _, ok := cfg.actors[actorName]; ok {
			return errors.Newf("duplicate actor definition: %q", actorName)
		}

		if extraEnv != "" {
			extraEnv = strings.TrimPrefix(extraEnv, "(")
			extraEnv = strings.TrimSuffix(extraEnv, ")")
		}

		act := actor{
			shellPath: cfg.shellPath,
			name:      actorName,
			role:      r,
			extraEnv:  extraEnv,
			workDir:   filepath.Join(cfg.artifactsDir, actorName),
			sinks:     make(map[string]*sink),
		}
		cfg.actors[actorName] = &act
		cfg.actorNames = append(cfg.actorNames, actorName)
	} else {
		return errors.New("unknown syntax")
	}
	return nil
}

var scriptRe = compileRe(`^script$`)
var tempoRe = compileRe(`^tempo\s+(?P<dur>.*)$`)
var actionRe = compileRe(`^action\s+(?P<char>\S)\s+entails\s+(?P<chardef>.*)$`)
var doRe = compileRe(`^:(?P<action>.*)$`)
var nopRe = compileRe(`^nop$`)
var ambianceRe = compileRe(`^mood\s+(?P<mood>.*)$`)
var stanzaRe = compileRe(`^prompt\s+(?P<target>every\s+\S+|\S+)\s+(?P<stanza>.*)$`)

func (cfg *config) parseScript(line string) error {
	if actionRe.MatchString(line) {
		actShorthand := actionRe.ReplaceAllString(line, "${char}")
		actChar := actShorthand[0]
		actActions := actionRe.ReplaceAllString(line, "${chardef}")
		components := strings.Split(actActions, ";")
		for _, c := range components {
			actAction := strings.TrimSpace(c)

			_, ok := cfg.actions[actChar]
			a := action{name: actShorthand}
			cfg.actions[actChar] = append(cfg.actions[actChar], &a)
			if !ok {
				cfg.actionChars = append(cfg.actionChars, actChar)
			}

			if nopRe.MatchString(actAction) {
				// action . nop
				a.typ = nopAction
			} else if ambianceRe.MatchString(actAction) {
				// action r mood red
				a.typ = ambianceAction
				act := ambianceRe.ReplaceAllString(actAction, "${mood}")
				a.act = strings.TrimSpace(act)
			} else if doRe.MatchString(actAction) {
				// action r :dosomething
				a.typ = doAction
				act := doRe.ReplaceAllString(actAction, "${action}")
				if strings.HasSuffix(act, "?") {
					a.failOk = true
					act = strings.TrimSuffix(act, "?")
				}
				a.act = act
			} else {
				return errors.Newf("unknown action syntax: %s", line)
			}
		}
	} else if tempoRe.MatchString(line) {
		durS := tempoRe.ReplaceAllString(line, "${dur}")
		dur, err := time.ParseDuration(durS)
		if err != nil {
			return errors.Wrap(err, "parsing tempo")
		}
		cfg.tempo = dur
	} else if stanzaRe.MatchString(line) {
		script := stanzaRe.ReplaceAllString(line, "${stanza}")
		target := stanzaRe.ReplaceAllString(line, "${target}")

		r, foundActors, err := cfg.selectActors(target)
		if err != nil {
			return err
		}
		if len(foundActors) == 0 {
			log.Warningf(context.TODO(), "there is no actor playing role %q to play this script: %s", r.name, line)
			return nil
		}

		for i := 0; i < len(script); i++ {
			actionSteps, ok := cfg.actions[script[i]]
			if !ok {
				return explainAlternatives(errors.Newf("script action '%c' not defined", script[i]), "script actions", cfg.actions)
			}
			for _, act := range actionSteps {
				if act.typ != doAction {
					continue
				}
				if _, ok := r.actionCmds[act.act]; !ok {
					return explainAlternatives(
						errors.Newf("action :%s (used in script action '%c') is not defined for role %q", act.act, script[i], r.name),
						fmt.Sprintf("actions for role %s", r.name), r.actionCmds)
				}
			}
		}
		for _, actor := range foundActors {
			cfg.stanzas = append(cfg.stanzas, stanza{
				actor:  actor,
				script: script,
			})
		}
	} else {
		return errors.New("unknown syntax")
	}
	return nil
}

func parseAuditWhen(when string) (*fsm, error) {
	if f, ok := automata[when]; ok {
		return f, nil
	}
	return nil, explainAlternatives(
		errors.Newf("predicate modality %q not recognized", when),
		"modalities", automata)
}

func explainAlternatives(err error, name string, theMap interface{}) error {
	keys := reflect.ValueOf(theMap).MapKeys()
	if len(keys) == 0 {
		return errors.WithHintf(err, "no %s defined yet", name)
	}
	keyS := make([]string, len(keys))
	for i, k := range keys {
		keyS[i] = fmt.Sprintf("%v", k)
	}
	return explainAlternativesList(err, name, keyS...)
}

func explainAlternativesList(err error, name string, values ...string) error {
	sort.Strings(values)
	return errors.WithHintf(err,
		"available %s: %s", name, strings.Join(values, ", "))
}
