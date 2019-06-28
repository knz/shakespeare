package cmd

import (
	"fmt"
	"testing"
)

func TestExtract(t *testing.T) {
	testData := []struct {
		s      string
		istart int
		exp    string
		ifinal int
	}{
		{"", 0, "", 0},
		{"", 123, "", 123},
		{"a", 0, "a", 1},
		{"ab", 0, "a", 1},
		{"a+b", 0, "a+b", 3},
		{"a+b+c", 0, "a+b+c", 5},
		{"da+b", 1, "a+b", 4},
	}

	for _, test := range testData {
		t.Run(fmt.Sprintf("%s/%d", test.s, test.istart), func(t *testing.T) {
			r, i := extractAction(test.s, test.istart)
			got := fmt.Sprintf("%q/%d", r, i)
			ref := fmt.Sprintf("%q/%d", test.exp, test.ifinal)
			if got != ref {
				t.Errorf("expected %s, got %s", ref, got)
			}
		})
	}

}

func TestCombine(t *testing.T) {
	testData := []struct {
		a1, a2 string
		exp    string
	}{
		{"", "", ""},
		{"a", "", "a"},
		{"", "a", "a"},
		{".", "", "."},
		{"", ".", "."},
		{".", "a", "a"},
		{"a", ".", "a"},
		{".a", "a.", "aa"},
		{"a", "b", "a+b"},
		{"a+bc", "d", "a+b+dc"},
		{"a+bc", "d", "a+b+dc"},
		{"ab+c", "d", "a+db+c"},
	}

	for _, test := range testData {
		t.Run(fmt.Sprintf("%s/%s", test.a1, test.a2), func(t *testing.T) {
			r := combineActs(test.a1, test.a2)
			if r != test.exp {
				t.Errorf("expected %q, got %q", test.exp, r)
			}
		})
	}
}
