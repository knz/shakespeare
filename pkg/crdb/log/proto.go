// Copyright 2019 The Cockroach Authors.
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

import "github.com/gogo/protobuf/proto"

type Severity int32

const (
	Severity_UNKNOWN Severity = 0
	Severity_INFO    Severity = 1
	Severity_WARNING Severity = 2
	Severity_ERROR   Severity = 3
	Severity_FATAL   Severity = 4
	// NONE is used to specify when no messages
	// should be printed to the log file or stderr.
	Severity_NONE Severity = 5
	// DEFAULT is the end sentinel. It is used during command-line
	// handling to indicate that another value should be replaced instead
	// (depending on which command is being run); see cli/flags.go for
	// details.
	Severity_DEFAULT Severity = 6
)

var Severity_name = map[int32]string{
	0: "UNKNOWN",
	1: "INFO",
	2: "WARNING",
	3: "ERROR",
	4: "FATAL",
	5: "NONE",
	6: "DEFAULT",
}
var Severity_value = map[string]int32{
	"UNKNOWN": 0,
	"INFO":    1,
	"WARNING": 2,
	"ERROR":   3,
	"FATAL":   4,
	"NONE":    5,
	"DEFAULT": 6,
}

func (x Severity) String() string {
	return proto.EnumName(Severity_name, int32(x))
}

// Entry represents a cockroach structured log entry.
type Entry struct {
	Severity  Severity // Nanoseconds since the epoch.
	Time      int64
	Goroutine int64
	File      string
	Line      int64
	Message   string
}

// A FileDetails holds all of the particulars that can be parsed by the name of
// a log file.
type FileDetails struct {
	Program  string
	Host     string
	UserName string
	Time     int64
	PID      int64
}

type FileInfo struct {
	Name         string
	SizeBytes    int64
	ModTimeNanos int64
	Details      FileDetails
}
