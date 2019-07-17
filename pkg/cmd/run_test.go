package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/datadriven"
	"github.com/knz/shakespeare/pkg/crdb/leaktest"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

func TestRun(t *testing.T) {
	includePath := []string{filepath.Join("testdata", "include"), ""}
	datadriven.Walk(t, filepath.Join("testdata", "run"), func(t *testing.T, path string) {
		if strings.HasSuffix(path, "~") ||
			strings.HasPrefix(path, ".#") ||
			strings.HasSuffix(path, ".cfg") {
			return
		}
		includePath[1] = filepath.Dir(path)
		datadriven.RunTest(t, path, func(d *datadriven.TestData) string {
			defer leaktest.AfterTest(t)()

			// Prepare a log scope for the test.
			sc := log.Scope(t)
			defer sc.Close(t)

			// Parse+compile the configuration.
			rd, err := newReaderFromString("<testdata>", d.Input)
			if err != nil {
				t.Fatal(err)
			}
			rd.includePath = includePath
			defer rd.close()
			cfg := newConfig()

			// Make the artifacts, etc. go to a separate directory for this test.
			workDir, err := ioutil.TempDir("", "shakespeare-test")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if !t.Failed() {
					if err := os.RemoveAll(workDir); err != nil {
						t.Error(err)
					}
				} else {
					t.Logf("artifacts and logs to be found in: %s", workDir)
				}
			}()
			cfg.dataDir = workDir
			cfg.subDir = "."

			// Override the shell, to make error messages deterministic.
			cfg.shellPath = "bash"

			if err := cfg.parseCfg(context.TODO(), rd); err != nil {
				t.Fatalf("parse error: %s\n", renderError(err))
			}
			if err := cfg.compileV2(); err != nil {
				t.Fatalf("compile error: %s\n", renderError(err))
			}

			// Disable plotting.
			cfg.skipPlot = true
			// Disable non-determistic narration output.
			cfg.avoidTimeProgress = true

			// Ensure the test terminates in a timely manner.
			ctx, bye := context.WithDeadline(context.TODO(), timeutil.Now().Add(5*time.Second))
			defer bye()

			// Write the narration in memory.
			var out bytes.Buffer
			cfg.narration = &out

			err = cfg.run(ctx)

			if len(d.CmdArgs) > 0 && d.CmdArgs[0].Key == "error" {
				if err == nil {
					t.Errorf("test:\n  %s\nexpected error, got success",
						strings.ReplaceAll(d.Input, "\n", "\n  "),
					)
				} else {
					fmt.Fprintf(&out, "run error: %s\n", renderError(err))
				}
			} else {
				if err != nil {
					t.Errorf("expected success, got error: %+v", err)
				}
			}

			return out.String()
		})
	})
}
