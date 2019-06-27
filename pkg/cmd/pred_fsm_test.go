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
          t        f        end      reset
checking* checking bad      good     checking
good      good     good     good     checking
bad       bad      bad      bad      checking
`},
		{&neverFsm, `# never
          t        f        end      reset
checking* checking good     bad      checking
bad       bad      bad      bad      checking
good      good     good     good     checking
`},
		{&notAlwaysFsm, `# not always
       t     f     end   reset
start* start good  bad   start
good   good  good  good  start
bad    bad   bad   bad   start
`},
		{&eventuallyFsm, `# eventually
          t        f        end      reset
checking* good     checking bad      checking
good      good     good     good     checking
bad       bad      bad      bad      checking
`},
		{&eventuallyAlwaysFsm, `# eventually always
       t     f     end   reset
start* good  start bad   start
good   good  bad   good  start
bad    bad   bad   bad   start
`},
		{&alwaysEventuallyFsm, `# always eventually
           t         f         end       reset
start*     activated start     bad       start
activated  activated start     good      start
good       good      good      good      start
bad        bad       bad       bad       start
`},
		{&onceFsm, `# once
        t      f      end    reset
notyet* good   notyet bad    notyet
good    bad    good   good   good
bad     bad    bad    bad    good
`},
		{&twiceFsm, `# twice
        t      f      end    reset
notyet* once   notyet bad    notyet
once    good   once   bad    notyet
good    bad    good   good   notyet
bad     bad    bad    bad    good
`},
		{&thriceFsm, `# thrice
        t      f      end    reset
notyet* once   notyet bad    notyet
once    twice  once   bad    notyet
twice   good   twice  bad    notyet
good    bad    good   good   notyet
bad     bad    bad    bad    good
`},
		{&atMostOnceFsm, `# at most once
        t      f      end    reset
notyet* once   notyet good   notyet
once    bad    once   good   notyet
good    good   good   good   notyet
bad     bad    bad    bad    once
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
		{&alwaysFsm, []string{"t", "t", "t", "end"},
			`start: checking
t, -> checking
t, -> checking
t, -> checking
end, -> good
`},
		{&alwaysFsm, []string{"t", "f", "t", "end"},
			`start: checking
t, -> checking
f, -> bad
t, -> bad
end, -> bad
`},
		{&eventuallyFsm, []string{"f", "f", "f", "end"},
			`start: checking
f, -> checking
f, -> checking
f, -> checking
end, -> bad
`},
		{&eventuallyFsm, []string{"f", "t", "f", "end"},
			`start: checking
f, -> checking
t, -> good
f, -> good
end, -> good
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
