# shakespeare

**shakespeare** is a testing framework for distributed processes.

It orchestrates the execution and monitoring of multiple concurrent
test processes and synthetizes test results including plots of
activity over time.

## How it works

1. the user provides a configuration that defines:
   - one or more *actors*, each playing a *role* (multiple actors can play the same role);
   - a *script* that defines what the actors should be doing over time;
   - *spotlights* that shine upon the actors during a play, to extract observable data;
   - an *audience* of observers during the play.

2. shakespeare reads the configuration and compiles the script to a
   *sequence of steps* for the actors to follow.

   At each step, the following can happen:
   - an actor can be called to perform an *action* (a command is executed);
   - the *mood* of the stage can be altered (a period of interest is marked in the output diagrams);
   - time is left to elapse.

3. shakespeare brings the conductor on stage. The play begins. The
   conductor does the following:

   - calls the actors to enter the scene (runs the role prelude commands)
   - calls the spotlights to shine upon the actors (starts the monitoring processes)
   - instructs the actors to carry out the steps defined by the script (runs action commands)
   - calls the actors to leave the scene (runs the role cleanup commands)

5. meanwhile, each observer in the audience scrutinizes one or more
   actor as revealed by their spotlight: plottable data is gathered by
   filtering the monitoring data.

6. at the end of the play, the audience reports on their experience
   (plots and test results are produced).

## Command line parameters

| Parameter      | Default | Description                                                                                          |
|----------------|---------|------------------------------------------------------------------------------------------------------|
| `-outdir`      | `.`     | Directory where to generated output data files and plot script.                                      |
| `-log-dir`     | (empty) | If non-empty, copy the logs to that directory.                                                       |
| `-logtostderr` | INFO    | Copy every log message at or above this threshold to stderr (use NONE to disable reports to stderr). |
