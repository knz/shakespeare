package cmd

import (
	"bytes"
	"context"
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

func TestPlot(t *testing.T) {
	t.Skip("remove the t.Skip call and compare the test output manually")

	includePath := []string{filepath.Join("testdata", "include"), ""}
	datadriven.Walk(t, filepath.Join("testdata", "plot"), func(t *testing.T, path string) {
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

			// Override the shell, to make error messages deterministic.
			cfg.shellPath = "bash"

			if err := cfg.parseCfg(context.TODO(), rd); err != nil {
				t.Fatalf("parse error: %s\n", renderError(err))
			}
			if err := cfg.compileV2(); err != nil {
				t.Fatalf("compile error: %s\n", renderError(err))
			}

			// Make plots monochrome to avoid escape codes in the test output.
			cfg.textPlotTerm = "mono"
			// Contract horizontally to avoid some non-determinism.
			cfg.textPlotWidth = 40
			// Contract vertically because we don't need much details for tests.
			cfg.textPlotHeight = 17

			// Ensure the test terminates in a timely manner.
			ctx, bye := context.WithDeadline(context.TODO(), timeutil.Now().Add(5*time.Second))
			defer bye()

			// Write the narration in memory.
			var out bytes.Buffer
			cfg.narration = &out

			if err = cfg.run(ctx); err != nil {
				t.Fatalf("%s\nrun error: %+v", out.String(), err)
			}

			plotData, err := ioutil.ReadFile(filepath.Join(cfg.dataDir, "plot.txt"))
			if err != nil {
				t.Fatal(err)
			}
			return trimLines(string(plotData))
		})
	})
}

func trimLines(data string) string {
	// Remove any trailing whitespace on each line.
	lines := strings.Split(data, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \f")
	}
	return strings.Join(lines, "\n")
}
