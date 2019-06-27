package cmd

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cockroachdb/datadriven"
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

func TestParse(t *testing.T) {
	includePath := []string{filepath.Join("testdata", "include")}
	datadriven.Walk(t, filepath.Join("testdata", "parse"), func(t *testing.T, path string) {
		if strings.HasSuffix(path, "~") || strings.HasPrefix(path, ".#") {
			return
		}
		datadriven.RunTest(t, path, func(d *datadriven.TestData) string {
			// First round of parse.
			rd, err := newReaderFromString("<testdata>", d.Input)
			if err != nil {
				t.Fatal(err)
			}
			rd.includePath = includePath
			defer rd.close()
			cfg := newConfig()
			if err := cfg.parseCfg(context.TODO(), rd); err != nil {
				return fmt.Sprintf("parse error: %s\n", renderError(err))
			}
			var out bytes.Buffer
			cfg.printCfg(&out, false, false)
			// FIXME: remove this when satisfied
			if len(cfg.stanzas) > 0 {
				if err := cfg.compileV1(); err != nil {
					return fmt.Sprintf("compile error: %s\n", renderError(err))
				}
				cfg.printSteps(&out, false)
			}
			if len(cfg.storyLine) > 0 {
				if err := cfg.compileV2(); err != nil {
					return fmt.Sprintf("compile error: %s\n", renderError(err))
				}
				cfg.printSteps(&out, false)
			}

			if out.String() != d.Expected {
				// Shortcut, don't even bother parsing a second time.
				return out.String()
			}

			// Second round of parse.
			rd2, err := newReaderFromString("<testdata-reparse>", out.String())
			if err != nil {
				t.Fatal(err)
			}
			rd2.includePath = includePath
			cfg = newConfig()
			if err := cfg.parseCfg(context.TODO(), rd2); err != nil {
				t.Fatalf("reparse error: %s\n", renderError(err))
			}
			var out2 bytes.Buffer
			cfg.printCfg(&out2, false, false)

			if len(cfg.stanzas) > 0 {
				if err := cfg.compileV1(); err != nil {
					t.Fatalf("compile error after reparse: %s\n", renderError(err))
				}
				cfg.printSteps(&out2, false)
			}
			if len(cfg.storyLine) > 0 {
				if err := cfg.compileV2(); err != nil {
					return fmt.Sprintf("compile error after reparse: %s\n", renderError(err))
				}
				cfg.printSteps(&out2, false)
			}
			if out.String() != out2.String() {
				t.Errorf("parse/print is not idempotent; got:\n  %s\nexpected:\n  %s",
					strings.ReplaceAll(out2.String(), "\n", "\n  "),
					strings.ReplaceAll(out.String(), "\n", "\n  "),
				)
			}
			return out.String()
		})
	})
}

func renderError(err error) string {
	var buf bytes.Buffer
	RenderError(&buf, err)
	return buf.String()
}
