package cmd

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Knetic/govaluate"
)

type config struct {
	// The output directory for data files, artifacts and plot scripts.
	dataDir string
	// Whether to print out the parsed configuration upon init.
	doPrint bool
	// Whether to stop after parsing and compiling the configuration.
	parseOnly bool
	// Whether to stop upon the first audit violation.
	earlyExit bool
	// Whether to silence logging.
	quiet bool
	// The artifacts sub-directory (always "artifacts" relative to datadir).
	artifactsDir string
	// The path to the unix shell used to execute script commands.
	shellPath string
	// The list of directory to search for includes.
	includePath []string

	// roles is the set of roles defined by the configuration.
	// This is populated during parsing.
	roles map[string]*role

	// actors is the set of actors defined by the configuration.
	// This is populated during parsing.
	actors map[string]*actor

	// actions is the set of actions defined by the configuration.
	// This is populated during parsing.
	actions map[byte][]*action

	// stanzas defines the programmatic play scenario.
	// This is populated during parsing, and transformed
	// into steps during compile().
	stanzas []stanza

	// play is the list of actions to play.
	// This is populated by compile().
	play []scene

	// tempo is the interval at which the stanzas are played.
	// This is populated during parsing, and used during compile().
	tempo time.Duration

	// audience is the set of observers/auditors for the play.
	audience map[string]*audienceMember
}

type audienceMember struct {
	name     string
	observer observer
	auditor  auditor
}

// newConfig creates a config with defaults.
func newConfig() *config {
	return &config{
		roles:    make(map[string]*role),
		actors:   make(map[string]*actor),
		actions:  make(map[byte][]*action),
		stanzas:  nil,
		tempo:    time.Second,
		audience: make(map[string]*audienceMember),
	}
}

// printCfg prints the current configuration.
func (cfg *config) printCfg() {
	for _, r := range cfg.roles {
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
	for _, a := range cfg.actors {
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
			if len(sink.observers) > 0 {
				fmt.Printf("  # %s %s%s are watched by audience %+v\n", a.name, sig, plural, sink.observers)
			}
			if len(sink.auditors) > 0 {
				fmt.Printf("  # %s %s%s are checked by auditors %+v\n", a.name, sig, plural, sink.auditors)
			}
		}
	}
	fmt.Println("end")
	fmt.Println()

	fmt.Println("script")
	fmt.Printf("  tempo %s\n", cfg.tempo)
	for _, aa := range cfg.actions {
		for _, a := range aa {
			fmt.Printf("  action %s entails %s\n", a.name, a.String())
		}
	}
	for _, stanza := range cfg.stanzas {
		fmt.Printf("  prompt %-10s %s\n", stanza.actor.name, stanza.script)
	}
	fmt.Println("end")
	fmt.Println()

	fmt.Println("audience")
	for _, a := range cfg.audience {
		for sigName, source := range a.observer.signals {
			fmt.Printf("  %s watches %s %s\n", a.name, source.origin, sigName)
		}
		if a.observer.ylabel != "" {
			fmt.Printf("  %s measures %s\n", a.name, a.observer.ylabel)
		}
		if a.auditor.when != auditNone {
			fmt.Printf("  %s expects %s: %s\n", a.name, a.auditor.when.String(), a.auditor.expr)
		}
	}
	fmt.Println("end")
	fmt.Println()
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

// actor is an agent that can participate in a play.
type actor struct {
	name      string
	role      *role
	workDir   string
	shellPath string
	extraEnv  string
	// sinks is the set of sinks that are listening to this
	// actor's signal(s). The map key is the signal name, the value
	// is the sink.
	sinks map[string]*sink
	// hasData indicates there were action events executed for this actor.
	hasData bool
}

type sink struct {
	// observers refers to names of audience instances.
	observers []string
	// auditors refers to names of auditor instances.
	auditors []string
	// lastVal is the last value received, for deltas.
	lastVal float64
}

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

// stanza describes a play line for one actor.
type stanza struct {
	actor  *actor
	script string
}

type observer struct {
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

type auditor struct {
	when            auditorWhen
	expr            string
	compiledExp     *govaluate.EvaluableExpression
	observedSignals map[exprVar]struct{}
}

type exprVar struct {
	actorName string
	sigName   string
}

func (e exprVar) Name() string {
	return e.actorName + "." + e.sigName
}

type auditorWhen int

const (
	auditNone auditorWhen = iota
	auditAlways
	auditEventually
	auditEventuallyAlways
)

var wName = map[auditorWhen]string{
	auditNone:             "???",
	auditAlways:           "always",
	auditEventually:       "eventually",
	auditEventuallyAlways: "eventually always",
}

func (w auditorWhen) String() string {
	return wName[w]
}

type auditorState struct {
	hasData bool
	history []auditorEvent
}

type auditorEvent struct {
	evTime float64
	value  interface{}
	err    error
}

type moodPeriod struct {
	startTime float64
	endTime   float64
	mood      string
}

type audition struct {
	cfg *config
	// epoch is the instant at which the collector started collecting events.
	// audition instants are relative to this moment.
	epoch         time.Time
	curMood       string
	curMoodStart  float64
	moodPeriods   []moodPeriod
	auditorStates map[string]*auditorState
	curVals       map[string]interface{}
	activations   map[exprVar]struct{}
	violations    []auditViolation
}

type auditViolation struct {
	ts          float64
	auditorName string
	output      string
}

// selectActors selects the actors (and the role) matching
// the target definition.
// - every <rolename>  -> every actor playing <rolename>
// - <actorname>       -> that specific actor
func (cfg *config) selectActors(target string) (*role, []*actor, error) {
	if strings.HasPrefix(target, "every ") {
		roleName := strings.TrimPrefix(target, "every ")
		r, ok := cfg.roles[roleName]
		if !ok {
			return nil, nil, fmt.Errorf("unknown role %q", roleName)
		}
		var res []*actor
		for _, a := range cfg.actors {
			if a.role != r {
				continue
			}
			res = append(res, a)
		}
		return r, res, nil
	}
	act, ok := cfg.actors[target]
	if !ok {
		return nil, nil, fmt.Errorf("unknown actor %q", target)
	}
	return act.role, []*actor{act}, nil
}

func (cfg *config) addOrGetAudienceMember(name string) *audienceMember {
	a, ok := cfg.audience[name]
	if !ok {
		a = &audienceMember{
			name: name,
			observer: observer{
				signals: make(map[string]*audienceSource),
			},
		}
		cfg.audience[name] = a
	}
	return a
}

// getSignal retrieves a role's signal.
func (r *role) getSignal(signal string) (isEventSignal bool, ok bool) {
	for _, rp := range r.resParsers {
		if rp.name == signal {
			return rp.typ == parseEvent, true
		}
	}
	return false, false
}

// addAuditor registers an auditor to an actor's signal sink.
// The auditor will receive data change events from that actor's signal.
// addAuditor entails addObserver.
func (a *actor) addAuditor(sigName, auditorName string) {
	a.addObserver(sigName, auditorName)

	s, ok := a.sinks[sigName]
	if !ok {
		s = &sink{}
		a.sinks[sigName] = s
	}
	found := false
	for _, ad := range s.auditors {
		if ad == auditorName {
			found = true
			break
		}
	}
	if !found {
		s.auditors = append(s.auditors, auditorName)
	}
}

// addObserver registers an observer to an actor's signal sink.
// The observer will receive plottable events from that actor's signal.
func (a *actor) addObserver(sigName, observerName string) {
	s, ok := a.sinks[sigName]
	if !ok {
		s = &sink{}
		a.sinks[sigName] = s
	}
	found := false
	for _, ad := range s.observers {
		if ad == observerName {
			found = true
			break
		}
	}
	if !found {
		s.observers = append(s.observers, observerName)
	}
}

// addOrUpdateSignalSource adds a signal source to an observer. It is
// possible for an observer to "merge" signals from multiple actors
// together into a single sink. ?????
func (a *audienceMember) addOrUpdateSignalSource(r *role, signal, target string) error {
	isEventSignal, ok := r.getSignal(signal)
	if !ok {
		return fmt.Errorf("unknown signal %q for role %s", signal, r.name)
	}

	aSrc := &audienceSource{
		origin:     target,
		hasData:    make(map[string]bool),
		drawEvents: isEventSignal,
	}
	a.observer.signals[signal] = aSrc
	return nil
}
