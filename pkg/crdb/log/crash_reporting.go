// Copyright 2013 Google Inc. All Rights Reserved.
// Copyright 2017 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This code originated in the github.com/golang/glog package.

package log

import (
	"context"
	"fmt"
	"runtime/debug"
)

// ReportPanic reports a panic has occurred on the real stderr.
func ReportPanic(ctx context.Context, r interface{}) {
	Shout(ctx, Severity_ERROR, "a panic has occurred!")

	if stderrRedirected {
		// We do not use Shout() to print the panic details here, because
		// if stderr is not redirected (e.g. when logging to file is
		// disabled) Shout() would copy its argument to stderr
		// unconditionally, and we don't want that: Go's runtime system
		// already unconditonally copies the panic details to stderr.
		// Instead, we copy manually the details to stderr, only when stderr
		// is redirected to a file otherwise.
		fmt.Fprintf(OrigStderr, "%v\n\n%s\n", r, debug.Stack())
	} else {
		// If stderr is not redirected, then Go's runtime will only print
		// out the panic details to the original stderr, and we'll miss a copy
		// in the log file. Produce it here.
		logging.printPanicToFile(r)
	}

	// Ensure that the logs are flushed before letting a panic
	// terminate the server.
	Flush()
}
