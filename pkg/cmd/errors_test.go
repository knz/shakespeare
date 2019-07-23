package cmd

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/errors/testutils"
)

func TestFormatError(t *testing.T) {
	tt := testutils.T{t}

	coll := errorCollection{
		errs: []error{
			errors.WithHint(errors.New("waa"), "woo"),
			errors.New("woo"),
		},
	}

	ref := `2 errors:
(1) waa
    HINT: woo
(2) woo`
	actual := coll.Error()
	tt.CheckEqual(actual, ref)

	refV := `2 errors:
    (1) waa
        HINT: woo
    (2) woo:
    -- details follow
    (1) error with user hint: woo
      - error with attached stack trace:
        <path>
        <tab><path>
        testing.tRunner
        <tab><path>
        runtime.goexit
        <tab><path>
      - waa
    (2) error with attached stack trace:
        <path>
        <tab><path>
        testing.tRunner
        <tab><path>
        runtime.goexit
        <tab><path>
      - woo`
	spv := fmt.Sprintf("%+v", &coll)
	spv = fileref.ReplaceAllString(spv, "<path>")
	spv = strings.ReplaceAll(spv, "\t", "<tab>")
	tt.CheckEqual(spv, refV)
}

var fileref = regexp.MustCompile(`[a-zA-Z0-9\._-]*/[a-zA-Z0-9\._/-]*\.(?:(?:go|s):\d+|[A-Za-z]*)`)
