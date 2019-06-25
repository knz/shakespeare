package cmd

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Knetic/govaluate"
	"github.com/cockroachdb/errors"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/log/logflags"
	"github.com/spf13/pflag"
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
	// Whether to displays emoji.
	asciiOnly bool

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

	// vars is the set of variables computed by the audience.
	vars     map[exprVar]*variable
	varNames []exprVar
}

func (cfg *config) initArgs(ctx context.Context) error {
	cfg.shellPath = os.Getenv("SHELL")
	pflag.StringVarP(&cfg.dataDir, "output-dir", "o", ".", "output data directory")
	pflag.BoolVarP(&cfg.doPrint, "print-cfg", "p", false, "print out the parsed configuration")
	pflag.BoolVarP(&cfg.parseOnly, "dry-run", "n", false, "do not execute anything, just check the configuration")
	pflag.BoolVarP(&cfg.quiet, "quiet", "q", false, "do not emit progress messages")
	pflag.BoolVarP(&cfg.earlyExit, "stop-at-first-violation", "S", false, "terminate the play as soon as an auditor is dissatisfied")
	pflag.StringSliceVarP(&cfg.includePath, "search-dir", "I", []string{}, "add this directory to the search path for include directives")
	pflag.BoolVar(&cfg.asciiOnly, "ascii-only", false, "do not display unicode emojis")

	// Load the go flag settings from the log package into pflag.
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	// We'll revisit this value in setupLogging().
	pflag.Lookup(logflags.LogToStderrName).NoOptDefVal = log.Severity_DEFAULT.String()

	// Parse the command-line.
	pflag.Parse()

	// Derive the artifacts directory.
	cfg.artifactsDir = filepath.Join(cfg.dataDir, "artifacts")

	// Ensure the output directory and artifacts dir exist.
	if err := os.MkdirAll(cfg.artifactsDir, 0755); err != nil {
		return err
	}
	return nil
}

type audienceMember struct {
	name     string
	observer observer
	auditor  auditor
}

// newConfig creates a config with defaults.
func newConfig() *config {
	cfg := &config{
		roles:    make(map[string]*role),
		actors:   make(map[string]*actor),
		actions:  make(map[byte][]*action),
		stanzas:  nil,
		tempo:    time.Second,
		audience: make(map[string]*audienceMember),
		vars:     make(map[exprVar]*variable),
	}
	cfg.maybeAddVar(nil, exprVar{sigName: "t"}, false)
	cfg.maybeAddVar(nil, exprVar{sigName: "mood"}, false)
	cfg.maybeAddVar(nil, exprVar{sigName: "moodt"}, false)
	return cfg
}

// printCfg prints the current configuration.
func (cfg *config) printCfg(w io.Writer) {
	if len(cfg.roles) == 0 {
		fmt.Fprintln(w, "# no roles defined")
	} else {
		for _, rn := range cfg.roleNames {
			r := cfg.roles[rn]
			fmt.Fprintf(w, "role %s\n", r.name)
			if r.cleanupCmd != "" {
				fmt.Fprintf(w, "  cleanup %s\n", r.cleanupCmd)
			}
			if r.spotlightCmd != "" {
				fmt.Fprintf(w, "  spotlight %s\n", r.spotlightCmd)
			}
			for _, rp := range r.sigParsers {
				fmt.Fprintf(w, "  signal %s\n", rp.String())
			}
			for _, actName := range r.actionNames {
				a := r.actionCmds[actName]
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
					fmt.Fprintf(w, "  # %s %s%s are watched by audience %s\n", a.name, sig, plural, strings.Join(sink.observers, ", "))
				}
				if len(sink.auditors) > 0 {
					fmt.Fprintf(w, "  # %s %s%s are checked by auditors %s\n", a.name, sig, plural, strings.Join(sink.auditors, ", "))
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
		for _, vn := range cfg.varNames {
			v := cfg.vars[vn]
			qual := ""
			if v.isArray {
				qual = "(collection)"
			}
			for _, watcherName := range v.watcherNames {
				fmt.Fprintf(w, "  # %s sensitive to %s%s\n", watcherName, vn, qual)
			}
		}
		for _, an := range cfg.audienceNames {
			a := cfg.audience[an]
			if a.auditor.activeCond.src != "" {
				if a.auditor.activeCond.src == "true" {
					fmt.Fprintf(w, "  %s audits throughout\n", a.name)
				} else {
					fmt.Fprintf(w, "  %s audits only while %s\n", a.name, a.auditor.activeCond.src)
				}
			}
			for _, as := range a.auditor.assignments {
				fmt.Fprintf(w, "  %s %s\n", a.name, as.String())
			}
			if a.auditor.expectFsm != nil {
				fmt.Fprintf(w, "  %s expects %s: %s\n", a.name, a.auditor.expectFsm.name, a.auditor.expectExpr.src)
			}
			for _, varName := range a.observer.obsVarNames {
				fmt.Fprintf(w, "  %s watches %s\n", a.name, varName)
			}
			if a.observer.ylabel != "" {
				fmt.Fprintf(w, "  %s measures %s\n", a.name, a.observer.ylabel)
			}
			if a.observer.disablePlot {
				fmt.Fprintf(w, "  %s only helps\n", a.name)
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
	// sigParsers are the supported signals for each actor.
	sigParsers []*resultParser
	sigNames   []string
}

func (r *role) clone(newName string) *role {
	newR := *r
	newR.actionNames = append([]string(nil), r.actionNames...)
	newR.sigParsers = append([]*resultParser(nil), r.sigParsers...)
	newR.sigNames = append([]string(nil), r.sigNames...)
	newR.actionCmds = make(map[string]cmd, len(r.actionCmds))
	for k, v := range r.actionCmds {
		newR.actionCmds[k] = v
	}
	newR.name = newName
	return &newR
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
	// This is merely used to:
	// - determine whether a signal is active (there is a sink, as opposed to no sink)
	// - to print the configuration.
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
	name   string
	typ    actionType
	act    string
	failOk bool
}

func (a *action) String() string {
	switch a.typ {
	case nopAction:
		return "nop"
	case ambianceAction:
		return fmt.Sprintf("mood %s", a.act)
	case doAction:
		qual := ""
		if a.failOk {
			qual = "?"
		}
		return fmt.Sprintf(":%s%s", a.act, qual)
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

// observer is the "collector" part of an audience member.
type observer struct {
	obsVars     map[exprVar]*collectedSignal
	obsVarNames []exprVar
	ylabel      string
	disablePlot bool
	// hasData indicates whether data was received for this audience.
	hasData bool
}

type collectedSignal struct {
	origin []string
	// hasData indicates whether data was received from a given actor.
	hasData    bool
	drawEvents bool
}

type variable struct {
	name         exprVar
	isArray      bool
	watchers     map[string]*audienceMember
	watcherNames []string
}

type auditor struct {
	// observedSignals is the set of actor signals
	// that this auditor is sensitive on.
	observedSignals map[exprVar]struct{}

	// activCond determines when this auditor wakes up.
	activeCond expr

	// assignments specifies what to compute when active.
	assignments []assignment

	// expectFsm and expectExpr are the expect condition, when active.
	expectFsm  *fsm
	expectExpr expr
}

type expr struct {
	src      string
	compiled *govaluate.EvaluableExpression
	deps     map[exprVar]struct{}
}

type assignment struct {
	// targetVar is the variable to assign.
	targetVar string
	// source value.
	expr expr
	// assignMode is how to assign to the variable.
	assignMode assignMode
	N          int
}

func (s *assignment) String() string {
	var buf bytes.Buffer
	var mode string
	switch s.assignMode {
	case assignSingle:
		fmt.Fprintf(&buf, "computes %s as %s", s.targetVar, s.expr.src)
		return buf.String()
	case assignFirstN:
		mode = "first"
	case assignLastN:
		mode = "last"
	case assignTopN:
		mode = "top"
	case assignBottomN:
		mode = "bottom"
	}
	fmt.Fprintf(&buf, "collects %s as %s %d %s", s.targetVar, mode, s.N, s.expr.src)
	return buf.String()
}

type assignMode int

const (
	assignSingle  assignMode = iota // stores the value
	assignFirstN                    // first N values in array
	assignLastN                     // last N values in array
	assignTopN                      // top N values in array
	assignBottomN                   // bottom N values in array
)

type exprVar struct {
	actorName string
	sigName   string
}

func (e exprVar) String() string {
	if e.actorName == "" {
		return e.sigName
	}
	return e.actorName + " " + e.sigName
}

type auditorState struct {
	// activated is set to false at the start of each audit round, and
	// set to true every time one of its dependent variables is
	// activated.
	activated bool

	// auditing is set to true when the activation condition has
	// last become true, and reset to false when the activation
	// condition has last become false.
	auditing bool

	// eval is the check FSM. Initialized every time auditing goes
	// from false to true.
	eval fsmEval

	// hasData is set to true the first time the auditor
	// derives an audit event.
	hasData bool
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
	epoch           time.Time
	curMood         string
	curMoodStart    float64
	moodPeriods     []moodPeriod
	auditorStates   map[string]*auditorState
	curActivated    map[exprVar]bool
	curVals         map[string]interface{}
	auditViolations []auditViolation
	// names of auditors that are not sensitive to any particular signal
	// and thus reacts to any of them.
	alwaysAudit []string
}

type auditViolation struct {
	ts          float64
	result      result
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
		if err := checkIdent(roleName); err != nil {
			return nil, nil, err
		}
		r, ok := cfg.roles[roleName]
		if !ok {
			return nil, nil, explainAlternatives(errors.Newf("unknown role %q", roleName), "roles", cfg.roles)
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
	if err := checkIdent(target); err != nil {
		return nil, nil, err
	}
	act, ok := cfg.actors[target]
	if !ok {
		return nil, nil, explainAlternatives(errors.Newf("unknown actor %q", target), "actors", cfg.actors)
	}
	return act.role, []*actor{act}, nil
}

func (cfg *config) addOrGetAudienceMember(name string) *audienceMember {
	a, ok := cfg.audience[name]
	if !ok {
		a = &audienceMember{
			name: name,
			observer: observer{
				obsVars: make(map[exprVar]*collectedSignal),
			},
		}
		cfg.audience[name] = a
		cfg.audienceNames = append(cfg.audienceNames, name)
	}
	return a
}

// getSignal retrieves a role's signal.
func (r *role) getSignal(signal string) (isEventSignal bool, ok bool) {
	for _, rp := range r.sigParsers {
		if rp.name == signal {
			return rp.typ == parseEvent, true
		}
	}
	return false, false
}

// addObserver registers an observer to an actor's signal sink.
// The observer is reported when printing the configuration.
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
func (a *audienceMember) addOrUpdateSignalSource(r *role, vr exprVar) error {
	isEventSignal, ok := r.getSignal(vr.sigName)
	if !ok {
		return explainAlternativesList(errors.Newf("unknown signal %q for role %s", vr.sigName, r.name),
			"signals", r.sigNames...)
	}

	if s, ok := a.observer.obsVars[vr]; ok {
		if s.drawEvents != isEventSignal {
			return errors.WithHint(errors.Newf("audience %q watches signal %q with mismatched types", a.name, vr.String()),
				"a single audience member cannot watch both event and non-event signals")
		}
		return nil
	}
	aSrc := &collectedSignal{
		drawEvents: isEventSignal,
	}
	a.observer.obsVars[vr] = aSrc
	a.observer.obsVarNames = append(a.observer.obsVarNames, vr)
	return nil
}

func (cfg *config) maybeAddVar(a *audienceMember, varName exprVar, dupOk bool) error {
	v, ok := cfg.vars[varName]
	if !ok {
		v = &variable{
			name:     varName,
			watchers: make(map[string]*audienceMember),
		}
		cfg.vars[varName] = v
		cfg.varNames = append(cfg.varNames, varName)
	} else {
		if !dupOk {
			return errors.Newf("variable already defined: %q", varName)
		}
	}
	if a != nil {
		v.maybeAddWatcher(a)
	}
	return nil
}

func (v *variable) maybeAddWatcher(watcher *audienceMember) {
	if _, ok := v.watchers[watcher.name]; ok {
		return
	}
	log.Warningf(context.TODO(), "added watcher %q for var %q", watcher.name, v.name)
	v.watchers[watcher.name] = watcher
	v.watcherNames = append(v.watcherNames, watcher.name)
}
