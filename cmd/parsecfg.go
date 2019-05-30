package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/log"
)

// parseCfg parses a configuration from the given buffered input.
func parseCfg(rd *bufio.Reader) error {
	topLevelParsers := []struct {
		headerRe *regexp.Regexp
		parseFn  func(line string) error
	}{
		{actionsRe, parseActs},
		{actorsRe, parseActors},
		{scriptRe, parseScript},
		{audienceRe, parseAudience},
	}
	for {
		line, stop, skip, err := readLine(rd)
		if err != nil || stop {
			return err
		} else if skip {
			continue
		}

		if roleRe.MatchString(line) {
			// The "role" syntax is special because the role name
			// is listed in the heading line.
			roleName := roleRe.ReplaceAllString(line, "${rolename}")
			if err := parseRole(rd, roleName); err != nil {
				return err
			}
		} else {
			// Every other section can be handled in a generic way.
			foundParser := false
			for _, parser := range topLevelParsers {
				if parser.headerRe.MatchString(line) {
					foundParser = true
					if err := parseSection(rd, parser.parseFn); err != nil {
						return err
					}
				}
			}
			if !foundParser {
				return fmt.Errorf("unknown syntax: %s", line)
			}
		}
	}
}

func compileRe(re string) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf("(?s:%s)", re))
}

var audienceRe = compileRe(`^audience$`)
var watchRe = compileRe(`^(?P<name>\S+)\s+watches\s+(?P<target>every\s+\S+|\S+)\s+(?P<signal>\S+)\s*$`)
var measuresRe = compileRe(`^(?P<name>\S+)\s+measures\s+(?P<ylabel>.*)$`)

// parseSection applies the given lineParser to every line inside a section,
// and stops at the "end" keyword.
func parseSection(rd *bufio.Reader, lineParser func(line string) error) error {
	for {
		line, stop, skip, err := readLine(rd)
		if err != nil || stop {
			return err
		} else if skip {
			continue
		}
		if line == "end" {
			return nil
		}
		if err := lineParser(line); err != nil {
			return err
		}
	}
}

func parseAudience(line string) error {
	if watchRe.MatchString(line) {
		aName := watchRe.ReplaceAllString(line, "${name}")
		target := watchRe.ReplaceAllString(line, "${target}")
		signal := watchRe.ReplaceAllString(line, "${signal}")

		a, ok := audiences[aName]
		if !ok {
			a = &audience{name: aName, signals: make(map[string]*audienceSource)}
			audiences[aName] = a
		}
		aSrc := &audienceSource{origin: target, hasData: make(map[string]bool)}
		a.signals[signal] = aSrc

		if strings.HasPrefix(target, "every ") {
			// Audience watches every actor playing a given role.
			roleName := strings.TrimPrefix(target, "every ")
			r, ok := roles[roleName]
			if !ok {
				return fmt.Errorf("unknown role %q: %s", roleName, line)
			}
			isEventSignal, ok := getSignal(r, signal)
			if !ok {
				return fmt.Errorf("unknown signal %q for role %s: %s", signal, r.name, line)
			}
			aSrc.drawEvents = isEventSignal

			foundActor := false
			for _, act := range actors {
				if act.role != r {
					continue
				}
				act.addAudience(signal, aName)
				foundActor = true
			}
			if !foundActor {
				log.Warningf(context.TODO(), "there is no actor playing role %q for audience %q to watch", r.name, aName)
			}
		} else {
			// Audience watches a specific actor.
			act, ok := actors[target]
			if !ok {
				return fmt.Errorf("unknown actor %q: %s", target, line)
			}
			isEventSignal, ok := getSignal(act.role, signal)
			if !ok {
				return fmt.Errorf("unknown signal %q for role %s: %s", signal, act.role.name, line)
			}
			aSrc.drawEvents = isEventSignal
			act.addAudience(signal, aName)
		}
	} else if measuresRe.MatchString(line) {
		aName := measuresRe.ReplaceAllString(line, "${name}")
		ylabel := strings.TrimSpace(measuresRe.ReplaceAllString(line, "${ylabel}"))
		a, ok := audiences[aName]
		if !ok {
			a = &audience{name: aName, signals: make(map[string]*audienceSource)}
			audiences[aName] = a
		}
		a.ylabel = ylabel
	}
	return nil
}

func (a *actor) addAudience(sigName, audienceName string) {
	s, ok := a.audiences[sigName]
	if !ok {
		s = &sink{}
		a.audiences[sigName] = s
	}
	s.audiences = append(s.audiences, audienceName)
}

func getSignal(r *role, signal string) (isEventSignal bool, ok bool) {
	for _, rp := range r.resParsers {
		if rp.name == signal {
			return rp.typ == parseEvent, true
		}
	}
	return false, false
}

var roleRe = compileRe(`^role\s+(?P<rolename>\w+)\s+is$`)
var actionDefRe = compileRe(`^:(?P<actionname>\w+)\s+(?P<cmd>.*)$`)
var spotlightDefRe = compileRe(`^spotlight\s+(?P<cmd>.*)$`)
var cleanupDefRe = compileRe(`^cleanup\s+(?P<cmd>.*)$`)
var prepareDefRe = compileRe(`^prepare\s+(?P<cmd>.*)$`)
var parseDefRe = compileRe(`^signal\s+(?P<name>\S+)\s+(?P<type>\S+)\s+at\s+(?P<re>.*)$`)

func parseRole(rd *bufio.Reader, roleName string) error {
	if _, ok := roles[roleName]; ok {
		return fmt.Errorf("duplicate role definition: %s", roleName)
	}

	parserNames := make(map[string]struct{})
	thisRole := role{name: roleName, actionCmds: make(map[string]cmd)}
	roles[roleName] = &thisRole

	return parseSection(rd, func(line string) error {
		if actionDefRe.MatchString(line) {
			aName := actionDefRe.ReplaceAllString(line, "${actionname}")
			aCmd := actionDefRe.ReplaceAllString(line, "${cmd}")
			if _, ok := thisRole.actionCmds[aName]; ok {
				return fmt.Errorf("role %q: duplicate action name %q", roleName, aName)
			}
			thisRole.actionCmds[aName] = cmd(aCmd)
		} else if spotlightDefRe.MatchString(line) {
			thisRole.spotlightCmd = cmd(spotlightDefRe.ReplaceAllString(line, "${cmd}"))
		} else if cleanupDefRe.MatchString(line) {
			thisRole.cleanupCmd = cmd(cleanupDefRe.ReplaceAllString(line, "${cmd}"))
		} else if prepareDefRe.MatchString(line) {
			thisRole.prepareCmd = cmd(prepareDefRe.ReplaceAllString(line, "${cmd}"))
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
				return fmt.Errorf("role %q: duplicate data series name %q: %s", roleName, pname, line)
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

var actorsRe = compileRe(`^actors$`)
var actorDefRe = compileRe(`^(?P<actorname>\S+)\s+plays\s+(?P<rolename>\w+)\s*(?P<extraenv>\(.*\)|)\s*$`)

func parseActors(line string) error {
	if actorDefRe.MatchString(line) {
		roleName := actorDefRe.ReplaceAllString(line, "${rolename}")
		actorName := actorDefRe.ReplaceAllString(line, "${actorname}")
		extraEnv := actorDefRe.ReplaceAllString(line, "${extraenv}")

		r, ok := roles[roleName]
		if !ok {
			return fmt.Errorf("unknown role: %s", roleName)
		}

		actorName = strings.TrimSpace(actorName)
		if _, ok := actors[actorName]; ok {
			return fmt.Errorf("duplicate actor definition: %s", actorName)
		}

		if extraEnv != "" {
			extraEnv = strings.TrimPrefix(extraEnv, "(")
			extraEnv = strings.TrimSuffix(extraEnv, ")")
		}

		act := actor{
			name:      actorName,
			role:      r,
			extraEnv:  extraEnv,
			workDir:   filepath.Join(artifactsDir, actorName),
			audiences: make(map[string]*sink),
		}
		actors[actorName] = &act
	} else {
		return fmt.Errorf("unknown syntax: %s", line)
	}
	return nil
}

var actionsRe = compileRe(`^actions$`)
var actionRe = compileRe(`^(?P<char>\S)\s+(?P<chardef>.*)$`)
var nopRe = compileRe(`^nop$`)
var doRe = compileRe(`^(?P<actor>[^:]+):(?P<action>.*)$`)
var ambianceRe = compileRe(`^mood\s+(?P<mood>.*)$`)

func parseActs(line string) error {
	if !actionRe.MatchString(line) {
		return fmt.Errorf("unknown syntax: %s", line)
	}

	actShorthand := actionRe.ReplaceAllString(line, "${char}")
	actChar := actShorthand[0]

	a := action{name: actShorthand}
	actions[actChar] = append(actions[actChar], &a)

	actAction := actionRe.ReplaceAllString(line, "${chardef}")
	actAction = strings.TrimSpace(actAction)
	if nopRe.MatchString(actAction) {
		a.typ = nopAction
	} else if ambianceRe.MatchString(actAction) {
		a.typ = ambianceAction
		act := ambianceRe.ReplaceAllString(actAction, "${mood}")
		a.act = strings.TrimSpace(act)
	} else if doRe.MatchString(actAction) {
		a.typ = doAction
		act := doRe.ReplaceAllString(actAction, "${action}")
		act = strings.TrimSpace(act)

		target := doRe.ReplaceAllString(actAction, "${actor}")
		targets := strings.Split(target, ",")
		for i := range targets {
			targets[i] = strings.TrimSpace(targets[i])
			theActor, ok := actors[targets[i]]
			if !ok {
				return fmt.Errorf("undefined actor %q: %s", targets[i], line)
			}
			if _, ok := theActor.role.actionCmds[act]; !ok {
				return fmt.Errorf("undefined action %q for role %s: %s", act, theActor.role.name, line)
			}
		}
		a.targets = targets
		a.act = act
	} else {
		return fmt.Errorf("unknown action syntax: %s", line)
	}
	return nil
}

var scriptRe = compileRe(`^script$`)
var stanzaRe = compileRe(`^stanza\s+(?P<stanza>.*)$`)
var tempoRe = compileRe(`^tempo\s+(?P<dur>.*)$`)

func parseScript(line string) error {
	if tempoRe.MatchString(line) {
		durS := tempoRe.ReplaceAllString(line, "${dur}")
		dur, err := time.ParseDuration(durS)
		if err != nil {
			return fmt.Errorf("parsing tempo for %q: %+v", line, err)
		}
		tempo = dur
	} else if stanzaRe.MatchString(line) {
		stanza := stanzaRe.ReplaceAllString(line, "${stanza}")
		for i := 0; i < len(stanza); i++ {
			if _, ok := actions[stanza[i]]; !ok {
				return fmt.Errorf("action '%c' not defined: %s", stanza[i], line)
			}
		}
		stanzas = append(stanzas, stanza)
	} else {
		return fmt.Errorf("unknown syntax: %s", line)
	}
	return nil
}

// readLine reads a logical line of specification. It may be split
// across multiple input lines using backslashes.
func readLine(rd *bufio.Reader) (line string, stop bool, skip bool, err error) {
	for {
		var oneline string
		oneline, err = rd.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", true, false, err
		}
		if strings.HasSuffix(oneline, "\\\n") {
			line += oneline
			continue
		}
		oneline = strings.TrimSuffix(oneline, "\n")
		if err == io.EOF && line != "" {
			return "", true, false, fmt.Errorf("EOF encountered while expecting line continuation")
		}
		line += oneline
		break
	}

	line = strings.TrimSpace(line)
	if err == io.EOF && ignoreLine(line) {
		return "", true, false, nil
	}

	if ignoreLine(line) {
		return "", false, true, nil
	}

	return line, false, false, nil
}

// Empty lines and comment lines are ignored.
func ignoreLine(line string) bool {
	return line == "" || strings.HasPrefix(line, "#")
}
