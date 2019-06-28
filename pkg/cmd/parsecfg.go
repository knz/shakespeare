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
	"unicode"
	"unicode/utf8"

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

		if title, ok := withPrefix(line, "title "); ok {
			cfg.titleStrings = append(cfg.titleStrings, title)
		} else if att, ok := withPrefix(line, "attention "); ok {
			cfg.seeAlso = append(cfg.seeAlso, att)
		} else if auth, ok := withPrefix(line, "author "); ok {
			cfg.authors = append(cfg.authors, auth)
		} else if p := pw(roleRe); p.m(line) {
			// The "role" syntax is special because the role name
			// is listed in the heading line.
			roleName, err := p.id("rolename")
			if err != nil {
				return pos.wrapErr(err)
			}
			extends := p.get("extends")
			extends, _ = withPrefix(extends, "extends")
			if extends != "" {
				if err := checkIdent(roleName); err != nil {
					return pos.wrapErr(err)
				}
			}

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
var noPlotRe = compileRe(`^(?P<name>\S+)\s+only\s+helps\s*$`)

func (cfg *config) parseAudience(line string) error {
	if p := pw(watchRe); p.m(line) {
		target := p.get("target")
		aName, err := p.id("name")
		if err != nil {
			return err
		}
		signal, err := p.id("signal")
		if err != nil {
			return err
		}

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

			vn := varName{actor.name, signal}
			if err := cfg.maybeAddVar(a, vn, true /* dupOk */); err != nil {
				return err
			}
			if err := a.addOrUpdateSignalSource(r, vn); err != nil {
				return err
			}
		}
	} else if p := pw(watchVarRe); p.m(line) {
		aName, err := p.id("name")
		if err != nil {
			return err
		}
		target, err := p.id("varname")
		if err != nil {
			return err
		}

		vn := varName{actorName: "", sigName: target}
		v, ok := cfg.vars[vn]
		if !ok {
			return explainAlternatives(
				errors.Newf("variable not defined: %q", vn),
				"variables", cfg.vars)
		}

		a := cfg.addOrGetAudienceMember(aName)
		v.maybeAddWatcher(a)
		if _, ok := a.observer.obsVars[vn]; !ok {
			a.observer.obsVars[vn] = &collectedSignal{}
			a.observer.obsVarNames = append(a.observer.obsVarNames, vn)
		}
	} else if p := pw(measuresRe); p.m(line) {
		aName, err := p.id("name")
		if err != nil {
			return err
		}
		ylabel := p.get("ylabel")
		a := cfg.addOrGetAudienceMember(aName)
		a.observer.ylabel = ylabel
	} else if p := pw(noPlotRe); p.m(line) {
		aName, err := p.id("name")
		if err != nil {
			return err
		}
		a := cfg.addOrGetAudienceMember(aName)
		a.observer.disablePlot = true
	} else if p := pw(expectsRe); p.m(line) {
		aWhen := p.get("when")
		aExpr := p.get("expr")
		aName, err := p.id("name")
		if err != nil {
			return err
		}

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
	} else if p := pw(activeRe); p.m(line) {
		aWhen := p.get("expr")
		aName, err := p.id("name")
		if err != nil {
			return err
		}

		var expr string
		if e, ok := withPrefix(aWhen, "only"); ok {
			if wh, ok := withPrefix(e, "while"); ok {
				expr = wh
			} else {
				expr, _ = withPrefix(e, "when")
			}
		} else if strings.HasPrefix(aWhen, "throughout") {
			expr = "true"
		} else {
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
	} else if p := pw(collectsRe); p.m(line) {
		aMode := p.get("mode")
		aN := p.get("N")
		aExpr := p.get("expr")
		aName, err := p.id("name")
		if err != nil {
			return err
		}
		aVar, err := p.id("var")
		if err != nil {
			return err
		}

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
		vn := varName{sigName: aVar}
		if err := cfg.maybeAddVar(nil, vn, false /*dupOk*/); err != nil {
			return err
		}
		cfg.vars[vn].isArray = true

		a.auditor.assignments = append(a.auditor.assignments, assignment{
			targetVar:  aVar,
			expr:       exp,
			assignMode: mode,
			N:          N,
		})
	} else if p := pw(computesRe); p.m(line) {
		aExpr := p.get("expr")
		aName, err := p.id("name")
		if err != nil {
			return err
		}
		aVar, err := p.id("var")
		if err != nil {
			return err
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
		vn := varName{sigName: aVar}
		if err := cfg.maybeAddVar(nil, vn, false /*dupOk*/); err != nil {
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

var roleRe = compileRe(`^role\s+(?P<rolename>\S+)(?P<extends>(?:\s+extends\s+\S+)?)\s*$`)
var actionDefRe = compileRe(`^:(?P<actionname>\S+)\s+(?P<cmd>.*)$`)
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
			return explainAlternatives(
				errors.Newf("unknown role: %s", extends), "roles", cfg.roles)
		}
		thisRole = parentRole.clone(roleName)
	} else {
		thisRole = &role{name: roleName, actionCmds: make(map[string]cmd)}
	}

	parserNames := make(map[string]struct{})
	cfg.roles[roleName] = thisRole
	cfg.roleNames = append(cfg.roleNames, roleName)

	err := parseSection(ctx, rd, func(line string) error {
		if p := pw(actionDefRe); p.m(line) {
			aName, err := p.id("actionname")
			if err != nil {
				return err
			}
			aCmd := p.get("cmd")
			if _, ok := thisRole.actionCmds[aName]; ok {
				return errors.Newf("duplicate action name: %q", aName)
			} else {
				thisRole.actionNames = append(thisRole.actionNames, aName)
			}
			thisRole.actionCmds[aName] = cmd(aCmd)
		} else if p := pw(spotlightDefRe); p.m(line) {
			thisRole.spotlightCmd = cmd(p.get("cmd"))
		} else if p := pw(cleanupDefRe); p.m(line) {
			thisRole.cleanupCmd = cmd(p.get("cmd"))
		} else if p := pw(parseDefRe); p.m(line) {
			pname, err := p.id("name")
			if err != nil {
				return err
			}

			rp := sigParser{}
			reS := p.get("re")

			// Expand commonly known time formats.
			reS = strings.Replace(reS, `(?P<ts_rfc3339>)`, `(?P<ts_rfc3339>\d\d\d\d-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)Z)`, 1)
			reS = strings.Replace(reS, `(?P<ts_log>)`, `(?P<ts_log>\d{6} \d\d:\d\d:\d\d\.\d{6})`, 1)

			re, err := regexp.Compile(reS)
			if err != nil {
				return errors.Wrapf(err, "compiling regexp %s", reS)
			}
			rp.re = re
			if _, ok := parserNames[pname]; ok {
				return errors.Newf("duplicate signal name: %q", pname)
			}
			parserNames[pname] = struct{}{}
			rp.name = pname

			ptype := p.get("type")
			switch ptype {
			case "event":
				rp.typ = sigTypEvent
				if !hasSubexp(re, "event") {
					return errors.New("expected (?P<event>...) in regexp")
				}
			case "scalar":
				rp.typ = sigTypScalar
				if !hasSubexp(re, "scalar") {
					return errors.New("expected (?P<scalar>...) in regexp")
				}
			case "delta":
				rp.typ = sigTypDelta
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
	if err != nil {
		return err
	}

	if len(thisRole.sigNames) > 0 && thisRole.spotlightCmd == "" {
		return errors.New("cannot extract signals without a spotlight")
	}
	return nil
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
var actorDefRe = compileRe(`^(?P<actorname>\S+?)(?P<star>\*?)\s+(?:plays|play\s+(?P<mul>\S+))\s+(?P<rolename>\S+)(?P<extraenv>(?:\s+with\s+.*)?)\s*$`)

func (cfg *config) parseActors(line string) error {
	if p := pw(actorDefRe); p.m(line) {
		roleName, err := p.id("rolename")
		if err != nil {
			return err
		}
		actorName, err := p.id("actorname")
		if err != nil {
			return err
		}

		mul := p.get("mul")

		r, ok := cfg.roles[roleName]
		if !ok && mul != "" {
			// try a little harder, maybe there was a plural.
			roleName = strings.TrimSuffix(roleName, "s")
			r, ok = cfg.roles[roleName]
		}
		if !ok {
			return explainAlternatives(
				errors.Newf("unknown role: %s", roleName), "roles", cfg.roles)
		}

		extraEnv := p.getp("extraenv", "with")

		addOneActor := func(actorName, extraEnv string) error {
			if _, ok := cfg.actors[actorName]; ok {
				return errors.Newf("duplicate actor definition: %q", actorName)
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
			return nil
		}

		if mul == "" {
			if p.get("star") != "" {
				return errors.New("cannot use * with a single actor definition")
			}
			return addOneActor(actorName, extraEnv)
		}
		if p.get("star") == "" {
			return errors.WithHintf(errors.New("must use * in name to define multiple actors"), "try: %s* play ...", actorName)
		}
		n, err := strconv.ParseInt(mul, 10, 0)
		if err != nil {
			return err
		}
		for i := 0; i < int(n); i++ {
			ns := strconv.Itoa(i + 1)
			thisActorName := actorName + ns
			if err != nil {
				return err
			}
			if err := addOneActor(thisActorName, extraEnv); err != nil {
				return err
			}
			cfg.actors[thisActorName].extraAssign = "i=" + ns
		}
	} else {
		return errors.New("unknown syntax")
	}
	return nil
}

var scriptRe = compileRe(`^script$`)
var tempoRe = compileRe(`^tempo\s+(?P<dur>.*)$`)

// V1 rules
var actionRe = compileRe(`^action\s+(?P<char>\S)\s+entails\s+(?P<chardef>.*)$`)
var doRe = compileRe(`^:(?P<action>\S+)$`)
var nopRe = compileRe(`^nop$`)
var ambianceRe = compileRe(`^mood\s+(?P<mood>\S+)$`)
var stanzaRe = compileRe(`^prompt\s+(?P<target>every\s+\S+|\S+)\s+(?P<stanza>.*)$`)

// V2 rules
var entailsRe = compileRe(`scene\s+(?P<char>\S)\s+entails\s+for\s+(?P<target>every\s+\S+|\S+)\s*:\s*(?P<actions>.*)$`)
var moodChangeRe = compileRe(`scene\s+(?P<char>\S)\s+mood\s+(?P<when>starts|ends)\s+(?P<mood>\S+)\s*$`)
var storyLineRe = compileRe(`storyline\s+(?P<storyline>.*)\s*$`)
var editRe = compileRe(`edit\s+(?P<editcmd>.*)$`)

func (cfg *config) parseScript(line string) error {
	if p := pw(actionRe); p.m(line) {
		actShorthand := p.get("char")
		if len(actShorthand) != 1 {
			return errors.New("only ASCII characters allowed as action shorthands")
		}
		actChar := actShorthand[0]
		if !unicode.In(rune(actChar), unicode.L, unicode.N, unicode.P) {
			return errors.New("only ASCII letters, digits and punctuation allowed as action shorthands")
		}

		actActions := p.get("chardef")
		components := strings.Split(actActions, ";")
		for _, c := range components {
			actAction := strings.TrimSpace(c)

			_, ok := cfg.actions[actChar]
			a := action{name: actShorthand}
			cfg.actions[actChar] = append(cfg.actions[actChar], &a)
			if !ok {
				cfg.actionChars = append(cfg.actionChars, actChar)
			}

			if p := pw(nopRe); p.m(actAction) {
				// action . nop
				a.typ = nopAction
			} else if p := pw(ambianceRe); p.m(actAction) {
				// action r mood red
				a.typ = ambianceAction
				mood, err := p.id("mood")
				if err != nil {
					return err
				}
				a.act = mood
			} else if p := pw(doRe); p.m(actAction) {
				// action r :dosomething
				a.typ = doAction
				act := p.get("action")
				if strings.HasSuffix(act, "?") {
					a.failOk = true
					act = strings.TrimSuffix(act, "?")
				}
				if err := checkIdent(act); err != nil {
					return err
				}
				a.act = act
			} else {
				return errors.Newf("unknown action syntax: %s", line)
			}
		}
	} else if p := pw(tempoRe); p.m(line) {
		durS := p.get("dur")
		dur, err := time.ParseDuration(durS)
		if err != nil {
			return errors.Wrap(err, "parsing tempo")
		}
		cfg.tempo = dur
	} else if p := pw(stanzaRe); p.m(line) {
		script := p.get("stanza")
		target := p.get("target")

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
	} else if p := pw(storyLineRe); p.m(line) {
		storyLine := p.get("storyline")
		st, err := cfg.validateStoryLine(storyLine)
		if err != nil {
			return err
		}
		cfg.storyLine = combineStoryLines(cfg.storyLine, st)
		log.Infof(context.Background(), "combined storylines: %s", strings.Join(cfg.storyLine, " "))
	} else if p := pw(editRe); p.m(line) {
		editcmd := p.get("editcmd")
		if len(editcmd) < 4 || editcmd[0] != 's' {
			return errors.WithHint(errors.New("invalid syntax"), "try edit s/.../.../")
		}
		splitChar := editcmd[1:2]
		parts := strings.SplitN(editcmd, splitChar, 4)
		if len(parts) < 4 || (parts[3] != "" && parts[3] != "g") {
			errors.WithHint(errors.New("invalid syntax"), "try edit s/.../.../")
		}
		orig, repl := parts[1], parts[2]
		origRe, err := regexp.Compile(orig)
		if err != nil {
			return errors.Wrapf(err, "compiling regexp %q", orig)
		}
		newStoryLine := origRe.ReplaceAllString(strings.Join(cfg.storyLine, " "), repl)
		st, err := cfg.validateStoryLine(newStoryLine)
		if err != nil {
			return errors.WithDetailf(err, "storyline after edit: %s", newStoryLine)
		}
		cfg.storyLine = st
	} else if p := pw(moodChangeRe); p.m(line) {
		aMood, err := p.id("mood")
		if err != nil {
			return err
		}
		aWhen := p.get("when")
		scShorthand := p.get("char")
		if _, err := validateShorthand(scShorthand); err != nil {
			return err
		}

		sc := cfg.maybeAddSceneSpec(scShorthand)
		if aWhen == "starts" {
			sc.moodStart = aMood
		} else {
			sc.moodEnd = aMood
		}
	} else if p := pw(entailsRe); p.m(line) {
		target := p.get("target")
		actActions := p.get("actions")
		scShorthand := p.get("char")
		if _, err := validateShorthand(scShorthand); err != nil {
			return err
		}

		r, foundActors, err := cfg.selectActors(target)
		if err != nil {
			return err
		}
		if len(foundActors) == 0 {
			log.Warningf(context.TODO(), "there is no actor playing role %q to play this script: %s", r.name, line)
			return nil
		}

		parts := strings.Split(actActions, ";")
		actions := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if strings.HasSuffix(part, "?") {
				part = part[:len(part)-1]
			}
			if _, ok := r.actionCmds[part]; !ok {
				return explainAlternatives(errors.Newf("action :%s is not defined for role %q", part, r.name),
					fmt.Sprintf("actions for role %s", r.name), r.actionCmds)
			}
			actions = append(actions, part)
		}

		sc := cfg.maybeAddSceneSpec(scShorthand)
		for _, actor := range foundActors {
			sc.entails = append(sc.entails, actionGroup{
				actor:   actor,
				actions: actions,
			})
		}
	} else {
		return errors.New("unknown syntax")
	}
	return nil
}

func validateShorthand(scSchorthand string) (byte, error) {
	if len(scSchorthand) != 1 {
		return 0, errors.New("only ASCII characters allowed as scene shorthands")
	}
	scChar := scSchorthand[0]
	if !unicode.In(rune(scChar), unicode.L, unicode.N) {
		return 0, errors.New("only ASCII letters, digits and punctuation allowed as scene shorthands")
	}
	return scChar, nil
}

func parseAuditWhen(when string) (*fsm, error) {
	if f, ok := automata[when]; ok {
		return f, nil
	}
	return nil, explainAlternatives(
		errors.Newf("predicate modality %q not recognized", when),
		"modalities", automata)
}

// explainAlternatives provides a hint to the user with the list
// of possible choices, taken from the keys of the map given as argument.
func explainAlternatives(err error, name string, theMap interface{}) error {
	keys := reflect.ValueOf(theMap).MapKeys()
	keyS := make([]string, len(keys))
	for i, k := range keys {
		keyS[i] = fmt.Sprintf("%v", k)
	}
	return explainAlternativesList(err, name, keyS...)
}

// explainAlternativesList is like explainAlternatives
// but the list of possible values is provided in the argument list.
func explainAlternativesList(err error, name string, values ...string) error {
	if len(values) == 0 {
		return errors.WithHintf(err, "no %s defined yet", name)
	}
	sort.Strings(values)
	return errors.WithHintf(err,
		"available %s: %s", name, strings.Join(values, ", "))
}

func checkIdents(names ...string) error {
	for _, name := range names {
		if err := checkIdent(name); err != nil {
			return err
		}
	}
	return nil
}

// checkIdent verifies that its argument is a valid identifier.
// It proposes an alternative as hint if the identifier is not valid.
// Unicode characters are allowed as long as they are letter- and number-like.
func checkIdent(name string) error {
	if len(name) == 0 {
		return errors.New("no identifier specified")
	}
	if !identRe.MatchString(name) {
		err := errors.Newf("not a valid identifier: %q", name)
		alternativeName := strings.Map(identify, name)
		if r, _ := utf8.DecodeRuneInString(alternativeName); unicode.IsDigit(r) {
			alternativeName = "_" + alternativeName
		}
		err = errors.WithHintf(err, "try this instead: %s", alternativeName)
		return err
	}
	return nil
}

var identRe = compileRe(`^[\p{L}\p{S}\p{M}_][\p{L}\p{S}\p{M}\p{N}_]*$`)

// identify returns its argument if valid in an identifier, otherwise `_`.
func identify(r rune) rune {
	if unicode.In(r, unicode.L, unicode.S, unicode.M, unicode.N) {
		return r
	}
	return '_'
}

type parser struct {
	re      *regexp.Regexp
	curLine string
}

func pw(re *regexp.Regexp) parser {
	return parser{re: re}
}

func (p *parser) m(line string) bool {
	p.curLine = line
	return p.re.MatchString(line)
}

func (p *parser) get(part string) string {
	return strings.TrimSpace(p.re.ReplaceAllString(p.curLine, "${"+part+"}"))
}

func (p *parser) id(part string) (string, error) {
	s := p.get(part)
	return s, checkIdent(s)
}

func (p *parser) getp(part, prefix string) string {
	res := p.get(part)
	return strings.TrimSpace(strings.TrimPrefix(res, prefix))
}

func withPrefix(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, prefix)), true
}

var preprocRe = compileRe(`%\w*%`)

// preprocReplace replaces occurrences of %pvar% by val in str.
func preprocReplace(str, pvar, val string) (string, error) {
	ref := "%" + pvar + "%"
	var err error
	res := preprocRe.ReplaceAllStringFunc(str, func(m string) string {
		switch m {
		case "%%":
			return "%"
		case ref:
			return val
		default:
			err = combineErrors(err, errors.Newf("undefined variable: %s", m))
			return m
		}
	})
	return res, err
}
