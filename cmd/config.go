package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// printCfg prints the current configuration.
func printCfg() {
	for _, r := range roles {
		fmt.Printf("role %s is\n", r.name)
		if r.prepareCmd != "" {
			fmt.Printf("  prepare %s\n", r.prepareCmd)
		}
		if r.cleanupCmd != "" {
			fmt.Printf("  cleanup %s\n", r.cleanupCmd)
		}
		if r.spotlightCmd != "" {
			fmt.Printf("  spotlight %s\n", r.spotlightCmd)
		}
		for _, rp := range r.resParsers {
			fmt.Printf("  parse %s\n", rp.String())
		}
		if r.checkExpr != "" {
			fmt.Printf("  check %s\n", r.checkExpr)
		}
		for actName, a := range r.actionCmds {
			fmt.Printf("  :%s %s\n", actName, a)
		}
		fmt.Println("end")
		fmt.Println()
	}
	fmt.Println("actors")
	for _, a := range actors {
		fmt.Printf("  %s plays %s", a.name, a.role.name)
		if a.extraEnv != "" {
			fmt.Printf("(%s)", a.extraEnv)
		}
		fmt.Printf(" # ... from work dir %s", a.workDir)
		fmt.Println()
	}
	fmt.Println("end")
	fmt.Println()
	fmt.Println("actions")
	for _, aa := range actions {
		for _, a := range aa {
			fmt.Printf("  %s %s\n", a.name, a.String())
		}
	}
	fmt.Println("end")
	fmt.Println()
	fmt.Println("play")
	fmt.Printf("  tempo %s\n", tempo)
	for _, stanza := range stanzas {
		fmt.Printf("  stanza %s\n", stanza)
	}
	fmt.Println("end")
}

// cmd is the type of a command that can be executed as
// effect of a play action.
type cmd string

// role is a model that can be played by zero or more actors.
type role struct {
	name         string
	prepareCmd   cmd
	cleanupCmd   cmd
	spotlightCmd cmd
	actionCmds   map[string]cmd
	resParsers   []*resultParser
	checkExpr    string
}

type resultParser struct {
	typ     parserType
	tsTyp   timeStyle
	name    string
	re      *regexp.Regexp
	hasData bool
}

type parserType int

const (
	parseEvent parserType = iota
	parseScalar
	parseDelta
)

type timeStyle int

const (
	timeStyleAbs timeStyle = iota
	timeStyleLog
)

func (p *resultParser) String() string {
	switch p.typ {
	case parseEvent:
		return fmt.Sprintf("event %s %s", p.name, p.re.String())
	case parseScalar:
		return fmt.Sprintf("scalar %s %s", p.name, p.re.String())
	case parseDelta:
		return fmt.Sprintf("delta %s %s", p.name, p.re.String())
	}
	return "<???parser>"
}

// roles is the set of roles defined by the configuration.
// This is populated during parsing.
var roles = make(map[string]*role)

// actor is an agent that can participate in a play.
type actor struct {
	name     string
	role     *role
	workDir  string
	extraEnv string
}

// actors is the set of actors defined by the configuration.
// This is populated during parsing.
var actors = make(map[string]*actor)

// action is the description of a step that can be mentioned
// in a play stanza.
type action struct {
	name    string
	typ     actionType
	dur     time.Duration
	act     string
	targets []string
}

func (a *action) String() string {
	switch a.typ {
	case nopAction:
		return "nop"
	case ambianceAction:
		return fmt.Sprintf("mood %s", a.act)
	case doAction:
		return fmt.Sprintf("%s:%s", strings.Join(a.targets, ","), a.act)
	}
	return "<action???>"
}

type actionType int

const (
	nopAction actionType = iota
	doAction
	ambianceAction
)

// actions is the set of actions defined by the configuration.
// This is populated during parsing.
var actions = make(map[byte][]*action)

// stanzas defines the programmatic play scenario.
// This is populated during parsing, and transformed
// into steps during compile().
var stanzas []string

// tempo is the interval at which the stanzas are played.
// This is populated during parsing, and used during compile().
var tempo = time.Second
