package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
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
				return pos.wrapErr(fmt.Errorf("unknown syntax: %s", line))
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
var measuresRe = compileRe(`^(?P<name>\S+)\s+measures\s+(?P<ylabel>.*)$`)
var auditorRe = compileRe(`^(?P<name>\S+)\s+expects\s+(?P<when>always|eventually|always eventually)\s*:\s*(?P<expr>.*)$`)

func (cfg *config) parseAudience(line string) error {
	if watchRe.MatchString(line) {
		aName := watchRe.ReplaceAllString(line, "${name}")
		target := watchRe.ReplaceAllString(line, "${target}")
		signal := watchRe.ReplaceAllString(line, "${signal}")

		r, foundActors, err := cfg.selectActors(target)
		if err != nil {
			return fmt.Errorf("%s: %s", err, line)
		}
		if len(foundActors) == 0 {
			log.Warningf(context.TODO(), "there is no actor playing role %q for audience %q to watch", r.name, aName)
			return nil
		}
		a := cfg.addOrGetAudienceMember(aName)

		if err := a.addOrUpdateSignalSource(r, signal, target); err != nil {
			return errors.Wrapf(err, "while parsing: %s", line)
		}

		for _, actor := range foundActors {
			actor.addObserver(signal, aName)
		}
	} else if measuresRe.MatchString(line) {
		aName := measuresRe.ReplaceAllString(line, "${name}")
		ylabel := strings.TrimSpace(measuresRe.ReplaceAllString(line, "${ylabel}"))
		a := cfg.addOrGetAudienceMember(aName)
		a.observer.ylabel = ylabel
	} else if auditorRe.MatchString(line) {
		aName := auditorRe.ReplaceAllString(line, "${name}")
		aWhen := auditorRe.ReplaceAllString(line, "${when}")
		aExpr := strings.TrimSpace(auditorRe.ReplaceAllString(line, "${expr}"))

		a := cfg.addOrGetAudienceMember(aName)
		if a.auditor.when != auditNone {
			return fmt.Errorf("duplicate auditor definition: %s", line)
		}
		a.auditor.expr = aExpr

		when, err := parseAuditWhen(aWhen)
		if err != nil {
			return errors.Wrapf(
				errors.Wrapf(err, "auditor %q", aName),
				"while parsing: %s", line)
		}
		a.auditor.when = when

		if err := a.checkExpr(cfg); err != nil {
			return errors.Wrapf(
				errors.Wrapf(err, "auditor %q", aName),
				"while parsing: %s", line)
		}
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
		return fmt.Errorf("duplicate role definition: %s", roleName)
	}

	var thisRole *role
	if extends != "" {
		parentRole, ok := cfg.roles[extends]
		if !ok {
			return fmt.Errorf("unknown role: %s", extends)
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
				return fmt.Errorf("role %q: duplicate action name %q", roleName, aName)
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
				return fmt.Errorf("role %q: error compiling regexp %s: %+v", roleName, reS, err)
			}
			rp.re = re
			pname := parseDefRe.ReplaceAllString(line, "${name}")
			if _, ok := parserNames[pname]; ok {
				return fmt.Errorf("role %q: duplicate signal name %q: %s", roleName, pname, line)
			}
			parserNames[pname] = struct{}{}
			rp.name = pname

			ptype := parseDefRe.ReplaceAllString(line, "${type}")
			switch ptype {
			case "event":
				rp.typ = parseEvent
				if !hasSubexp(re, "event") {
					return fmt.Errorf("role %q: expected 'event' in regexp: %s", roleName, line)
				}
			case "scalar":
				rp.typ = parseScalar
				if !hasSubexp(re, "scalar") {
					return fmt.Errorf("role %q: expected 'scalar' in regexp: %s", roleName, line)
				}
			case "delta":
				rp.typ = parseDelta
				if !hasSubexp(re, "delta") {
					return fmt.Errorf("role %q: expected 'delta' in regexp: %s", roleName, line)
				}
			default:
				return fmt.Errorf("role %q: unknown parser type %q: %s", roleName, ptype, line)
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
				return fmt.Errorf("role %q: unknown time stamp format in regexp: %s", roleName, line)
			}

			thisRole.resParsers = append(thisRole.resParsers, &rp)
		} else {
			return fmt.Errorf("role %q: unknown syntax: %s", roleName, line)
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
			return fmt.Errorf("unknown role: %s", roleName)
		}

		actorName = strings.TrimSpace(actorName)
		if _, ok := cfg.actors[actorName]; ok {
			return fmt.Errorf("duplicate actor definition: %s", actorName)
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
		return fmt.Errorf("unknown syntax: %s", line)
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
				a.act = act
			} else {
				return fmt.Errorf("unknown action syntax: %s", line)
			}
		}
	} else if tempoRe.MatchString(line) {
		durS := tempoRe.ReplaceAllString(line, "${dur}")
		dur, err := time.ParseDuration(durS)
		if err != nil {
			return fmt.Errorf("parsing tempo for %q: %+v", line, err)
		}
		cfg.tempo = dur
	} else if stanzaRe.MatchString(line) {
		script := stanzaRe.ReplaceAllString(line, "${stanza}")
		target := stanzaRe.ReplaceAllString(line, "${target}")

		r, foundActors, err := cfg.selectActors(target)
		if err != nil {
			return fmt.Errorf("%s: %s", err, line)
		}
		if len(foundActors) == 0 {
			log.Warningf(context.TODO(), "there is no actor playing role %q to play this script: %s", r.name, line)
			return nil
		}

		for i := 0; i < len(script); i++ {
			actionSteps, ok := cfg.actions[script[i]]
			if !ok {
				return fmt.Errorf("action '%c' not defined: %s", script[i], line)
			}
			for _, act := range actionSteps {
				if act.typ != doAction {
					continue
				}
				if _, ok := r.actionCmds[act.act]; !ok {
					return fmt.Errorf("action '%c', :%q not defined for role %q: %s", script[i], act.act, r.name, line)
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
		return fmt.Errorf("unknown syntax: %s", line)
	}
	return nil
}

func parseAuditWhen(when string) (auditorWhen, error) {
	switch when {
	case "always":
		return auditAlways, nil
	case "eventually":
		return auditEventually, nil
	case "eventually always":
		return auditEventuallyAlways, nil
	default:
		return 0, fmt.Errorf("unknown when syntax: %q", when)
	}
}
