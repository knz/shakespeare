package cmd

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

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
	// The path to the gnuplot command to generate plots.
	gnuplotPath string
	// Disables plotting altogether
	skipPlot bool
	// Disables reporting of progress using time, to make the output more deterministic
	// (used in tests).
	avoidTimeProgress bool
	// Where to write the narration messages.
	narration io.Writer
	// How big each text plot should be (used in tests).
	textPlotHeight, textPlotWidth int
	// Terminal escape code mode to use (used in tests).
	textPlotTerm string

	// The list of directory to search for includes.
	includePath []string
	// Whether to displays emoji.
	asciiOnly bool
	// Additional lines of script config.
	extraScript []string
	// Preprocessing parameter definitions.
	defines []string
	// Preprocessing variables.
	pVars     map[string]string
	pVarNames []string

	// titleStrings is the list of title strings encountered
	// in the configuration.
	titleStrings []string

	// authors is the list of author strings encountered in the configuration
	authors []string

	// seeAlso is the list of "see also" strings encountered in the configuration
	seeAlso []string

	// roles is the set of roles defined by the configuration.
	// This is populated during parsing.
	roles     map[string]*role
	roleNames []string

	// actors is the set of actors defined by the configuration.
	// This is populated during parsing.
	actors     map[string]*actor
	actorNames []string

	// sceneSpecs is the set of scenes defined by the configuration,
	// using the "new" format (V2).
	// This is populated during parsing.
	sceneSpecs     map[byte]*sceneSpec
	sceneSpecChars []byte
	// storyLine is the list of act specifications.
	storyLine []string
	// repeatFrom/repeatActNum is the point at which acts will be repeated.
	repeatFrom   *regexp.Regexp
	repeatActNum int

	// play is the list of actions to play.
	// This is populated by compile().
	play [][]scene

	// tempo is the interval at which the stanzas are played.
	// This is populated during parsing, and used during compile().
	tempo time.Duration

	// audience is the set of observers/auditors for the play.
	audience      map[string]*audienceMember
	audienceNames []string

	// vars is the set of variables computed by the audience.
	vars     map[varName]*variable
	varNames []varName
}

func (cfg *config) initArgs(ctx context.Context) error {
	pflag.StringVarP(&cfg.dataDir, "output-dir", "o", ".", "output data directory")
	pflag.BoolVarP(&cfg.doPrint, "print-cfg", "p", false, "print out the parsed configuration")
	pflag.BoolVarP(&cfg.parseOnly, "dry-run", "n", false, "do not execute anything, just check the configuration")
	pflag.BoolVarP(&cfg.quiet, "quiet", "q", false, "do not emit progress messages")
	pflag.BoolVarP(&cfg.earlyExit, "stop-at-first-violation", "S", false, "terminate the play as soon as an auditor is dissatisfied")
	pflag.StringSliceVarP(&cfg.includePath, "search-dir", "I", []string{}, "add this directory to the search path for include directives")
	pflag.BoolVar(&cfg.asciiOnly, "ascii-only", false, "do not display unicode emojis")
	pflag.BoolVar(&cfg.skipPlot, "disable-plots", false, "do not generate plot scripts at the end")
	var showVersion bool
	pflag.BoolVar(&showVersion, "version", false, "show version information and exit")
	pflag.StringSliceVarP(&cfg.extraScript, "extra-script", "s", []string{}, "additional lines of script configuration, processed at end")
	pflag.StringSliceVarP(&cfg.defines, "define", "D", []string{}, "preprocessing variable definition (eg -Dfoo=bar)")

	// Load the go flag settings from the log package into pflag.
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	// We'll revisit this value in setupLogging().
	pflag.Lookup(logflags.LogToStderrName).NoOptDefVal = log.Severity_DEFAULT.String()

	// Parse the command-line.
	pflag.Parse()

	// Just version?
	if showVersion {
		fmt.Println("shakespeare", versionName)
		os.Exit(0)
	}

	// Derive the initial preproc variables assignments.
	if err := cfg.parseDefines(); err != nil {
		return err
	}

	// Ensure the include path contains the current directory.
	if !hasLocalDir(cfg.includePath) {
		cfg.includePath = append(cfg.includePath, ".")
	}

	return nil
}

func (cfg *config) parseDefines() error {
	for _, d := range cfg.defines {
		// Split variable=value.
		dname := d
		dval := ""
		idx := strings.IndexByte(d, '=')
		if idx >= 0 {
			dname = d[:idx]
			dval = d[idx+1:]
		}
		if _, ok := cfg.pVars[dname]; ok {
			// Define already exists. Do nothing.
			continue
		}
		cfg.pVars[dname] = dval
		cfg.pVarNames = append(cfg.pVarNames, dname)
	}
	return nil
}

// newConfig creates a config with defaults.
func newConfig() *config {
	cfg := &config{
		narration:      os.Stdout,
		roles:          make(map[string]*role),
		actors:         make(map[string]*actor),
		sceneSpecs:     make(map[byte]*sceneSpec),
		pVars:          make(map[string]string),
		tempo:          time.Second,
		audience:       make(map[string]*audienceMember),
		vars:           make(map[varName]*variable),
		textPlotHeight: 24,
		textPlotWidth:  160,
		textPlotTerm:   "ansi256",
	}
	if sh, ok := os.LookupEnv("SHELL"); ok {
		cfg.shellPath = sh
	} else {
		cfg.shellPath = "/bin/bash"
	}
	if gp, ok := os.LookupEnv("GNUPLOT"); ok {
		cfg.gnuplotPath = gp
	} else {
		cfg.gnuplotPath = "gnuplot"
	}
	cfg.maybeAddVar(nil, varName{sigName: "t"}, false)
	cfg.maybeAddVar(nil, varName{sigName: "mood"}, false)
	cfg.maybeAddVar(nil, varName{sigName: "moodt"}, false)
	return cfg
}

// printCfg prints the current configuration.
func (cfg *config) printCfg(w io.Writer, skipComments, skipVer, annot bool) {
	if !skipComments && !skipVer {
		fmt.Fprintln(w, "# configuration parsed by shakespeare", versionName)
	}

	fkw, fsn, fan, frn, facn, fann, fmod, fre, fsh := fid, fid, fid, fid, fid, fid, fid, fid, escapeNl
	if annot {
		fkw = fw("kw")                                               // keyword
		fsn = fw("sn")                                               // sig name
		fan = fw("an")                                               // actor name
		frn = fw("rn")                                               // role name
		facn = fw("acn")                                             // action name
		fann = fw("ann")                                             // audience name
		fmod = fw("mod")                                             // modality
		fre = fw("re")                                               // regexp
		fsh = func(s string) string { return fw("sh")(escapeNl(s)) } // shell script
	}

	for _, title := range cfg.titleStrings {
		fmt.Fprintln(w, fkw("title"), title)
	}
	for _, author := range cfg.authors {
		fmt.Fprintln(w, fkw("author"), author)
	}
	for _, seeAlso := range cfg.seeAlso {
		fmt.Fprintln(w, fkw("attention"), seeAlso)
	}
	if len(cfg.roles) == 0 {
		if !skipComments {
			fmt.Fprintln(w, "# no roles defined")
		}
	} else {
		for _, rn := range cfg.roleNames {
			r := cfg.roles[rn]
			fmt.Fprintf(w, "%s %s\n", fkw("role"), frn(r.name))
			if r.cleanupCmd != "" {
				fmt.Fprintf(w, "  %s %s\n", fkw("cleanup"), fsh(string(r.cleanupCmd)))
			}
			if r.spotlightCmd != "" {
				fmt.Fprintf(w, "  %s %s\n", fkw("spotlight"), fsh(string(r.spotlightCmd)))
			}
			for _, rp := range r.sigParsers {
				fmt.Fprintf(w, "  %s %s\n", fkw("signal"), rp.fmt(fsn, fkw, fre))
			}
			for _, actName := range r.actionNames {
				cmd := r.actionCmds[actName]
				fmt.Fprintf(w, "  :%s %s\n", facn(actName), fsh(string(cmd)))
			}
			fmt.Fprintln(w, fkw("end"))
			// fmt.Fprintln(w)
		}
	}
	if len(cfg.actors) == 0 {
		if !skipComments {
			fmt.Fprintln(w, "# no cast defined")
		}
	} else {
		fmt.Fprintln(w, fkw("cast"))
		for _, an := range cfg.actorNames {
			a := cfg.actors[an]
			fmt.Fprintf(w, "  %s %s %s", fan(a.name), fkw("plays"), frn(a.role.name))
			if a.extraEnv != "" {
				fmt.Fprintf(w, " %s %s", fkw("with"), fsh(a.extraEnv))
			}
			fmt.Fprintln(w)
			if !skipComments {
				fmt.Fprintf(w, "  # %s plays from working directory %s\n", fan(a.name), a.workDir)
			}
			for _, sig := range a.sinkNames {
				sink := a.sinks[sig]
				plural := "s"
				if strings.HasSuffix(sig, "s") {
					plural = ""
				}
				if !skipComments {
					if len(sink.observers) > 0 {
						fmt.Fprintf(w, "  # %s %s%s are watched by audience %s\n", fan(a.name), fsn(sig), plural, strings.Join(sink.observers, ", "))
					}
				}
			}
		}
		fmt.Fprintln(w, fkw("end"))
		// fmt.Fprintln(w)
	}

	fmt.Fprintln(w, fkw("script"))
	fmt.Fprintf(w, "  %s %s\n", fkw("tempo"), cfg.tempo)
	for _, aan := range cfg.sceneSpecChars {
		sc := cfg.sceneSpecs[aan]
		for _, ag := range sc.entails {
			fmt.Fprintf(w, "  %s %s %s %s: ", fkw("scene"), sc.name, fkw("entails for"), fan(ag.actor.name))
			comma := ""
			for _, a := range ag.actions {
				fmt.Fprint(w, comma)
				comma = "; "
				fmt.Fprint(w, facn(a))
			}
			fmt.Fprintln(w)
		}
		if sc.moodStart != "" {
			fmt.Fprintf(w, "  %s %s %s %s\n", fkw("scene"), sc.name, fkw("mood starts"), sc.moodStart)
		}
		if sc.moodEnd != "" {
			fmt.Fprintf(w, "  %s %s %s %s\n", fkw("scene"), sc.name, fkw("mood ends"), sc.moodEnd)
		}
	}
	if len(cfg.storyLine) == 0 {
		if !skipComments {
			fmt.Fprintln(w, "  # no storyline defined, play will terminate immediately")
		}
	} else {
		if len(cfg.storyLine) > 0 {
			fmt.Fprintf(w, "  %s %s\n", fkw("storyline"), strings.Join(cfg.storyLine, " "))
		}
		if cfg.repeatFrom != nil {
			fmt.Fprintf(w, "  %s %s\n", fkw("repeat from"), cfg.repeatFrom.String())
			if cfg.repeatActNum > 0 {
				fmt.Fprintf(w, "  # (repeating act %d and following)\n", cfg.repeatActNum)
			} else {
				fmt.Fprintln(w, "  # (no matching act, nothing is repeated)")
			}
		}
	}
	fmt.Fprintln(w, fkw("end"))
	// fmt.Fprintln(w)

	var interpretation bytes.Buffer
	if len(cfg.audience) == 0 {
		if !skipComments {
			fmt.Fprintln(w, "# no audience defined")
		}
	} else {
		fmt.Fprintln(w, fkw("audience"))
		if !skipComments {
			for _, vn := range cfg.varNames {
				v := cfg.vars[vn]
				qual := ""
				if v.isArray {
					qual = "(collection)"
				}
				for _, watcherName := range v.watcherNames {
					fmt.Fprintf(w, "  # %s sensitive to %s%s\n", fann(watcherName), vn.fmt(fan, fsn), qual)
				}
			}
		}
		for _, an := range cfg.audienceNames {
			a := cfg.audience[an]
			if a.auditor.activeCond.src != "" {
				if a.auditor.activeCond.src == "true" {
					fmt.Fprintf(w, "  %s %s\n", fann(a.name), fkw("audits throughout"))
				} else {
					fmt.Fprintf(w, "  %s %s %s\n", fann(a.name), fkw("audits only while"), fre(a.auditor.activeCond.src))
				}
			}
			for _, as := range a.auditor.assignments {
				fmt.Fprintf(w, "  %s %s\n", fann(a.name), as.fmt(fkw, fsn, fmod, fre))
			}
			if a.auditor.expectFsm != nil {
				fmt.Fprintf(w, "  %s %s %s: %s\n", fann(a.name), fkw("expects"), fmod(a.auditor.expectFsm.name), fre(a.auditor.expectExpr.src))
				a.auditor.fmtFoul(&interpretation, fkw, fann)
			}
			for _, varName := range a.observer.obsVarNames {
				fmt.Fprintf(w, "  %s %s %s\n", fann(a.name), fkw("watches"), varName.fmt(fan, fsn))
			}
			if a.observer.ylabel != "" {
				fmt.Fprintf(w, "  %s %s %s\n", fann(a.name), fkw("measures"), a.observer.ylabel)
			}
			if a.observer.disablePlot {
				fmt.Fprintf(w, "  %s %s\n", fann(a.name), fkw("only helps"))
			}
		}
		fmt.Fprintln(w, fkw("end"))
	}
	if interpretation.Len() > 0 {
		fmt.Fprintln(w, "interpretation")
		w.Write(interpretation.Bytes())
		fmt.Fprintln(w, "end")
	}
}

func fid(n string) string { return n }

func fw(cat string) func(string) string {
	return func(n string) string {
		return fmt.Sprintf("<span class=%s>%s</span>", cat, html.EscapeString(n))
	}
}

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
	sigParsers []*sigParser
	sigNames   []string
}

func (r *role) clone(newName string) *role {
	newR := *r
	newR.actionNames = append([]string(nil), r.actionNames...)
	newR.sigParsers = append([]*sigParser(nil), r.sigParsers...)
	newR.sigNames = append([]string(nil), r.sigNames...)
	newR.actionCmds = make(map[string]cmd, len(r.actionCmds))
	for k, v := range r.actionCmds {
		newR.actionCmds[k] = v
	}
	newR.name = newName
	return &newR
}

// cmd is the type of a command that can be executed as
// effect of a play action.
type cmd string

// sigParser describes how to extract signals from a spotlight.
type sigParser struct {
	typ sigType
	// name is the name of this signal/source on each actor.
	name string
	// re is how to parse the signal/source to get data points.
	re *regexp.Regexp
	// reGroup is the regexp group holding the time stamp.
	reGroup string
	// timeLayout is how to parse the timestamp discovered by reGroup using time.Parse.
	timeLayout string
}

type sigType int

const (
	sigTypEvent sigType = iota
	sigTypScalar
	sigTypDelta
)

func (p *sigParser) fmt(fsn, fkw, fre func(string) string) string {
	var s string
	switch p.typ {
	case sigTypEvent:
		s = "event at"
	case sigTypScalar:
		s = "scalar at"
	case sigTypDelta:
		s = "delta at"
	default:
		return "<???parser>"
	}
	return fmt.Sprintf("%s %s %s", fsn(p.name), fkw(s), fre(p.re.String()))
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

func (a *action) fmt(fkw, facn func(string) string) string {
	switch a.typ {
	case nopAction:
		return fkw("nop")
	case ambianceAction:
		return fmt.Sprintf("%s %s", fkw("mood"), a.act)
	case doAction:
		qual := ""
		if a.failOk {
			qual = "?"
		}
		return fmt.Sprintf(":%s%s", facn(a.act), qual)
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

// audienceMember is a member of the audience.
type audienceMember struct {
	name     string
	observer observer
	auditor  auditor
}

// observer is the "collector" part of an audience member.
type observer struct {
	// obsVars is the set of signals/variables watched by this observer.
	obsVars     map[varName]*collectedSignal
	obsVarNames []varName

	// the y-axis label on plots.
	ylabel string
	// disablePlot is set for "only helps" observers.
	disablePlot bool
	// hasData indicates whether data was received for this audience.
	// It is used to omit the plot if there was no data.
	hasData bool
}

type collectedSignal struct {
	// hasData is set when at least one event was collected,
	// and is used by the plotter to omit the plot curve if there was no data.
	hasData bool
	// drawEvents determines the plot style.
	drawEvents bool
}

// variable is one of the variables active during an audition.
// It contains values either for a signal or for an auditor-defined variable.
type variable struct {
	name         varName
	isArray      bool
	watchers     map[string]*audienceMember
	watcherNames []string
}

// auditor is the "auditor" part of an audience member.
type auditor struct {
	name string

	// observedSignals is the set of actor signals
	// that this auditor is sensitive on.
	observedSignals map[varName]struct{}

	// activCond determines when this auditor wakes up.
	activeCond expr

	// assignments specifies what to compute when active.
	assignments []assignment

	// expectFsm and expectExpr are the expect condition, when active.
	expectFsm  *fsm
	expectExpr expr

	// foulOnBad/Good indicate how to translate the FSM good/bad
	// transition counts into an exit code.
	foulOnBad  foulCondition
	foulOnGood foulCondition

	// hasData is set to true the first time the collector
	// receives an audit violation/event for this auditor.
	hasData bool
}

type foulCondition int

const (
	foulIgnore foulCondition = iota
	foulUponNonZero
	foulUponZero
)

func (a *auditor) fmtFoul(w io.Writer, fkw, fann func(string) string) {
	switch a.foulOnBad {
	case foulIgnore:
		fmt.Fprintf(w, "  %s %s %s\n", fkw("ignore"), fann(a.name), fkw("disappointment"))
	case foulUponNonZero:
		fmt.Fprintf(w, "  %s %s %s\n", fkw("foul upon"), fann(a.name), fkw("disappointment"))
	case foulUponZero:
		fmt.Fprintf(w, "  %s %s %s\n", fkw("require"), fann(a.name), fkw("disappointment"))
	}
	switch a.foulOnGood {
	case foulIgnore:
		fmt.Fprintf(w, "  %s %s %s\n", fkw("ignore"), fann(a.name), fkw("satisfaction"))
	case foulUponNonZero:
		fmt.Fprintf(w, "  %s %s %s\n", fkw("foul upon"), fann(a.name), fkw("satisfaction"))
	case foulUponZero:
		fmt.Fprintf(w, "  %s %s %s\n", fkw("require"), fann(a.name), fkw("satisfaction"))
	}
}

// assignment describes one of the "collects" or "computes" clauses in
// an auditor.
type assignment struct {
	// targetVar is the variable to assign.
	targetVar string
	// source value.
	expr expr
	// assignMode is how to assign to the variable.
	assignMode assignMode
	N          int
}

func (s *assignment) fmt(fkw, fsn, fmod, fre func(string) string) string {
	var buf bytes.Buffer
	var mode string
	switch s.assignMode {
	case assignSingle:
		fmt.Fprintf(&buf, "%s %s %s %s", fkw("computes"), fsn(s.targetVar), fkw("as"), fre(s.expr.src))
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
	fmt.Fprintf(&buf, "%s %s %s %s %d %s", fkw("collects"), fsn(s.targetVar), fkw("as"), fmod(mode), s.N, fre(s.expr.src))
	return buf.String()
}

// assignMode describes one of the assignment types for "collects" or
// "computes" clauses.
type assignMode int

const (
	assignSingle  assignMode = iota // stores the value
	assignFirstN                    // first N values in array
	assignLastN                     // last N values in array
	assignTopN                      // top N values in array
	assignBottomN                   // bottom N values in array
)

type varName struct {
	actorName string
	sigName   string
}

func (e varName) String() string {
	if e.actorName == "" {
		return e.sigName
	}
	return e.actorName + " " + e.sigName
}

func (e varName) fmt(fan, fsn func(string) string) string {
	if e.actorName == "" {
		return fsn(e.sigName)
	}
	return fan(e.actorName) + " " + fsn(e.sigName)
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

	// badCnt, goodCnd are the bad/good event counts throughout the
	// audition. This is used to compute the exit code.
	badCnt, goodCnt int
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

// selectActors selects the actors (and the role) matching
// the target definition.
// - every <rolename>  -> every actor playing <rolename>
// - <actorname>       -> that specific actor
func (cfg *config) selectActors(target string) (*role, []*actor, error) {
	if roleName, ok := withPrefix(target, "every "); ok {
		var err error
		roleName, err = cfg.preprocReplace(roleName)
		if err != nil {
			return nil, nil, err
		}
		if err := checkIdent(roleName); err != nil {
			return nil, nil, err
		}
		r, ok := cfg.roles[roleName]
		if !ok {
			return nil, nil, explainAlternatives(
				errors.Newf("unknown role %q", roleName), "roles", cfg.roles)
		}
		var res []*actor
		for _, an := range cfg.actorNames {
			a := cfg.actors[an]
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
		return nil, nil, explainAlternatives(
			errors.Newf("unknown actor %q", target), "actors", cfg.actors)
	}
	return act.role, []*actor{act}, nil
}

func (cfg *config) addOrGetAudienceMember(name string) *audienceMember {
	a, ok := cfg.audience[name]
	if !ok {
		a = &audienceMember{
			name: name,
			observer: observer{
				obsVars: make(map[varName]*collectedSignal),
			},
			auditor: auditor{
				name:       name,
				foulOnBad:  foulUponNonZero,
				foulOnGood: foulIgnore,
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
			return rp.typ == sigTypEvent, true
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
func (a *audienceMember) addOrUpdateSignalSource(r *role, vr varName) error {
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

// maybeAddVar adds a variable if it does not exist.
// If it already exists and dupOk is set, it is a no-op.
// Otherwise an error is returned.
func (cfg *config) maybeAddVar(a *audienceMember, vn varName, dupOk bool) error {
	v, ok := cfg.vars[vn]
	if !ok {
		v = &variable{
			name:     vn,
			watchers: make(map[string]*audienceMember),
		}
		cfg.vars[vn] = v
		cfg.varNames = append(cfg.varNames, vn)
	} else {
		if !dupOk {
			return errors.Newf("variable already defined: %q", vn)
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
	v.watchers[watcher.name] = watcher
	v.watcherNames = append(v.watcherNames, watcher.name)
}

func (cfg *config) maybeAddSceneSpec(shortHand string) *sceneSpec {
	b := shortHand[0]
	if sc, ok := cfg.sceneSpecs[b]; ok {
		return sc
	}
	sc := &sceneSpec{name: shortHand}
	cfg.sceneSpecs[b] = sc
	cfg.sceneSpecChars = append(cfg.sceneSpecChars, b)
	return sc
}

func (cfg *config) validateStoryLine(storyLine string) ([]string, error) {
	parts := strings.Split(storyLine, " ")
	newParts := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ReplaceAll(strings.TrimSpace(part), "_", "")
		if part == "" {
			continue
		}
		actNum := len(newParts) + 1
		for i := 0; i < len(part); i++ {
			scChar := part[i]
			switch scChar {
			case ' ', '.':
				continue
			case '+':
				if i == 0 {
					return nil, errors.Newf("in act %d: cannot use + at beginning of act", actNum)
				}
				if i == len(part)-1 {
					return nil, errors.Newf("in act %d: cannot use + at end of act", actNum)
				}
				if part[i-1] == '+' {
					return nil, errors.Newf("in act %d: sequence ++ is invalid", actNum)
				}
			default:
				if _, ok := cfg.sceneSpecs[scChar]; !ok {
					return nil, explainAlternativesList(
						errors.Newf("in act %d, scene %c not defined", actNum, scChar),
						"scenes", bytesToStrings(cfg.sceneSpecChars)...)
				}
			}
		}
		newParts = append(newParts, part)
	}
	return newParts, nil
}

func (cfg *config) updateRepeat() {
	if cfg.repeatFrom == nil {
		return
	}
	for i, part := range cfg.storyLine {
		// Find the repetition point.
		if cfg.repeatFrom.MatchString(part) {
			cfg.repeatActNum = i + 1
			break
		}
	}
}

// bytesToStrings convers an array of bytes to an array of strings,
// this is used to propose alternatives upon encountering an error.
func bytesToStrings(bs []byte) []string {
	res := make([]string, len(bs))
	for i, b := range bs {
		res[i] = fmt.Sprintf("%c", b)
	}
	return res
}

func escapeNl(s string) string { return strings.ReplaceAll(s, "\n", "\\\n") }
