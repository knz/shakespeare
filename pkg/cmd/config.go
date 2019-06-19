package cmd

import (
	"fmt"
	"io"
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
	roles     map[string]*role
	roleNames []string

	// actors is the set of actors defined by the configuration.
	// This is populated during parsing.
	actors     map[string]*actor
	actorNames []string

	// actions is the set of actions defined by the configuration.
	// This is populated during parsing.
	actions     map[byte][]*action
	actionChars []byte

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
	audience      map[string]*audienceMember
	audienceNames []string
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
func (cfg *config) printCfg(w io.Writer) {
	if len(cfg.roles) == 0 {
		fmt.Fprintln(w, "# no roles defined")
	} else {
		for _, rn := range cfg.roleNames {
			r := cfg.roles[rn]
			fmt.Fprintf(w, "role %s is\n", r.name)
			if r.cleanupCmd != "" {
				fmt.Fprintf(w, "  cleanup %s\n", r.cleanupCmd)
			}
			if r.spotlightCmd != "" {
				fmt.Fprintf(w, "  spotlight %s\n", r.spotlightCmd)
			}
			for _, rp := range r.resParsers {
				fmt.Fprintf(w, "  signal %s\n", rp.String())
			}
			for actName, a := range r.actionCmds {
				fmt.Fprintf(w, "  :%s %s\n", actName, a)
			}
			fmt.Fprintln(w, "end")
			// fmt.Fprintln(w)
		}
	}
	if len(cfg.actors) == 0 {
		fmt.Fprintln(w, "# no cast defined")
	} else {
		fmt.Fprintln(w, "cast")
		for _, an := range cfg.actorNames {
			a := cfg.actors[an]
			fmt.Fprintf(w, "  %s plays %s", a.name, a.role.name)
			if a.extraEnv != "" {
				fmt.Fprintf(w, "(%s)", a.extraEnv)
			}
			fmt.Fprintln(w)
			fmt.Fprintf(w, "  # %s plays from working directory %s\n", a.name, a.workDir)
			for _, sig := range a.sinkNames {
				sink := a.sinks[sig]
				plural := "s"
				if strings.HasSuffix(sig, "s") {
					plural = ""
				}
				if len(sink.observers) > 0 {
					fmt.Fprintf(w, "  # %s %s%s are watched by audience %+v\n", a.name, sig, plural, sink.observers)
				}
				if len(sink.auditors) > 0 {
					fmt.Fprintf(w, "  # %s %s%s are checked by auditors %+v\n", a.name, sig, plural, sink.auditors)
				}
			}
		}
		fmt.Fprintln(w, "end")
		// fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "script")
	fmt.Fprintf(w, "  tempo %s\n", cfg.tempo)
	for _, aan := range cfg.actionChars {
		aa := cfg.actions[aan]
		for _, a := range aa {
			fmt.Fprintf(w, "  action %s entails %s\n", a.name, a.String())
		}
	}
	if len(cfg.stanzas) == 0 {
		fmt.Fprintln(w, "  # no stanzas defined, play will terminate immediately")
	} else {
		for _, stanza := range cfg.stanzas {
			fmt.Fprintf(w, "  prompt %-10s %s\n", stanza.actor.name, stanza.script)
		}
	}
	fmt.Fprintln(w, "end")
	// fmt.Fprintln(w)

	if len(cfg.audience) == 0 {
		fmt.Fprintln(w, "# no audience defined")
	} else {
		fmt.Fprintln(w, "audience")
		for _, an := range cfg.audienceNames {
			a := cfg.audience[an]
			for _, sigName := range a.observer.sigNames {
				source := a.observer.signals[sigName]
				for _, origin := range source.origin {
					fmt.Fprintf(w, "  %s watches %s %s\n", a.name, origin, sigName)
				}
			}
			if a.observer.ylabel != "" {
				fmt.Fprintf(w, "  %s measures %s\n", a.name, a.observer.ylabel)
			}
			if a.auditor.when != auditNone {
				fmt.Fprintf(w, "  %s expects %s: %s\n", a.name, a.auditor.when.String(), a.auditor.expr)
			}
		}
		fmt.Fprintln(w, "end")
	}
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
	actionCmds  map[string]cmd
	actionNames []string
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
	sinks     map[string]*sink
	sinkNames []string
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
	signals  map[string]*audienceSource
	sigNames []string
	ylabel   string
	// hasData indicates whether data was received for this audience.
	hasData bool
}

type audienceSource struct {
	origin []string
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
		cfg.audienceNames = append(cfg.audienceNames, name)
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
		a.sinkNames = append(a.sinkNames, sigName)
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
		a.sinkNames = append(a.sinkNames, sigName)
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

// addOrUpdateSignalSource adds a signal source to an observer.
func (a *audienceMember) addOrUpdateSignalSource(r *role, signal, target string) error {
	isEventSignal, ok := r.getSignal(signal)
	if !ok {
		return fmt.Errorf("unknown signal %q for role %s", signal, r.name)
	}

	if s, ok := a.observer.signals[signal]; ok {
		if s.drawEvents != isEventSignal {
			return fmt.Errorf("audience %q watches signal %q with mismatched types", a.name, signal)
		}
		s.origin = append(s.origin, target)
		return nil
	}
	aSrc := &audienceSource{
		origin:     []string{target},
		hasData:    make(map[string]bool),
		drawEvents: isEventSignal,
	}
	a.observer.signals[signal] = aSrc
	a.observer.sigNames = append(a.observer.sigNames, signal)
	return nil
}
