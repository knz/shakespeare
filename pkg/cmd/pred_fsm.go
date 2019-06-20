package cmd

import (
	"fmt"
	"io"

	"github.com/cockroachdb/errors"
)

type fsm struct {
	name       string
	startState int
	edges      [][]int
	stateNames []string
	labels     []string
}

type fsmEval struct {
	curState int
	labelMap map[string]int
	fsm      *fsm
}

func makeFsmEval(f *fsm) fsmEval {
	labelMap := make(map[string]int, len(f.labels))
	for i, label := range f.labels {
		labelMap[label] = i
	}
	return fsmEval{curState: f.startState, fsm: f, labelMap: labelMap}
}

func (e *fsmEval) state() string {
	return e.fsm.stateNames[e.curState]
}

func (e *fsmEval) advance(label string) {
	l, ok := e.labelMap[label]
	if !ok {
		panic(errors.WithDetailf(
			errors.Newf("label not defined: %q", label),
			"available labels: %+v", e.fsm.labels))
	}
	e.curState = e.fsm.edges[e.curState][l]
}

var automata = func() map[string]*fsm {
	r := make(map[string]*fsm)
	for _, f := range []*fsm{
		&alwaysFsm,
		&notAlwaysFsm,
		&eventuallyFsm,
		&alwaysEventuallyFsm,
		&eventuallyAlwaysFsm,
		&onceFsm,
		&twiceFsm,
		&thriceFsm,
	} {
		r[f.name] = f
	}
	return r
}()

var onceFsm = fsm{
	name:       "once",
	startState: 0,
	stateNames: []string{"notyet", "good", "bad"},
	labels:     []string{"t", "f", "e:t", "e:f", "reset"},
	edges: [][]int{
		0: []int{1, 0, 1, 2, 0}, // notyet
		1: []int{2, 1, 2, 1, 1}, // good
		2: []int{2, 2, 2, 2, 1}, // bad
	},
}

var twiceFsm = fsm{
	name:       "twice",
	startState: 0,
	stateNames: []string{"notyet", "once", "good", "bad"},
	labels:     []string{"t", "f", "e:t", "e:f", "reset"},
	edges: [][]int{
		0: []int{1, 0, 3, 3, 0}, // notyet
		1: []int{2, 1, 2, 3, 0}, // once
		2: []int{3, 2, 2, 2, 0}, // good
		3: []int{3, 3, 3, 3, 2}, // bad
	},
}

var thriceFsm = fsm{
	name:       "thrice",
	startState: 0,
	stateNames: []string{"notyet", "once", "twice", "good", "bad"},
	labels:     []string{"t", "f", "e:t", "e:f", "reset"},
	edges: [][]int{
		0: []int{1, 0, 4, 4, 0}, // notyet
		1: []int{2, 1, 4, 4, 0}, // once
		2: []int{3, 2, 3, 4, 0}, // twice
		3: []int{4, 3, 3, 3, 0}, // good
		4: []int{4, 4, 4, 4, 3}, // bad
	},
}

var notAlwaysFsm = fsm{
	name:       "not always",
	startState: 0,
	stateNames: []string{"start", "good", "bad"},
	labels:     []string{"t", "f", "e:t", "e:f", "reset"},
	edges: [][]int{
		0: []int{0, 1, 2, 1, 0}, // start
		1: []int{1, 1, 1, 1, 0}, // good
		2: []int{2, 2, 2, 2, 0}, // bad
	},
}

var alwaysFsm = fsm{
	name:       "always",
	startState: 0,
	stateNames: []string{"start", "good", "bad"},
	labels:     []string{"t", "f", "e:t", "e:f", "reset"},
	edges: [][]int{
		0: []int{0, 2, 1, 2, 0}, // start
		1: []int{1, 1, 1, 1, 0}, // good
		2: []int{2, 2, 2, 2, 0}, // bad
	},
}

var eventuallyFsm = fsm{
	name:       "eventually",
	startState: 0,
	stateNames: []string{"start", "good", "bad"},
	labels:     []string{"t", "f", "e:t", "e:f", "reset"},
	edges: [][]int{
		0: []int{1, 0, 1, 2, 0}, // start
		1: []int{1, 1, 1, 1, 0}, // good
		2: []int{2, 2, 2, 2, 0}, // bad
	},
}

var alwaysEventuallyFsm = fsm{
	name:       "always eventually",
	startState: 0,
	stateNames: []string{"start", "good", "bad"},
	labels:     []string{"t", "f", "e:t", "e:f", "reset"},
	edges: [][]int{
		0: []int{0, 0, 1, 2, 0}, // start
		1: []int{1, 1, 1, 1, 0}, // good
		2: []int{2, 2, 2, 2, 0}, // bad
	},
}

var eventuallyAlwaysFsm = fsm{
	name:       "eventually always",
	startState: 0,
	stateNames: []string{"start", "activated", "good", "bad"},
	labels:     []string{"t", "f", "e:t", "e:f", "reset"},
	edges: [][]int{
		0: []int{1, 0, 2, 3, 0}, // start
		1: []int{1, 3, 2, 3, 1}, // activated
		2: []int{2, 2, 2, 2, 1}, // good
		3: []int{3, 3, 3, 3, 1}, // bad
	},
}

func (f *fsm) printMatrix(w io.Writer) {
	// Make columns wide enough for state names.
	colWidth := 0
	for _, stateName := range f.stateNames {
		if colWidth < len(stateName) {
			colWidth = len(stateName)
		}
	}
	// Also for edge labels.
	for _, label := range f.labels {
		if colWidth < len(label) {
			colWidth = len(label)
		}
	}

	fmt.Fprintln(w, "#", f.name)
	// Print the edge labels at the top.
	fmt.Fprintf(w, "%*s ", colWidth, " ")
	for i, e := range f.labels {
		if i < len(f.labels)-1 {
			fmt.Fprintf(w, " %-*s", colWidth, e)
		} else {
			fmt.Fprintf(w, " %s", e)
		}
	}
	fmt.Fprintln(w)
	// Print all the states.
	for s, stateName := range f.stateNames {
		if f.startState == s {
			stateName += "*"
		}
		fmt.Fprintf(w, "%-*s", colWidth+1, stateName)
		nextStates := f.edges[s]
		for i, ns := range nextStates {
			if i < len(nextStates)-1 {
				fmt.Fprintf(w, " %-*s", colWidth, f.stateNames[ns])
			} else {
				fmt.Fprintf(w, " %s", f.stateNames[ns])
			}
		}
		fmt.Fprintln(w)
	}
}

func (f *fsm) printDot(w io.Writer) {
	fmt.Fprintln(w, "//", f.name)
	fmt.Fprintln(w, "digraph G {")
	// Print all the states.
	for s, stateName := range f.stateNames {
		fmt.Fprintf(w, "  %s", stateName)
		if f.startState == s {
			fmt.Fprint(w, " [peripheries=2]")
		}
		fmt.Fprint(w, ";")
	}
	fmt.Fprintln(w)
	for s, stateName := range f.stateNames {
		nextStates := f.edges[s]
		for e, ns := range nextStates {
			fmt.Fprintf(w, "  %s -> %s [label=\"%s\"];\n",
				stateName, f.stateNames[ns], f.labels[e])
		}
	}
	fmt.Fprintln(w, "}")
}
