# Shakespeare reference manual

- Configuration:
  - [Common syntax elements](#Common-syntax-elements)
  - [Roles](#Roles-configuration)
  - [Cast](#Cast-configuration)
  - [Script](#Script-configuration)
  - [Audience](#Audience-configuration)

## Common syntax elements

The following syntax placeholders are reused throughout the rest of
the manual:

- `<name>` denotes an identifier.

- `<regexp>` denotes a [regular
  expression](https://en.wikipedia.org/wiki/Regular_expression) using
  [Google's re2 syntax](https://github.com/google/re2/wiki/Syntax).

- `<cmd>` denotes a unix shell command. This will be passed to `$SHELL
  -c` and thus can contain multiple statements, parameter expansions,
  redirections, etc.

- `<expr>` denotes a [boolean or scalar
  expression](#Expression-evaluation) (currently based off the
  [`govaluate` expression
  language](https://github.com/Knetic/govaluate)).

## Roles configuration

One or more roles definitions is expected. 
A role definition defines a behavior template (blue print) for zero or
more characters in a play.

### Syntax

A role definition is defined as follows:

```
role <name> is
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

The `role` keyword followed by a role name, the `is` keyword and
the various role specification directives.
The section ends with the keyword `end` on its own line.

### Semantics

Constraints:

- Each defined role must have a unique name.
- Each action defined for a role must have a unique name within the role.
- Action, cleanup, spotlight and signal definitions are optional,
  although signals are only meaningful if there is also a spotlight defined.

Behavior:

- The command associated with an action is executed when that action
  is prompted in a [script](#Script-configuration).

- The cleanup command is executed once at the beginning before the play,
  and once when the play terminates.

- The spotlight command is executed asynchronously at the start
  of the play, and its output (on standard error + standard output combined)
  is used as input to the signals.

- The working directory for the action, cleanup and spotlight commands
  is that of the character/actor that plays the role. Different
  actors playing the same role use separate working directories.

- The signal regular expressions are applied on each line of the
  spotlight's output. Each matching line becomes an event or a data
  point with the provided signal name.

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
    received is used as timestamp.

  - the pseudo-pattern `(?P<ts_rfc3889>)` expands to a regular
    expression that matches a date/time printed in the RFC 3339 format,
    which is then used to determine the timestamp from the input.
    
  - the pseudo-pattern `(?P<ts_log>)` expands to a regular expression
    that matches a date/time printed using the CockroachDB log format,
    which is then used to determine the timestamp from the input.

  In general, it is preferrable to have the underlying system
  process log and report (via a spotlight) its own time measurements.
  This is useful especially in cases where the process runs remotely,
  and there is a delay between the moment the event occurs and the
  moment it is collected by `shakespeare`.


- `shakespeare` terminates
  with an error if the cleanup command fails with a non-zero status.

- Currently, errors encountered while executing the spotlight command
  are ignored.

- Currently, errors encountered while executing the action commands
  are reported in the final plot(s).

## Cast configuration

The cast defines actors/characters that can execute actions during the play.

### Syntax

The cast is defined as follows:

```
cast
  # zero or more actor/character definitions.
  [ <name> plays <name> [( <vars> )] ] ...
end
```

The `cast` keyword followed by zero or more character definitions,
ending with the `end` keyword on its own line.

Each character definition is a (newly defined) character/actor name,
followed by the keyword `plays`, followed by a role name, and
optionally followed by environment variable definitions enclosed by
parentheses.

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

## Script configuration

The script defines what is executed during a play.

### Syntax

The script is defined as follows:

```
script
  # an optional tempo specification, the default is 1 second.
  [tempo <duration>]

  # zero or more action aliases.
  [ action <char> entails <sceneline> ] ...

  # zero or more script lines.
  [ prompt <selector> <line> ] ...
end
```

The tempo duration uses the Go duration syntax: a number followed by a
time unit. For example `1s`, `100ms`, etc.

An action alias is defined by the `action` keyword, followed by an
ASCII printable character, followed by the `entails` keyword, followed
by a "scene line": one or more of the following items, separated by semicolons:

- `:<name>`: a reference to a role action.
- `mood <name>`: sets the mood of the play to the given name.
- `nop`: "no-operation": do nothing.

A script line is defined by the `prompt` keyword, followed by an actor
selector, followed by a string of control chars.

An actor selector is either:

- `<name>`: a reference to a single actor/character.
- `every <name>`: a reference to all actors playing the given role.

### Semantics

Constraints:
- each action character must be unique.
- the control characters used in script lines must be defined by `action` lines prior.
- the actions entailed by the control chars must be valid for the selected actors' role.

Semantics:

- Each column of control chars across all script lines defines a *scene*.

- `shakespeare` executes the script lines of each actor inside a
  single scene concurrently with each other.

- `shakespeare` waits until each actor has finished running its line
  in a scene before starting the next scene.

- Each scene is not allowed to start before its minimum start time,
  defined as the position of the column times the tempo. For example,
  with the default tempo of 1s, the 4th column scene is not allowed
  to start before 4s has elapsed since the start of the test.

It is possible for actions to last longer than the tempo interval. When this occurs,
further scenes may be delayed further than their minimum start time, in which
case they will start immediately when their previous scene completes.

Therefore, the length of the longest script line, multiplied by the
tempo, determines the *minimum* play duration (assuming no errors
occur), but a play may last longer.

## Audience configuration

The audience defines collection points for signals, and
enables `shakespeare` to generate behavior plots.

### Syntax

The audience is defined as follows:

```
audience
   # zero or more watch specifications
   [ <name> watches <selector> <name> ] ...

   # optional y-axis labels
   [ <name> measures <label> ] ...
end
```

Each specification inside the `audience` section refers to an *observer* name at the beginning of the line, followed by either:

- `measures` and an arbitrary string (until the end of the line),
  which defines a label for the y-axis of the generated plot.

- `watches` followed by an actor selector, followed by the name of a
  signal for the actor's role.

As in [script configurations](#Script-configuration) above, an actor
selector is either:

- `<name>`: a reference to a single actor/character.
- `every <name>`: a reference to all actors playing the given role.


### Semantics

Constraints:

- the signal name on each watch specification must be valid for that
  actor's roles (or the role of all selected actors with `every`).

Behavior:

- Each unique observer name defines a separate plot in the output.

- Each plot's x-axis is time.

- Each signal selected by `watches` is plotted as a separate line in the observer's plot:

  - `event` signals are plotted as dots around a fixed y-value that identifies its source. Some
    randomness is added to the y value so that multiple events occurring close to the same
    instant in time can be distinguished in the plot.

  - `scalar` (and `delta`) signals are plotted as lines with their
    value on the y axis. The y range is automatically scaled to
    enclose all the curves.

- Each plot's y-axis is labeled with the `measures` specification, if present.

- The moods are used to define color bands in the background of all observer plots.

## Expression evaluation

The expression syntax is detailed in the [govaluate manual](https://github.com/Knetic/govaluate/blob/master/MANUAL.md).

Summary:

- just 3 atomic types: booleans, strings and floats.
- dates/timestamps in strings auto-casted to float (unix time).
- operators:
  - scalar arithmetic `+` `-` `/` `%` `*`, also `**` (exponentiation)
  - comparisons `==` `!=` `<` `>` `<=` `>=`
  - boolean negation `!`
  - string concatenation `+`
  - regexp match `=~` `!=`
  - short circuit evaluation `&&` `||`
  - ternary conditional `? :` and `??`
  - bitwise arithmetic on the rounded value of the float: `&` `|` `^` `~` `>>` `<<`
- non-homogeneous arrays:
  - literal `(a, b, c, ...)`
  - membership `x IN array`
- function application `f(x, y, z...)`
