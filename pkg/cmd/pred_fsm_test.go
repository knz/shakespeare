package cmd

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestPrintFsm(t *testing.T) {
	fsm := fsm{
		name:       "woo",
		startState: 2,
		edges: [][]int{
			0: []int{1, 3, 0},
			1: []int{2, 0, 0},
			2: []int{1, 1, 2},
			3: []int{2, 1, 0},
		},
		stateNames: []string{"as", "b", "c", "d"},
		labels:     []string{"t", "f", "N"},
	}

	t.Run("matrix", func(t *testing.T) {

		ref := `# woo
    t  f  N
as  b  d  as
b   c  as as
c*  b  b  c
d   c  b  as
`

		var w bytes.Buffer
		fsm.printMatrix(&w)
		ref = strings.TrimPrefix(ref, "\n")
		if w.String() != ref {
			t.Errorf("expected:\n%s\ngot:\n%s", ref, w.String())
		}
	})

	t.Run("graphviz-dot", func(t *testing.T) {

		ref := `// woo
digraph G {
  as;  b;  c [peripheries=2];  d;
  as -> b [label="t"];
  as -> d [label="f"];
  as -> as [label="N"];
  b -> c [label="t"];
  b -> as [label="f"];
  b -> as [label="N"];
  c -> b [label="t"];
  c -> b [label="f"];
  c -> c [label="N"];
  d -> c [label="t"];
  d -> b [label="f"];
  d -> as [label="N"];
}
`

		var w bytes.Buffer
		fsm.printDot(&w)
		if w.String() != ref {
			t.Errorf("expected:\n%s\ngot:\n%s", ref, w.String())
		}
	})

}

func TestReferenceFsms(t *testing.T) {
	testData := []struct {
		f   *fsm
		exp string
	}{
		{&alwaysFsm, `# always
       t     f     e:t   e:f  reset
start* start bad   good  bad  start
good   good  good  good  good start
bad    bad   bad   bad   bad  start
`},
		{&neverFsm, `# never
       t     f     e:t   e:f   reset
start* start good  bad   good  start
bad    bad   bad   bad   bad   start
good   good  good  good  good  start
`},
		{&notAlwaysFsm, `# not always
       t     f     e:t   e:f   reset
start* start good  bad   good  start
good   good  good  good  good  start
bad    bad   bad   bad   bad   start
`},
		{&eventuallyFsm, `# eventually
       t     f     e:t   e:f  reset
start* good  start good  bad  start
good   good  good  good  good start
bad    bad   bad   bad   bad  start
`},
		{&eventuallyAlwaysFsm, `# eventually always
           t         f         e:t       e:f   reset
start*     activated start     good      bad   start
activated  activated bad       good      bad   activated
good       good      good      good      good  activated
bad        bad       bad       bad       bad   activated
`},
		{&alwaysEventuallyFsm, `# always eventually
       t     f     e:t   e:f   reset
start* start start good  bad   start
good   good  good  good  good  start
bad    bad   bad   bad   bad   start
`},
		{&onceFsm, `# once
        t      f      e:t    e:f    reset
notyet* good   notyet good   bad    notyet
good    bad    good   bad    good   good
bad     bad    bad    bad    bad    good
`},
		{&twiceFsm, `# twice
        t      f      e:t    e:f    reset
notyet* once   notyet bad    bad    notyet
once    good   once   good   bad    notyet
good    bad    good   good   good   notyet
bad     bad    bad    bad    bad    good
`},
		{&thriceFsm, `# thrice
        t      f      e:t    e:f    reset
notyet* once   notyet bad    bad    notyet
once    twice  once   bad    bad    notyet
twice   good   twice  good   bad    notyet
good    bad    good   good   good   notyet
bad     bad    bad    bad    bad    good
`},
	}

	sp := regexp.MustCompile(`(\s+)`)
	for _, test := range testData {
		t.Run(test.f.name, func(t *testing.T) {
			var w bytes.Buffer
			test.f.printMatrix(&w)
			ref := sp.ReplaceAllString(strings.TrimPrefix(test.exp, "\n"), " ")
			actual := sp.ReplaceAllString(w.String(), " ")
			if actual != ref {
				t.Errorf("expected:\n%s\ngot:\n%s", test.exp, w.String())
			}
		})
	}
}

func TestFsmBehavior(t *testing.T) {
	testData := []struct {
		f        *fsm
		input    []string
		expected string
	}{
		{&alwaysFsm, []string{"t", "t", "t", "e:t"},
			`start: start
t, -> start
t, -> start
t, -> start
e:t, -> good
`},
		{&alwaysFsm, []string{"t", "f", "t", "e:t"},
			`start: start
t, -> start
f, -> bad
t, -> bad
e:t, -> bad
`},
		{&eventuallyFsm, []string{"f", "f", "f", "e:t"},
			`start: start
f, -> start
f, -> start
f, -> start
e:t, -> good
`},
		{&eventuallyFsm, []string{"f", "t", "f", "e:f"},
			`start: start
f, -> start
t, -> good
f, -> good
e:f, -> good
`},
		{&eventuallyFsm, []string{"f", "f", "f", "e:f"},
			`start: start
f, -> start
f, -> start
f, -> start
e:f, -> bad
`},
	}

	for _, test := range testData {
		t.Run(test.f.name, func(t *testing.T) {
			var buf bytes.Buffer
			eval := makeFsmEval(test.f)
			fmt.Fprintf(&buf, "start: %s\n", eval.state())
			for _, step := range test.input {
				eval.advance(step)
				fmt.Fprintf(&buf, "%s, -> %s\n", step, eval.state())
			}
			if buf.String() != test.expected {
				t.Errorf("expected:\n%s\ngot:\n%s", test.expected, buf.String())
			}
		})
	}
}
