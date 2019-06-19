package cmd

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
)

func TestParse(t *testing.T) {
	includePath := []string{filepath.Join("testdata", "include")}
	datadriven.Walk(t, filepath.Join("testdata", "parse"), func(t *testing.T, path string) {
		if strings.HasSuffix(path, "~") || strings.HasPrefix(path, ".#") {
			return
		}
		datadriven.RunTest(t, path, func(d *datadriven.TestData) string {
			rd, err := newReaderFromString("<testdata>", d.Input)
			rd.includePath = includePath
			if err != nil {
				t.Fatal(err)
			}
			cfg := newConfig()
			if err := cfg.parseCfg(context.TODO(), rd); err != nil {
				return fmt.Sprintf("parse error: %s\n", renderError(err))
			}
			var out bytes.Buffer
			cfg.printCfg(&out)
			if len(cfg.stanzas) > 0 {
				if err := cfg.compile(); err != nil {
					return fmt.Sprintf("compile error: %s\n", renderError(err))
				}
				cfg.printSteps(&out)
			}
			return out.String()
		})
	})
}

func renderError(err error) string {
	var buf bytes.Buffer
	buf.WriteString(err.Error())
	if d := errors.FlattenDetails(err); d != "" {
		fmt.Fprintf(&buf, "\n--\n%s", d)
	}
	if d := errors.FlattenHints(err); d != "" {
		fmt.Fprintf(&buf, "\nHINT: %s", d)
	}
	return buf.String()
}
