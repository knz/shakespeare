# shakespeare

**shakespeare** is a scenario-based testing framework for distributed
processes.

It orchestrates the execution and monitoring of multiple concurrent
test processes and synthetizes test results including plots of
activity over time.

- [How it works](#How-it-works)
- [Command line parameters](#Command-line-parameters)
- [Example: traffic and red lights](#Example-traffic-and-red-lights)
- [Example: CockroachDB sustaining client traffic while a node is  down](#Example-CockroachDB-sustaining-client-traffic-while-a-node-is-down)
- [Reference manual](#Reference-manual)

## How it works

1. `shakespeare` reads a configuration from its standard input that defines:
   - one or more *actors*, each playing a *role* (multiple actors can play the same role);
   - a *script* that defines what the actors should be doing over time;
   - optionally, to obtain diagrams of the behavior:
     - *spotlights* that shine upon the actors during a play, to extract observable data;
     - an *audience* of observers during the play;
   - optionally, to verify the behavior:
     - some observers can also be *auditors* that check whether the actors perform well.

2. `shakespeare` reads the configuration from its standard input and
   compiles the script to a *sequence of scenes* for the actors to
   follow.

   At each scene, the following can happen:
   - an actor can be called to perform some *actions* (a command is executed);
   - the *mood* of the stage can be altered (a period of interest is marked in the output diagrams);

3. `shakespeare` brings the conductor on stage. The play begins. The
   conductor does the following:

   - calls the spotlights to shine upon the actors (starts the monitoring processes).
   - invites the prompter to the front of the stage. The prompter
     instructs the actors to carry out the steps defined by the script
     (runs action commands).
   - calls the actors to leave the scene (runs the role cleanup commands).

4. meanwhile, each observer in the audience scrutinizes one or more
   actor as revealed by their spotlight: plottable data is gathered by
   filtering the monitoring data.

   observers that are also auditors scrutinize the actors and check
   that their performance is acceptable: behavior signals are checked
   for specified acceptance criteria defined by predicates.

5. during the play, the prompter reports the scenes and actions on the left
   side of the terminal output. The observers report collected events
   on the middle of the terminal output. Audit results, if any, are
   shown on the right of the terminal output.

6. at the end of the play, the audience reports on their
   experience (plots and test results are produced).
   The result plots all share the timeline of the play
   on the x-axis.

   - the performed actions are displayed in the top plot.
   - each observer then gets its own plot.
   - on each observer plot, each of the observed signal sources (from
     spotlights) produces a data series, and audit results are
     overlaid on top.

This overall operation is illustrated by the following diagram:

![Overall operation](docs/Shakespeare.png)

## Command line parameters

| Parameter                         | Default              | Description                                                                                         |
|-----------------------------------|----------------------|-----------------------------------------------------------------------------------------------------|
| `-o`, `--output-dir`              | `.`                  | Directory where to generate artifacts, output data files and the plot script.                       |
| `-S`, `--stop-at-first-violation` | false                | Stop the play as soon as an auditor detects a violation.                                            |
| `-p`, `--print-cfg`               | false                | Print configuration after parsing and compilation.                                                  |
| `-n`, `--dry-run`                 | false                | Stop after parsing the configuration and compiling the steps (do not actually execute the script).  |
| `-q`, `--quiet`                   | false                | Run quietly.                                                                                        |
| `-I`, `--search-dir`              | .                    | Directory to search for included configurations.                                                    |
| `--ascii-only`                    | false                | Avoid printing out special unicode characters.                                                      |
| `--log-dir`                       | `logs` in output dir | If non-empty, copy the logs to that directory.                                                      |
| `--logtostderr`                   | NONE                 | Copy every log message at or above this threshold to stderr (choices: INFO, WARNING, ERROR, FATAL). |

## Environment variables

| Variable  | Default     | Description                                                                 |
|-----------|-------------|-----------------------------------------------------------------------------|
| `SHELL`   | `/bin/bash` | Shell to use to execute commands. A bourne-compatible shell is recommended. |
| `GNUPLOT` | `gnuplot`   | [Gnuplot](http://gnuplot.info) command to use to generate plots.            |

## Example: traffic and red lights

A [separate document page](docs/redlight.md) introduces the main
features of `shakespeare` using a running example.

The example orchestrates two play characters: a road where traffic
occurs, and a traffic light meant to stop or let traffic flow.
Example behavior diagram:

![simple example plot with behavior and mood](examples/redlight3.svg)

The top plot shows the actions carried out by `shakespeare` on the system.
The bottom plot shows how the system reacted.

([link to configuration](examples/redlight3.cfg) — [link to doc](docs/redlight.md))

## Example: CockroachDB sustaining client traffic while a node is down

This example shows the behavior of a 3-node CockroachDB clusters with
two "KV workload" clients. The node to which one of the two clients is
connected is brought down two times gracefully. The behavior plot
shows that the other client's throughput remains stable during the
downtime.

![example crdb plot](examples/kv3.svg)

The top plot shows the actions carried out by `shakespeare` on the system.
The bottom plots show how the system reacted.

([link to configuration](examples/kv3.cfg))

## Reference manual

A [reference manual](docs/manual.md) (currently incomplete) is
included in the `docs/` sub-directory.
