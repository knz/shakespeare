// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package log

import (
	"context"
	"fmt"
	"strings"

	"github.com/knz/shakespeare/pkg/crdb/caller"
	"github.com/knz/shakespeare/pkg/crdb/log/logtags"
)

// FormatTags appends the tags to a strings.Builder. If there are no tags,
// returns false.
func FormatTags(ctx context.Context, buf *strings.Builder) bool {
	tags := logtags.FromContext(ctx)
	if tags == nil {
		return false
	}
	buf.WriteByte('[')
	for i, t := range tags.Get() {
		if i > 0 {
			buf.WriteByte(',')
		}

		buf.WriteString(t.Key())
		if v := t.Value(); v != nil && v != "" {
			// For tags that have a value and are longer than a character,
			// we output "tag=value". For one character tags we don't use a
			// separator (e.g. "n1").
			if len(t.Key()) > 1 {
				buf.WriteByte('=')
			}
			fmt.Fprint(buf, v)
		}
	}
	buf.WriteString("] ")
	return true
}

// MakeMessage creates a structured log entry.
func MakeMessage(ctx context.Context, format string, args []interface{}) string {
	var buf strings.Builder
	FormatTags(ctx, &buf)
	if len(args) == 0 {
		buf.WriteString(format)
	} else if len(format) == 0 {
		fmt.Fprint(&buf, args...)
	} else {
		fmt.Fprintf(&buf, format, args...)
	}
	return buf.String()
}

// addStructured creates a structured log entry to be written to the
// specified facility of the logger.
func addStructured(ctx context.Context, s Severity, depth int, format string, args []interface{}) {
	file, line, _ := caller.Lookup(depth + 1)
	msg := MakeMessage(ctx, format, args)

	// MakeMessage already added the tags when forming msg, we don't want
	// eventInternal to prepend them again.
	logging.outputLogEntry(s, file, line, msg)
}
