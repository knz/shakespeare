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
		if r.cleanupCmd != "" {
			fmt.Printf("  cleanup %s\n", r.cleanupCmd)
		}
		if r.spotlightCmd != "" {
			fmt.Printf("  spotlight %s\n", r.spotlightCmd)
		}
		for _, rp := range r.resParsers {
			fmt.Printf("  signal %s\n", rp.String())
		}
		for actName, a := range r.actionCmds {
			fmt.Printf("  :%s %s\n", actName, a)
		}
		fmt.Println("end")
		fmt.Println()
	}
	fmt.Println("cast")
	for _, a := range actors {
		fmt.Printf("  %s plays %s", a.name, a.role.name)
		if a.extraEnv != "" {
			fmt.Printf("(%s)", a.extraEnv)
		}
		fmt.Println()
		fmt.Printf("  # %s plays from working directory %s", a.name, a.workDir)
		fmt.Println()
		for sig, sink := range a.sinks {
			plural := "s"
			if strings.HasSuffix(sig, "s") {
				plural = ""
			}
			fmt.Printf("  # %s %s%s are watched by audience %+v\n", a.name, sig, plural, sink.audiences)
		}
	}
	fmt.Println("end")
	fmt.Println()

	fmt.Println("audience")
	for _, a := range audiences {
		for sigName, source := range a.signals {
			fmt.Printf("  %s watches %s %s\n", a.name, source.origin, sigName)
		}
		if a.ylabel != "" {
			fmt.Printf("  %s measures %s\n", a.name, a.ylabel)
		}
	}
	fmt.Println("end")

	fmt.Println()
	fmt.Println("script")
	fmt.Printf("  tempo %s\n", tempo)
	for _, aa := range actions {
		for _, a := range aa {
			fmt.Printf("  action %s entails %s\n", a.name, a.String())
		}
	}
	for _, stanza := range stanzas {
		fmt.Printf("  prompt %-10s %s\n", stanza.actor.name, stanza.script)
	}
	fmt.Println("end")
}

// cmd is the type of a command that can be executed as
// effect of a play action.
type cmd string

// role is a model that can be played by zero or more actors.
type role struct {
	name string
	// cleanupCmd is executed once at the end, and also once at the beginning.
	cleanupCmd cmd
	// spotlightCmd is executed in the background during the test.
	spotlightCmd cmd
	// actionCmds are executed upon steps in the script.
	actionCmds map[string]cmd
	// resParsers are the supported signals for each actor.
	resParsers []*resultParser
}

type resultParser struct {
	typ parserType
	// name is the name of this signal/source on each actor.
	name string
	// re is how to parse the signal/source to get data points.
	re *regexp.Regexp
	// reGroup is the regexp group holding the time stamp.
	reGroup string
	// timeLayout is how to parse the timestamp discovered by reGroup using time.Parse.
	timeLayout string
}

type parserType int

const (
	parseEvent parserType = iota
	parseScalar
	parseDelta
)

func (p *resultParser) String() string {
	switch p.typ {
	case parseEvent:
		return fmt.Sprintf("%s event at %s", p.name, p.re.String())
	case parseScalar:
		return fmt.Sprintf("%s scalar at %s", p.name, p.re.String())
	case parseDelta:
		return fmt.Sprintf("%s delta at %s", p.name, p.re.String())
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
	// sinks is the set of sinks that are listening to this
	// actor's signal(s). The map key is the signal name, the value
	// is the sink.
	sinks map[string]*sink
	// hasData indicates there were action events executed for this actor.
	hasData bool
}

type sink struct {
	// audiences refers to names of audience instances.
	audiences []string
	// lastVal is the last value received, for deltas.
	lastVal float64
}

// actors is the set of actors defined by the configuration.
// This is populated during parsing.
var actors = make(map[string]*actor)

// action is the description of a step that can be mentioned
// in a play stanza.
type action struct {
	name string
	typ  actionType
	dur  time.Duration
	act  string
}

func (a *action) String() string {
	switch a.typ {
	case nopAction:
		return "nop"
	case ambianceAction:
		return fmt.Sprintf("mood %s", a.act)
	case doAction:
		return fmt.Sprintf(":%s", a.act)
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
var stanzas []stanza

// stanza describes a play line for one actor.
type stanza struct {
	actor  *actor
	script string
}

// tempo is the interval at which the stanzas are played.
// This is populated during parsing, and used during compile().
var tempo = time.Second

type audience struct {
	name    string
	signals map[string]*audienceSource
	ylabel  string
	// hasData indicates whether data was received for this audience.
	hasData bool
}

type audienceSource struct {
	origin string
	// hasData indicates whether data was received from a given actor.
	hasData    map[string]bool
	drawEvents bool
}

var audiences = make(map[string]*audience)
