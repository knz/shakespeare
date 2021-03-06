# Shakespeare reference manual

- [Command line usage](#usage)
  - [Command line parameters](#command-line-parameters)
  - [Environment variables](#environment-variables)
  - [Exit status](#exit-status)
  - [Result files](#result-files)
  - [Uploading results](#uploading-results)
- Configuration:
  - [Common syntax elements](#common-syntax-elements)
  - [Overall structure of a configuration](#overall-structure-of-a-configuration)
  - [Configuration preprocessing](#configuration-preprocessing)
  - [Roles](#roles-configuration)
  - [Cast](#cast-configuration)
  - [Script](#script-configuration)
  - [Audience](#audience-configuration)
- [Command execution](#command-execution)
- [Predicate, ascalar and array expressions](#expressions) during auditions
- [Interpretation of results](#interpretation-of-results)

## Usage

Overall command line syntax:

```
shakespeare [parameters...] [configfiles...]
```

If no parameters are specified, defaults are used as described below.

If no configuration files are specified, `shakespeare` reads its
configuration from standard input. If multiple files are specified,
they are processed as if they were concatenated.  The additional
strings provided via `-s` on the command line are processed at the end
as if they appeared in a `script` section.

### Command line parameters

| Parameter                      | Default              | Description                                                                                                                                                       |
|--------------------------------|----------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `-D`, `--define`               | (none)               | [Preprocessing](#configuration-preprocessing) variable definitions.                                                                                               |
| `-I`, `--search-dir`           | .                    | Directory to search for included configurations.                                                                                                                  |
| `-n`, `--dry-run`              | false                | Stop after parsing the configuration and compiling the steps (do not actually execute the script).                                                                |
| `-o`, `--output-dir`           | `.`                  | Directory where to generate artifacts, output data files and the plot script.                                                                                     |
| `-k`, `--keep-artifacts`       | false                | If set, keep the `artifacts` sub-directory at the end of the play, before upload, if there was no foul. The artifacts are kept in any case if a foul is detected. |
| `-p`, `--print-cfg`            | false                | Print configuration after parsing and compilation.                                                                                                                |
| `-q`, `--quiet`                | false                | Run quietly.                                                                                                                                                      |
| `-r`, `--extra-interpretation` | (none)               | Additional lines of [`interpretation` configuration](#interpreation).                                                                                             |
| `-s`, `--extra-script`         | (none)               | Additional lines of [`script` configuration](#script-configuration).                                                                                              |
| `-S`, `--stop-at-first-foul`   | false                | Stop the play as soon as [a foul was detected](#interpretation-of-results).                                                                                       |
| `--upload-url`                 | (none)               | [Upload](#uploading-results) the [result files](#result-files) to the provided URL. Implies `--clear`.                                                            |
| `--clear`                      | false                | Remove the [result files](#result-files) at the end of the play (after upload) if there was no foul.                                                              |
| `--ascii-only`                 | false                | Avoid printing out special unicode characters.                                                                                                                    |
| `--version`                    | false                | Show version number and exit.                                                                                                                                     |
| `--log-dir`                    | `logs` in output dir | If non-empty, copy the logs to that directory.                                                                                                                    |
| `--logtostderr`                | NONE                 | Copy every log message at or above this threshold to stderr (choices: INFO, WARNING, ERROR, FATAL).                                                               |

### Environment variables

| Variable  | Default     | Description                                                                 |
|-----------|-------------|-----------------------------------------------------------------------------|
| `SHELL`   | `/bin/bash` | Shell to use to execute commands. A bourne-compatible shell is recommended. |
| `GNUPLOT` | `gnuplot`   | [Gnuplot](http://gnuplot.info) command to use to generate plots.            |

### Exit status

`shakespeare` returns a non-zero
exit status in the following circumstances:

- a cleanup command failed to execute, or terminated with non-zero status.
- a spotlight command failed to execute, or terminated with a non-zero status.
- an action command failed to execute,
- an action command executed but terminated with a non-zero status,
  and action failure was not tolerated (using the `?` syntax in the
  [script configuration](#script-configuration)),
- an auditor [expression](#expression) failed to evaluate with an error.
- [a foul was detected](#interpretation-of-results) during or at the end of the play.
- a directory or upload operation failed.

Otherwise, status 0 is returned.

### Result files

Upon starting the play, `shakespeare` creates a working directory named
after the current date and time, and updates the symlink `latest` to
point to it (both are placed inside the target directory specified via
`-o`).

During the play, the actor artifacts are stored in an `artifacts`
sub-directory. Data points are stored in a `csv` sub-directory. If
`--log-dir` was not specified, the log files are stored in a `logs`
sub-directory.

At the end of the play:

1. plots are produced from the CSV files in a `plots` sub-directory.
2. a summary of results is produced in `result.js`.
3. the report template page is written to `index.html`.
4. unless a foul was detected or `-k` is specified, `artifacts` is erased.
5. if `--upload-url` was specified, the contents of the working
   directory (including the results from steps 1-4 above) are uploaded
   to the specified URL. [See below for details](#uploading-results).
6. if `--clear` or `--upload-url` was specified and no foul was
   detected (and no upload error), the working directory is erased.

Example result tree after a test completion:

```
├── artifacts
│   ├── elmstreet
│   │   └── traffic.log
│   └── theporch
├── csv
│   ├── audit-bob.csv
│   ├── bob..mood.csv
│   ├── bob..moodt.csv
│   ├── bob.elmstreet.traffic.csv
│   ├── elmstreet.csv
│   └── theporch.csv
├── plots
│   ├─ plot.gp
│   ├─ plot.pdf
│   ├─ plot.svg
│   ├─ plot.txt
│   └─ runme.gp
├── index.html
└── result.js
```

### Uploading results

If `--upload-url` is specified, the contents of the [working directory](#result-files)
are uploaded to the provided URL.

The following URL schemes are supported:

- `scp://` (secure copy over SSH): passed to `scp -r <workdir>`
- `gs://` (Google Cloud Storage): passed to `gsutil -m cp -r <workdir>`
- `s3://` (Amazon S3): passed to `aws s3 cp --recursive <workdir>`

## Common syntax elements

The following syntax placeholders are reused throughout the rest of
the manual:

- `<name>` denotes an identifier.

- `<regexp>` denotes a [regular
  expression](https://en.wikipedia.org/wiki/Regular_expression) using
  [Google's re2 syntax](https://github.com/google/re2/wiki/Syntax).

  For an intro-level guide see this: https://www.regular-expressions.info/

- `<cmd>` denotes a unix shell command. This will be passed to `$SHELL
  -c` and thus can contain multiple statements, parameter expansions,
  redirections, etc.

- `<expr>` denotes a [boolean or scalar
  expression](#expressions) (currently based off the
  [`govaluate` expression
  language](https://github.com/Knetic/govaluate)).

Additionally, `shakespeare` supports:

- specifications spanning multiple lines: a backslash `\` at the end of
  the current line will cause `shakespeare` to continue reading input
  on the subsequent line. The result line preserves the backslash and
  newline characters. (This is automatically suitable for shell
  commands.)

- comments: a line starting by whitespace, followed by `#`, followed
  by arbitrary characters (possibly spanning multiple lines with
  `\`). These are ignored.

## Overall structure of a configuration

The minimum valid configuration is an empty file. However an empty
file also specifies an empty script, and thus instructs `shakespeare`
to terminate immediately.

For `shakespeare` to actually "do something", there must be a script
with at least some actions. The script must involves actors, so at
least one actor must be defined. Each actor plays a role, so at least
one role must be defined.

The definitions for roles, actors (the cast), script and audience can
be interleaved, although something can only be used after it has been
defined. For example, the following configuration is valid:

```
role doctor
end
cast
  alice plays doctor
end

role patient
end
cast
  david plays patient
end
```

Whereas this is invalid:

```
cast
  # this fails with "role doctor is not defined"
  alice plays doctor
end
role doctor
end
```

Here are the possible top level elements of a configuration:

- `parameter ...`: a definition for a preprocessing parameter.
- `role ... end`: a definition for one role.
- `cast ... end`: some definitions for the cast.
- `script ... end`: some definitions for the script.
- `audience ... end`: some definitions for the audience. Note that the
  definition lines for one audience member can span multiple
  `audience...end` sections. This is useful to e.g. add concerns for
  an auditor throughout separate included files.
- `include <file>`: include the named file at this point.
- `title ...`: a title string.
- `author ...`: an author string.
- `attention ...`: a "see also" annotation.

Tip: there may be multiple author, title and "see also" strings throughout
the configuration. They are combined in the results.

Tip: use `shakespeare -n -p` to print out the final configuration
after all directives have been processed.

## Configuration preprocessing

`shakespeare` provides a preprocessing facility as follows:

- preprocessing parameters can be defined with `-D` on the command
  line, or `parameter <name> defaults to <value>` inside the
  configuration.

- while reading/parsing the configuration, any occurrence of a name of
  a preprocessing parameter enclosed in `~...~` is replaced by its
  configured value. For example, with `-Dfoo=123`, occurrences of
  `~foo~` are replaced by `123`.

- this substitution occurs specifically on the following fields:

  - the text provided to `title` and `attention` clauses.
  - role names in role definitions and "extends" clauses, e.g. `role ~name~` or `role .. extends ~name~`.
  - role names in `every ` clauses used to define audiences and scenes, e.g. `alice watches every ~role~`.
  - the multiplicity of multi-actor definitions, e.g. `alice* play ~N~ doctor`.
  - the extra shell environment provided to an actor, e.g. `alice plays doctor with ~stuff~`.
  - evaluable expressions, e.g. `alice expects always: v < ~N~`.
  - include directives, e.g. `include ~filename~.cfg`.
  - the repeat count or duration in scripts e.g. `repeat ~n~ times` or `repeat time ~timeout~`.


## Roles configuration

A role definition defines a behavior template (blue print) for zero or
more characters in a play.

### Syntax

A role definition is defined as follows:

```
role <name> [extends <name>]
  # zero or more action definition:
  [ :<name>  <cmd> ] ...

  # zero or one cleanup definition:
  [ cleanup <cmd> ]

  # zero or one spotlight definition:
  [ spotlight <cmd> ]

  # zero or more signal definitions:
  [ signal <name> {event|scalar|delta} at <regexp> ] ...
end
```

The `role` keyword followed by a role name, an optional `extends`
directive, followed by the various role specification directives.  The
section ends with the keyword `end` on its own line.

### Semantics

Constraints:

- Each defined role must have a unique name.
- Each action defined for a role must have a unique name within the role.
- Action, cleanup, spotlight and signal definitions are optional,
  although signals are only meaningful if there is also a spotlight defined.

Behavior:

- If the `extends` clause is specified, the role it designates
  is copied as template for the new role.

- The command associated with an action is executed when that action
  is prompted in a [script](#script-configuration).
  See [Command execution](#command-execution) for details.

- The cleanup command is executed once at the beginning before the play,
  and once when the play terminates.
  See [Command execution](#command-execution) for details.

- The spotlight command is executed asynchronously at the start
  of the play, and its output (on standard error + standard output combined)
  is used as input to the signals.
  See [Command execution](#command-execution) for details.

- Tip/Note: when using `tail -F` to observe a log file produced for
  another action, ensure that the file exists prior to running `tail`
  (e.g. using `touch`). Otherwise, `tail` may wait multiple seconds
  before detecting the first write to the fail.

- The signal regular expressions are applied on each line of the
  spotlight's output. Each matching line becomes an event or a data
  point with the provided signal name.

- The data collected by signals are written to CSV files in the output
  directory (specified via `--output-dir` on the command line, by
  default the current directory).

- The following signal types are defined:

  - `event` signals: these collect a *string* associated with moments
    in time. Events are plotted at points at a constant y-value
    in the behavior plots, without connecting line.
    
  - `scalar` signals: these collect a *numeric value* associated with
    moments in time. Scalars are plotted at their value on the y axis
    with a curve.

  - `delta` signals: these sample a numeric value in the spotlight
    data, but their scalar value the difference between the current
    input value and the last.

- On each line of data sampled by the spotlight, signals must
  determine a *timestamp*. The timestamp can be collected as follows:

  - if the regular expression contains the pseudo-pattern
    `(?P<ts_now>)`, then the time at which the line of data was
    received by `shakespeare` is used as timestamp.

  - the pseudo-pattern `(?P<ts_rfc3889>)` expands to a regular
    expression that matches a date/time printed in the RFC 3339 format,
    which is then used to determine the timestamp from the spotlight input.
    
  - the pseudo-pattern `(?P<ts_log>)` expands to a regular expression
    that matches a date/time printed using the CockroachDB log format,
    which is then used to determine the timestamp from the sportlight input.

  - the pseudo-pattern `(?P<ts_deltasecs>)` expands to a regular
    expression that matches a decimal number (either `123`, `123.456`
    or `.456`) and interprets it as a duration in seconds since the
    start of the test.

  In general, it is preferrable to have the underlying system
  process log and report (via a spotlight) its own time measurements.
  This is useful especially in cases where the process runs remotely,
  and there is a delay between the moment the event occurs and the
  moment it is collected by `shakespeare`.

- `shakespeare` terminates
  with an error if the cleanup command fails with a non-zero status.

- Currently, errors encountered while executing the spotlight command
  are ignored.

## Cast configuration

The cast defines actors/characters that can execute actions during the play.

### Syntax

The cast is defined as follows:

```
cast
  # zero or more actor/character definitions.
  [ <name> plays <name> [with <vars>] ] ...

  # zero or more definitions for multiple actors.
  [ <name>* play <N> <name> [with <vars>] ] ...
end
```

The `cast` keyword followed by zero or more character definitions,
ending with the `end` keyword on its own line.

Each character definition is a (newly defined) character/actor name,
followed by the keyword `plays`, followed by a role name, and
optionally followed by environment variable definitions enclosed by
parentheses.

It is possible to define multiple actors simultaneously by following
the name with a `*`, and using `play <N>` instead of `plays`.  The
role name may be pluralized for readability. This expands into N
definitions of a single actor; the actor name is generated by
concatenating the name given with a value between 1 and N inclusive.
Each actor defined in this way will also see its environment variable
expanded with a variable called `i`, as if it was configured using
`with i=...`, with values ranging from 0 to N-1.

### Semantics

Constraints:

- each actor/character name must be unique.
- the variables passed between parentheses, if present, must be valid
  input to the shell command `env -S`.

Behavior:

- Each actor can execute the actions defined by its role.
- Each actor defines a separate working directory for its role's
  actions. The directory is named after the character's name in the
  configuration.
- The variable definitions can be used to customize the role for
  different characters. They are used to define environment variables
  for the role's commands via the standard shell command `env -S`.
- In multi-actor definitions, the shell variable `i` is defined
  in every action to the number of the actor.

## Script configuration

The script defines what is executed during a play.

### Syntax

The script is defined as follows:

```
script
  # an optional tempo specification, the default is 1 second.
  [tempo <duration>]

  # zero or more scene action definition
  [ scene <char> entails for <target>: <actions>[?] [ ; <action>[?] ] ... ] ...

  # zero or more mood change
  [ scene <char> mood ends <name> ] ...
  [ scene <char> mood starts <name> ] ...

  # zero or more story line definition
  [ storyline <line> ] ...

  # zero or more story edits
  [ edit s/<regexp>/<subst>/ ] ...

  # zero or more repeat specifiers
  [ repeat from <regexp> ] ...

  # zero or more repeat max count
  [ repeat {always | <number> times} ] ....

  # zero or more repeat max durations
  [ repeat time {unconstrained | <duration>} ] ...
end
```

The `tempo` and `repeat time` durations use the Go duration syntax,
for example a number followed by a time unit. For example `1s`,
`100m50us`, etc.

Actions in a scene are defined by the `scene` keyword, followed by a
scene handle (ASCII letter or number), followed by the `entails for`
keyword, followed by an actor selector, followed by `:`, followed by a
list of actions separated by semicolons. Each action may optionally
end with `?` (see below).

A mood change can be configured by a scene using the `scene` keyword,
followed by a scene handle, followed by `mood`, followed by a mood
name.

A storyline is defined by the `storyline` keyword, followed by
zero or more act strings separated by spaces. Any occurrence of `_` in these strings
is ignored during parsing, and can thus be used to pad strings visually.

An actor selector is either:

- `<name>`: a reference to a single actor/character.
- `every <name>`: a reference to all actors playing the given role.

Act strings are composed as follows:

- the special notation `.` for "no operation";
- a control character that refers to a scene definition;
- a concurrent group, composed of two or more scene references separated by `+`.

### Semantics

Constraints:
- each scene handle must be unique.
- the scene handles used in story lines must be defined by `scene` lines prior.

Semantics:

- the storyline is decomposed in *story acts* at space boundaries.

- each act is then decomposed in *scene groups* consisting of either
  "do nothing (`.`), a single scene refered by its handle, or
  two or more scenes refered by handles separated by `+`.

- if there are multiple `storyline` definitions, they are *combined*
  during parsing, merging the scene groups on all definitions, and
  aligning the acts. For example `..a bc ` merged with `a b c` produces
  `a.a b+bc c`.

- each `edit` line is processed during parsing by performing a regular
  sed-like regexp substitution on the storyline computed so far.

- `shakespeare` performs each act sequentially. Within one act,
  scene groups are performed sequentially. Within one scene group,
  scenes are performed concurrently. There is an execution barrier
  at the end of each scene group.

- Each scene group is not allowed to start before its minimum start
  time, defined as the position of the column inside the act times the
  tempo. For example, with the default tempo of 1s, the 4th column
  scene is not allowed to start before 4s has elapsed since the start
  of the act.

  It is possible for actions to last longer than the tempo interval. When this occurs,
  further scenes may be delayed further than their minimum start time, in which
  case they will start immediately when their previous scene completes.

  Therefore, the total number of scene groups, multiplied by the tempo,
  determines the *minimum* play duration (assuming no errors occur), but
  a play may last longer.

- If a `repeat from` clause is specified, `shakespeare` will find the
  first act that matches the provided regexp. During execution of the
  play, every time the last act is reached, the play will continue,
  restarting at the matching act.

  Additionally, in the generated plots, a "zoomed in" plot will be
  generated for the last two iterations of the repeated act(s).

  Subsequent `repeat` clauses override each other.

- The `repeat always` / `repeat N times` clause specifies the maximum
  number of repetitions of the play after the act specified by `repeat
  from`. `repeat always` (no maximum repetition count) is the default.

- The `repeat time` clause specifies the maximum duration of the
  repeated part of the play. `repeat time unconstrained` (no maximum
  duration) is the default.

- **Note:** when using `repeat always` or `repeat time unconstrained`,
  it is strongly recommended to run with [command line
  parameter](#command-line-parameters) `-S` to ensure that the program
  stops upon encountering a foul.

## Audience configuration

The audience defines collection points for signals, and
enables `shakespeare` to generate behavior plots.

### Syntax

The audience is defined as follows:

```
audience
   # optional activation period: when the observer also audits
   [ <name> audits {only while <expr> | throughout} ]

   # zero or more derived scalar values
   [ <name> computes <var> as <expr> ] ...

   # zero or more aggregations
   [ <name> collects <var> as <aggregation> <expr> ] ...

   # optional audit predicate
   [ <name> expects <modality>: <expr> ]

   # expect the same as another auditor
   [ <name> expects like <name> ]

   # zero or more watch specifications
   [ <name> watches {<var> | <selector> <name>} ] ...

   # optional y-axis labels
   [ <name> measures <label> ] ...

   # disable plot generation
   [ <name> only helps ]
end
```

Each specification inside the `audience` section refers to an *observer* name at the beginning of the line, followed by either:

- `audits` and an audit period consisting of either "`throughout`", or
  `only when` followed by a predicate expression.

- `computes` followed by a variable name, `as` and an expression.

- `collects` followed by a variable name, `as`, an aggregator (see below), and an expression.

- `expects` followed by a modality (see below), a colon, and an expression.

- `watches` followed by an actor selector, followed by the name of a
  signal for the actor's role.

- `measures` and an arbitrary string (until the end of the line),
  which defines a label for the y-axis of the generated plot.

- just the keywords `only helps`.

As in [script configurations](#script-configuration) above, an actor
selector is either:

- `<name>`: a reference to a single actor/character.
- `every <name>`: a reference to all actors playing the given role.

Aggregators are either:

- `first <N>`
- `last <N>`
- `top <N>`
- `bottom <N>`

Modalities are either: `always`, `never`, `once`, `twice`, `thrice`,
`not always`, `eventually`, `eventually always`, `always eventually`.

### Semantics

Constraints:

- the signal name on each watch specification must be valid for that
  actor's roles (or the role of all selected actors with `every`).

- the signals and variables used in expressions for `expects`,
  `collects`, `computes` and `audits` must be defined before use.

Behavior for auditors:

- An observer is also an auditor if it has a `computes`, `collects` or `expects` clause.

- *Activation periods*:

  - Auditors only work (either compute, collect or expect) during their
    activation periods. The activation period begins when the `audits` expression
    becomes true, and ends when it becomes false.

  - During one activation period, the auditor expects the `expects`
    predicate to evaluate to true and non-true according to the
    modality. For example, `expects always` expects the predicate to
    always be true. `expects once` expects the preedicate to become true
    exactly once. etc.

  - If there are multiple activation periods, the `expects` modality resets
    at the beginning of each period.

- *Variables*:

  - Auditors can also compute or collect data into *variables*. Variables
    are preserved across activation periods.

  - Variables are visible to/reusable by other auditors and observers defined
    afterwards.

  - Any audience member can also observe (via `watches`) a computed
    variable, which brings it to the observer's plot.

  - The non-nil result of a `computes` expression is stored in the variable as-is.
    In comparison, the result of a `collects` expression is aggregated
    into an array, and the array stored in the variable.
    nil results are ignored.

  - Array variable are suitable as input for array functions, see
    [Expressions](#expressions) below.

- *Dataflow sensitivity*: the expression for `audits`, `computes`,
  `collects` or `expects` is only evaluated when all its
  input dependencies hold:

    - all the signals that they depend on have generated a value, and
    - all of the variables they depend on were just assigned.

  (When a `computes` or `collects` clause stores a value in a variable
  that a later expression depends on, such that all its dependencies
  bewcome satisfied, it can be evaluated in the same activation. This
  behavior intends to mimic the traditional behavior of dependent
  computations in a spreadsheet.)

Behavior for data observation:

- When `shakespeare` completes a play, it generates a
  [Gnuplot](http://www.gnuplot.info/) script that generates output
  plots.

- Each observer that does not specify `only helps` defines a separate
  plot box in the output.

- Each plot's x-axis is time.

- Each signal selected by `watches` is plotted as a separate line in the observer's plot:

  - `event` signals are plotted as dots around a fixed y-value that identifies its source. Some
    randomness is added to the y value so that multiple events occurring close to the same
    instant in time can be distinguished in the plot.

  - `scalar` (and `delta`) signals are plotted as lines with their
    value on the y axis. The y range is automatically scaled to
    enclose all the curves.

- Each plot's y-axis is labeled with the `measures` specification, if present.

- Audit events (from `expects` clauses) are plotted near the top.

- The moods are used to define color bands in the background of all observer plots.

## Command execution

`shakespeare` runs external commands for:

- initializing / cleaning up actors via their
  [role](#role-configuration)'s `cleanup` definition;
- starting actor spotlights via their role's `spotlight` definition;
- effecting actions as determined by the [script](#script-configuration).

The text of the command given in the configuration is not executed
as-is; instead, before the play starts `shakespeare` prepares a
"command file"—a *shell script*—in the actor's `actions` sub-directory.

Exporting the command into a file achieves the following:

- the shell script can be inspected to verify the result of
  [preprocessing](#configuration-preprocessing) and how the per-actor
  custom environment (the `with` clause) was expanded.

- for non-spotlight commands, the shell script also automatically
  redirects the command's output to a log file. This facilitates
  inspection of command outputs after a play completes.

- prior to executing the command, the generated shell script also
  forces the current directory (to the actor's working directory) and
  resets the TMPDIR/HOME environment variables. This makes it
  possible to safely **execute one actor's command from the action of
  another actor**.

## Expressions

The expression syntax is detailed in the [govaluate manual](https://github.com/Knetic/govaluate/blob/master/MANUAL.md).

### Syntax summary

- just 3 atomic types: booleans, strings and floats.
- can use identifiers either directly when simple, or enclosed between `[...]` when containing special characters.
- dates/timestamps in strings auto-casted to float (unix time).
- operators:
  - scalar arithmetic `+` `-` `/` `%` `*`, also `**` (exponentiation)
  - comparisons `==` `!=` `<` `>` `<=` `>=`
  - boolean negation `!`
  - string concatenation `+`
  - regexp match `=~` `!=`
  - short circuit evaluation `&&` `||`
  - ternary conditional `? :`
  - nil choice `??`
  - bitwise arithmetic on the rounded value of the float: `&` `|` `^` `~` `>>` `<<`
- non-homogeneous arrays:
  - literal `(a, b, c, ...)`.
  - membership `x IN array`
- function application `f(x, y, z...)`

### Signal references

An expression can refer to an actor signal with the following syntax:
`[<actor> <signal>]`.

This dependency is satisfied (and thus triggers dependent evaluations)
every time the signal is received from that actor.

### Predefined variables

The predefined variables are updated (and thus triggers dependent
evaluations) on every mood change and signal received.

- `t`: current time relative to start of scenario.

- `mood`: current mood. Note that it updates even when the mood has
  not changed.

- `moodt`: current time relative to the start of the current mood.

### Predefined functions

- `ndiff(x, y)`: percentage difference of `x` relative to `y`. For example,
  `ndiff(120, 100) == .20`

- `abs(x)`: absolute value of `x`

- `floor(x)`: floor of `x`

- `ceil(x)`: ceiling of `x`

- `round(x)`: nearest integer to `x`, rounding away from zero

- `log(x)`: natural logarithm of `x`. Produces NaN for zero or negative arguments.

- `sqrt(x)`: square root of `x`. Produces NaN for negative arguments.

- `count(ar)`: count of values in array

- `first(ar)`: first value in array, nil if empty

- `last(ar)`: last value in array, nil if empty

- `sorted(ar)`: sorted copy of array

- `sum(ar)`: sum of values in array, nil if empty

- `avg(ar)`: average of values in array, nil if empty

- `med(ar)`: median of values in array, nil if empty

- `min(ar)`, `max(ar)`: minimum and maximum values in array, nil if empty.

## Interpretation

The *interpretation* of a play indicates how to translate auditor
expectations into a *foul*, which causes a non-zero [exit
status](#exit-status).

By default, `shakespeare` reports a foul if any auditor found itself
disappointed, as per its [`expect` clause](#audience-configuration).

This corresponds to the common case where the play specification
describes expected "good" behavior, the system is expected to behave
well to start with, and `shakespeare` is used to verify that the
system does, actually, performs well.

It is possible to change this to indicate “expected failures,” without
having to change the audience definition. This makes it possible
to share a constant/shared play configuration for the purpose of
investigating a foul.

To achieve this, an optional `interpretation` section can be included,
either in the input configuration files or [via `-r`](#command-line-parameters).

### Syntax

The interpretation is defined as follows:

```
interpretation
   [ ignore <name> {disappointment | satisfaction} ] ...

   [ ignore {disappointment | satisfaction} ] ...

   [ require <name> {disappointment | satisfaction} ] ...

   [ foul upon <name> {disappointment | satisfaction} ] ...
end
```

### Semantics

- The `ignore` clause indicates that the corresponding `expect` result
  does not have any influence on whether the play is considered to be
  fouled or not.

- The `require` clause indicates that the play is considered to be fouled
  if the corresponding result was never encountered.

- The `foul upon` clause indicates that the play is considered to be
  fouled if the corresponding result was ever encountered.

- Later clauses override the configuration of earlier clauses for a given
  (auditor, result) pair.

- For each auditor, the initial configuration is as follows:

  ```
  foul upon ... disappointment
  ignore ... satisfaction
  ```

- The shorthand `ignore disappointment/satisfaction` without an auditor
  name configures all the auditors at once.
