package cmd

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cockroachdb/errors"
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
		{interpretationRe, cfg.parseInterpretation},
	}
	for {
		line, pos, stop, skip, err := rd.readLine(ctx, cfg)
		if err != nil || stop {
			return err
		} else if skip {
			continue
		}

		if title, ok := withPrefix(line, "title "); ok {
			title, err = cfg.preprocReplace(title)
			if err != nil {
				return pos.wrapErr(err)
			}
			cfg.titleStrings = append(cfg.titleStrings, title)
		} else if att, ok := withPrefix(line, "attention "); ok {
			att, err = cfg.preprocReplace(att)
			if err != nil {
				return pos.wrapErr(err)
			}
			cfg.seeAlso = append(cfg.seeAlso, att)
		} else if auth, ok := withPrefix(line, "author "); ok {
			// Author strings are not modified by preprocessing!
			cfg.authors = append(cfg.authors, auth)
		} else if p := pw(paramRe); p.m(line) {
			name, err := p.id("name")
			if err != nil {
				return pos.wrapErr(err)
			}
			val := p.get("val")
			if _, ok := cfg.pVars[name]; !ok {
				cfg.pVars[name] = val
				cfg.pVarNames = append(cfg.pVarNames, name)
			}
		} else if p := pw(roleRe); p.m(line) {
			// The "role" syntax is special because the role name
			// is listed in the heading line.
			roleName, err := p.idp("rolename", cfg)
			if err != nil {
				return pos.wrapErr(err)
			}
			extends := p.get("extends")
			extends, _ = withPrefix(extends, "extends")
			extends, err = cfg.preprocReplace(extends)
			if err != nil {
				return pos.wrapErr(err)
			}
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
					if err := cfg.parseSection(ctx, rd, parser.parseFn); err != nil {
						// Error is already decorated by parseSection.
						return err
					}
				}
			}
			if !foundParser {
				return pos.wrapErr(
					errors.WithHint(errors.Newf("unknown syntax"),
						`supported syntax:
  # add a title string
  title a midsummer's dream

  # add an author
  author shakespeare

  # add an informative reference
  attention this is fiction

  # include other file
  include filename.cfg

  # define preprocessing variable "season" and default its value to "summer"
  parameter season defaults to summer

  # role definition
  role doctor ... end

  # role surgeon can do all that doctor does and more
  role surgeon extends doctor ... end

  # cast definition(s)
  cast ... end

  # script definition(s)
  script ... end

  # audience definition(s)
  audience ... end

  # interpretation(s)
  interpretation ... end
`))
			}
		}
	}
}

func compileRe(re string) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf("(?s:%s)", re))
}

var paramRe = compileRe(`^parameter\s+(?P<name>\S+)\s+defaults\s+to\s+(?P<val>.*)$`)

// parseSection applies the given lineParser to every line inside a section,
// and stops at the "end" keyword.
func (cfg *config) parseSection(
	ctx context.Context, rd *reader, lineParser func(line string) error,
) error {
	for {
		line, pos, stop, skip, err := rd.readLine(ctx, cfg)
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

var interpretationRe = compileRe(`^interpretation$`)
var ignoreRe = compileRe(`^ignore\s+(?P<result>disappointment|satisfaction)$`)
var foulRe = compileRe(`^(?P<mode>ignore|foul\s+upon|require)\s+(?P<target>\S+)\s+(?P<result>disappointment|satisfaction)$`)

func (cfg *config) parseInterpretation(line string) error {
	if p := pw(ignoreRe); p.m(line) {
		aResult := p.get("result")

		for _, a := range cfg.audience {
			switch aResult {
			case "disappointment":
				a.auditor.foulOnBad = foulIgnore
			case "satisfaction":
				a.auditor.foulOnGood = foulIgnore
			}
		}
	} else if p := pw(foulRe); p.m(line) {
		aMode := p.get("mode")
		aTarget, err := p.id("target")
		if err != nil {
			return err
		}
		aResult := p.get("result")

		a, ok := cfg.audience[aTarget]
		if !ok {
			return errors.Newf("audience member %q not defined", aTarget)
		}

		var f foulCondition
		switch {
		case aMode == "ignore":
			f = foulIgnore
		case strings.HasPrefix(aMode, "foul"):
			f = foulUponNonZero
		case aMode == "require":
			f = foulUponZero
		}

		switch aResult {
		case "disappointment":
			a.auditor.foulOnBad = f
		case "satisfaction":
			a.auditor.foulOnGood = f
		}
	} else {
		return errors.WithHint(errors.New("unknown syntax"),
			`supported syntax:
  # ignore all disappointments from candice
  ignore candice disappointment

  # ignore any disappointment whatsoever
  ignore disappointment

  # require candice to be disappointed; the play is fouled if candice
  # does not get disappointed.
  require candice disappointment

  # foul the play if david becomes satisfied.
  foul upon david satisfaction
`)
	}
	return nil
}

var audienceRe = compileRe(`^audience$`)
var watchRe = compileRe(`^(?P<name>\S+)\s+watches\s+(?P<target>every\s+\S+|\S+)\s+(?P<signal>\S+)\s*$`)
var watchVarRe = compileRe(`^(?P<name>\S+)\s+watches\s+(?P<varname>\S+)\s*$`)
var measuresRe = compileRe(`^(?P<name>\S+)\s+measures\s+(?P<ylabel>.*)$`)
var activeRe = compileRe(`^(?P<name>\S+)\s+audits\s+(?P<expr>only\s+(?:while|when)\s+.*|throughout)\s*$`)
var collectsRe = compileRe(`^(?P<name>\S+)\s+collects\s+(?P<var>\S+)\s+as\s+(?P<mode>\S+)\s+(?P<N>\d+)\s+(?P<expr>.*)$`)
var computesRe = compileRe(`^(?P<name>\S+)\s+computes\s+(?P<var>\S+)\s+as\s+(?P<expr>.*)$`)
var expectsRe = compileRe(`^(?P<name>\S+)\s+expects\s+(?P<when>[a-z]+[a-z ]*[a-z])\s*:\s*(?P<expr>.*)$`)
var expectsSameRe = compileRe(`^(?P<name>\S+)\s+expects\s+like\s+(?P<target>.*)$`)
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
			fmt.Fprintf(os.Stderr,
				"warning: there is no actor playing role %q for audience %q to watch\n",
				r.name, aName)
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
	} else if p := pw(expectsSameRe); p.m(line) {
		aName, err := p.id("name")
		if err != nil {
			return err
		}
		aTarget, err := p.id("target")
		if err != nil {
			return err
		}

		tg, ok := cfg.audience[aTarget]
		if !ok {
			return errors.Newf("auditor %q not defined", aTarget)
		}
		if tg.auditor.expectFsm == nil {
			return errors.Newf("audience member %q does not expect anything", aTarget)
		}

		a := cfg.addOrGetAudienceMember(aName)

		if a.auditor.expectFsm != nil {
			return errors.New("only one 'expects' verb is supported at this point")
		}
		if a.auditor.activeCond.src == "" {
			exp, err := a.checkExpr(cfg, tg.auditor.activeCond.src)
			if err != nil {
				return err
			}
			a.auditor.activeCond = exp
		}
		a.auditor.expectExpr = tg.auditor.expectExpr
		a.auditor.expectFsm = tg.auditor.expectFsm
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
		return errors.WithHint(errors.New("unknown syntax"),
			`supported syntax:
  # observer david observes feelgood events from doctors
  # (and produces a plot of the observations at the end)
  david watches every doctor feelgood

  # auditor "candice" only pays attention when the mood is blue
  candice audits only while mood == 'blue'

  # auditor "candice" becomes disappointed if forced to pay attention for more than 5 seconds
  candice expects always: t < 5

  # auditor "candice" collects the last 5 feelgood events from "bob" into "bin"
  candice collects bin as last 5 [bob feelgood]

  # auditor "candice" remembers the last time measurement as "last_t"
  candice computes last_t as t

  # auditor "candice" does not produce a final plot
  candice only helps

  # observer "beth" watches computed variable last_t
  beth watches last_t

  # the plot y-label for observer "beth" is "time"
  beth measures time

  # auditor "martin" expects the same as "candice"
  martin expects like candice
`)
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

	err := cfg.parseSection(ctx, rd, func(line string) error {
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
			reS = strings.Replace(reS, `(?P<ts_deltasecs>)`, `(?P<ts_deltasecs>(?:\d+(?:\.\d+)?|\.\d+))`, 1)

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
			} else if hasSubexp(re, "ts_deltasecs") {
				// Relative seconds.
				rp.reGroup = "ts_deltasecs"
			} else {
				return explainAlternativesList(
					errors.New("unknown or missing time stamp format (?P<ts_...>) in regexp"),
					"formats", "ts_now", "ts_log", "ts_rfc3339", "ts_deltasecs")
			}

			thisRole.sigParsers = append(thisRole.sigParsers, &rp)
			thisRole.sigNames = append(thisRole.sigNames, rp.name)
		} else {
			return errors.WithHint(errors.New("unknown syntax"),
				`supported syntax:
  # action "cure" is performed by running the command "echo medecine >actions.log"
  :cure echo medecine >>actions.log

  # command "echo sterilize >>actions.log" is ran once at the beginning,
  # and also once at the end.
  cleanup echo sterilize >>actions.log

  # command "tail -F actions.log" is ran in the background to collect data points
  spotlight tail -F actions.log

  # occurrences of "medecine" in the spotlight data are
  # translated to "feelbetter" signals. (uses regular expressions)
  signal feelbetter event at (?P<ts_now>)(?P<event>medecine)
`)
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
		roleName, err := p.idp("rolename", cfg)
		if err != nil {
			return err
		}
		actorName, err := p.id("actorname")
		if err != nil {
			return err
		}

		mul := p.get("mul")
		mul, err = cfg.preprocReplace(mul)
		if err != nil {
			return err
		}

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
		extraEnv, err = cfg.preprocReplace(extraEnv)
		if err != nil {
			return err
		}

		addOneActor := func(actorName, extraEnv string) error {
			if _, ok := cfg.actors[actorName]; ok {
				return errors.Newf("duplicate actor definition: %q", actorName)
			}

			act := actor{
				shellPath:     cfg.shellPath,
				name:          actorName,
				role:          r,
				extraEnv:      extraEnv,
				workDir:       cfg.actorArtifactDirName(actorName),
				sinks:         make(map[string]*sink),
				actionScripts: make(map[string]string),
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
			thisActorName := actorName + strconv.Itoa(i+1)
			if err != nil {
				return err
			}
			if err := addOneActor(thisActorName, extraEnv); err != nil {
				return err
			}
			a := cfg.actors[thisActorName]
			extraVar := fmt.Sprintf("i=%d", i)
			if a.extraEnv != "" {
				a.extraEnv = extraVar + "; " + a.extraEnv
			} else {
				a.extraEnv = extraVar
			}
		}
	} else {
		return errors.WithHint(errors.New("unknown syntax"),
			`supported syntax:
  # actor "bob" plays role "doctor"
  bob plays doctor

  # actor "bob" plays "doctor" and provides shell environment "patient=alice" to commands
  bob plays doctor with patient=alice

  # actors "bob1" and "bob2" play "doctor"
  bob* play 2 doctors
`)
	}
	return nil
}

var scriptRe = compileRe(`^script$`)
var tempoRe = compileRe(`^tempo\s+(?P<dur>.*)$`)
var repeatCountRe = compileRe(`^repeat\s+(?P<count>\S+)\s+times$`)
var repeatAlwaysRe = compileRe(`^repeat\s+always$`)
var repeatTimeoutRe = compileRe(`^repeat\s+time\s+(?P<dur>.*)$`)

// V2 rules
var entailsRe = compileRe(`^scene\s+(?P<char>\S)\s+entails\s+for\s+(?P<target>every\s+\S+|\S+)\s*:\s*(?P<actions>.*)$`)
var moodChangeRe = compileRe(`^scene\s+(?P<char>\S)\s+mood\s+(?P<when>starts|ends)\s+(?P<mood>\S+)\s*$`)
var storyLineRe = compileRe(`^storyline\s+(?P<storyline>.*)\s*$`)
var editRe = compileRe(`^edit\s+(?P<editcmd>.*)$`)
var repeatRe = compileRe(`^repeat\s+from\s+(?P<repeat>.*)$`)

func (cfg *config) parseScript(line string) error {
	var err error
	if p := pw(tempoRe); p.m(line) {
		durS := p.get("dur")
		dur, err := time.ParseDuration(durS)
		if err != nil {
			return errors.Wrap(err, "parsing tempo")
		}
		cfg.tempo = dur
	} else if p := pw(repeatAlwaysRe); p.m(line) {
		cfg.repeatCount = -1
	} else if p := pw(repeatCountRe); p.m(line) {
		count := p.get("count")

		count, err = cfg.preprocReplace(count)
		if err != nil {
			return err
		}

		n, err := strconv.Atoi(count)
		if err != nil {
			return err
		}
		cfg.repeatCount = n
	} else if p := pw(repeatTimeoutRe); p.m(line) {
		dur := p.get("dur")
		dur, err := cfg.preprocReplace(dur)
		if err != nil {
			return err
		}

		if dur == "unconstrained" {
			cfg.repeatTimeout = -1
		} else {
			d, err := time.ParseDuration(dur)
			if err != nil {
				return err
			}
			cfg.repeatTimeout = d
		}
	} else if p := pw(repeatRe); p.m(line) {
		repeat := p.get("repeat")
		re, err := regexp.Compile(repeat)
		if err != nil {
			return err
		}
		cfg.repeatFrom = re
		cfg.updateRepeat()
	} else if p := pw(storyLineRe); p.m(line) {
		storyLine := p.get("storyline")
		st, err := cfg.validateStoryLine(storyLine)
		if err != nil {
			return err
		}
		cfg.storyLine = combineStoryLines(cfg.storyLine, st)
		cfg.updateRepeat()
		// log.Infof(context.Background(), "combined storylines: %s", strings.Join(cfg.storyLine, " "))
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
		cfg.updateRepeat()
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
			fmt.Fprintf(os.Stderr,
				"warning: there is no actor playing role %q to play this script: %s\n",
				r.name, line)
			return nil
		}

		parts := strings.Split(actActions, ";")
		actions := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			checkAct := part
			if strings.HasSuffix(part, "?") {
				checkAct = part[:len(part)-1]
			}
			if _, ok := r.actionCmds[checkAct]; !ok {
				return explainAlternatives(errors.Newf("action :%s is not defined for role %q", checkAct, r.name),
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
		return errors.WithHint(errors.New("unknown syntax"),
			`supported syntax:
  # set the tempo to 300 milliseconds
  tempo 300ms

  # scene character "a" causes actor "bob" to perform action "cure"
  scene a entails for bob: cure

  # scene character "a" entails "bob" to perform "cure", a failure is tolerated
  scene a entails for bob: cure?

  # scene "b" entails "bob" to perform "cure" and "sleep"
  scene b entails for bob: cure; sleep

  # scene "c" entails for every actor playing "doctor" to perform "cure"
  scene c entails for every doctor: cure

  # scene "d" finishes on a red mood, "e" starts on a blue mood
  scene d mood ends red
  scene e mood starts blue

  # script defines one act with three scenes "a", "b", "c"
  storyline abc

  # script defines two acts, second act waits two seconds before scene "d"
  storyline abc ..d

  # in act two, scenes "b" and "c" run concurrently
  storyline . b
  storyline . c

  # ensure every scene "a" is followed by "b" (uses regular expressions)
  edit s/a/ab/

  # repeat the act containing scene "a" and every act after (uses regular expressions)
  repeat from a

  # repeat at most 5 times
  repeat 5 times

  # repeat for at most 5 minutes
  repeat time 5m
`)
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

func (p *parser) idp(part string, cfg *config) (string, error) {
	n := p.get(part)
	var err error
	n, err = cfg.preprocReplace(n)
	if err != nil {
		return "", err
	}
	return n, checkIdent(n)
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

var preprocRe = compileRe(`~\w+~`)

// preprocReplace replaces occurrences of %pvar% by val in str.
func (cfg *config) preprocReplace(str string) (string, error) {
	var err error
	res := preprocRe.ReplaceAllStringFunc(str, func(m string) string {
		vn := m[1 : len(m)-1]
		if val, ok := cfg.pVars[vn]; ok {
			return val
		}
		err = combineErrors(err, errors.Newf("undefined parameter: %s", m))
		return m
	})
	return res, errors.WithHint(
		explainAlternatives(err, "parameters", cfg.pVars),
		"maybe use -D...?")
}
