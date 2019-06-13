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

package logflags

import "flag"

// LogToStderrName and others are flag names.
const (
	LogToStderrName               = "logtostderr"
	NoColorName                   = "no-color"
	VModuleName                   = "vmodule"
	LogDirName                    = "log-dir"
	NoRedirectStderrName          = "no-redirect-stderr"
	ShowLogsName                  = "show-logs"
	LogFileMaxSizeName            = "log-file-max-size"
	LogFilesCombinedMaxSizeName   = "log-dir-max-size"
	LogFileVerbosityThresholdName = "log-file-verbosity"
)

// InitFlags creates logging flags which update the given variables. The passed mutex is
// locked while the boolean variables are accessed during flag updates.
func InitFlags(
	noRedirectStderr *bool,
	logDir flag.Value,
	showLogs *bool,
	nocolor *bool,
	vmodule flag.Value,
	logFileMaxSize, logFilesCombinedMaxSize *int64,
) {
	flag.BoolVar(nocolor, NoColorName, *nocolor, "disable standard error log colorization")
	flag.BoolVar(noRedirectStderr, NoRedirectStderrName, *noRedirectStderr, "disable redirect of stderr to the log file")
	flag.Var(vmodule, VModuleName, "comma-separated list of pattern=N settings for file-filtered logging (significantly hurts performance)")
	flag.Var(logDir, LogDirName, "if non-empty, write log files in this directory")
	flag.BoolVar(showLogs, ShowLogsName, *showLogs, "print logs instead of saving them in files")
	flag.Int64Var(logFileMaxSize, LogFileMaxSizeName, 2*1024*1024, "maximum size of each log file")
	flag.Int64Var(logFilesCombinedMaxSize, LogFilesCombinedMaxSizeName, 10*1024*1024, "maximum combined size of all log files")
}
