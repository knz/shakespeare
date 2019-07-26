package cmd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/stop"
	"github.com/knz/shakespeare/pkg/crdb/timeutil"
)

// runActorCommand runs a command on behalf of an actor.
//
// If timeout is non-zero, the command is cancelled after that amount
// of time has passed.
//
// If interruptible is true, the command is also cancelled if the
// stopper is shutting down the program. This should be true in all
// cases, but can be set to false for e.g. cleanup actions run during
// shutdown.
//
// The method returns the command output and process state.
// Two separate errors may be returned:
// - err to indicate something went wrong while attempting to run the command.
//   This is nil if the command could be executed successfully.
// - exitErr with the result on waiting on the process after it completed
//   execution. an *exec.ExitError, if any, will be found there.
func (a *actor) runActorCommand(
	ctx context.Context,
	stopper *stop.Stopper,
	timeout time.Duration,
	interruptible bool,
	pScript string,
) (outdata string, ps *os.ProcessState, err error, exitErr error) {
	var outbuf bytes.Buffer
	ps, err, exitErr = a.runActorCommandWithConsumer(ctx, stopper, timeout, interruptible, pScript, nil, func(line string) error {
		outbuf.WriteString(line)
		return nil
	})
	outdata = outbuf.String()
	if len(outdata) == 0 {
		log.Info(ctx, "(command produced no output)")
	} else {
		log.Infof(ctx, "command output:\n%s", outdata)
	}
	return outdata, ps, err, exitErr
}

func (a *actor) runActorCommandWithConsumer(
	ctx context.Context,
	stopper *stop.Stopper,
	timeout time.Duration,
	interruptible bool,
	pScript string,
	termCh <-chan struct{},
	consumer func(string) error,
) (ps *os.ProcessState, err error, exitErr error) {
	execCtx, killCmd := context.WithCancel(context.Background())
	defer killCmd()
	cmd := a.makeShCmd(execCtx, true /*bindCtx*/, pScript)

	log.Infof(ctx, "running: %s (workdir %s)", strings.Join(cmd.Args, " "), cmd.Dir)

	outstream, err := cmd.StderrPipe()
	if err != nil {
		return nil, errors.WithContextTags(errors.Wrap(err, "setting up"), ctx), nil
	}
	cmd.Stdout = cmd.Stderr

	// We'll use a buffered reader to extract lines of data from it.
	rd := bufio.NewReader(outstream)
	lines := make(chan res)
	readerDone := make(chan struct{})
	runReaderAsync(ctx, stopper, rd, lines, readerDone)

	if err := cmd.Start(); err != nil {
		return nil, errors.WithContextTags(errors.Wrap(err, "exec"), ctx), nil
	}

	if timeout != 0 {
		// If the command does not complete within the specified timeout, we'll terminate it.
		var cancel func()
		ctx, cancel = context.WithDeadline(ctx, timeutil.Now().Add(timeout))
		defer cancel()
	}

	var stopRequested <-chan struct{}
	if interruptible {
		// If not interruptible, we want to ignore the stopper.
		// Just override the channel.
		stopRequested = stopper.ShouldQuiesce()
	}

	coll := errorCollection{}
	stopRead := false
	interrupt := false
	for !stopRead {
		// Consume lines of data until either:
		// - there is no more data (presumably because the process has terminated).
		// - the consumer is unhappy.
		// - the stopper or context tells us to stop.
		select {
		case res := <-lines:
			// log.Infof(ctx, "received from reader: %+v", res)
			if res.err != nil {
				coll.errs = append(coll.errs, err)
				interrupt = true
			}
			if res.err != nil || res.line == "" {
				stopRead = true
				break
			}
			if res.line == "" {
				stopRead = true
				break
			}
			if err := consumer(res.line); err != nil {
				coll.errs = append(coll.errs, err)
				stopRead = true
				interrupt = true
				break
			}
		case <-stopRequested:
			log.Info(ctx, "interrupted")
			interrupt = true
			stopRead = true
			break
		case <-ctx.Done():
			log.Info(ctx, "canceled")
			coll.errs = append(coll.errs, wrapCtxErr(ctx))
			interrupt = true
			stopRead = true
			break
		case <-termCh:
			// Prompter is completing, and telling the spotlights to stop.
			log.Info(ctx, "terminated by prompter")
			stopRead = true
			interrupt = true
			break
		}
	}

	// At this point, we are on our way out.
	// We need to drain asynchronously, because
	// Wait() below is blocking.
	go func() {
		// If we're interrupting the process, do it.
		if interrupt {
			log.Info(ctx, "interrupting command")
			pgid, err := syscall.Getpgid(cmd.Process.Pid)
			if err != nil {
				log.Warningf(ctx, "unable to obtain process group: %v", err)
			} else {
				// First, try to ask the process to terminate gracefully.
				syscall.Kill(-pgid, syscall.SIGHUP)
			}
		}

		timer := time.After(2 * time.Second)

		if log.V(1) {
			log.Info(ctx, "draining remaining output")
		}
		stopRead = false
		for !stopRead {
			select {
			case <-ctx.Done():
				if !interrupt {
					// Above we had not yet encountered a cancellation,
					// and now we have one. This is a hard fail.
					killCmd()
				}
			case <-stopRequested:
				// Ditto.
				if !interrupt {
					killCmd()
				}

			case <-timer:
				if interrupt {
					// Interrupting the command softly, after a timeout.
					killCmd()
				}

			case res := <-lines:
				// log.Infof(ctx, "received from reader: %+v", res)
				if res.err != nil {
					coll.errs = append(coll.errs, res.err)
				}
				if res.line != "" {
					if err := consumer(res.line); err != nil {
						coll.errs = append(coll.errs, err)
						stopRead = true
						break
					}
				}
				if res.err == nil && res.line == "" {
					stopRead = true
					break
				}
			}
		}
		close(readerDone)
	}()

	// The command should really have terminated by now.
	if log.V(1) {
		log.Info(ctx, "waiting")
	}
	exitErr = cmd.Wait()
	ps = cmd.ProcessState
	if log.V(1) {
		log.Infof(ctx, "terminated: %s", ps)
	}

	<-readerDone
	if log.V(1) {
		log.Infof(ctx, "%d errors encountered", len(coll.errs))
	}

	// Finalize the errors.
	if len(coll.errs) == 0 {
		return ps, nil, exitErr
	} else if len(coll.errs) == 1 {
		return ps, coll.errs[0], exitErr
	}
	return ps, &coll, exitErr
}

func (a *actor) prepareActionCommands() error {
	actionDir := filepath.Join(a.workDir, "actions")
	if err := os.Mkdir(actionDir, 0755); err != nil {
		return errors.Wrap(err, "mkdir")
	}
	for actName, actCmd := range a.role.actionCmds {
		actScript := filepath.Join(actionDir, actName+".sh")
		a.actionScripts[actName] = actScript
		if err := a.prepareScript(actScript, actName, actCmd, true /*redirect*/); err != nil {
			return errors.Wrapf(err, "preparing script for action %s", actName)
		}
	}
	if a.role.spotlightCmd != "" {
		a.spotlightScript = filepath.Join(actionDir, "_spotlight.sh")
		if err := a.prepareScript(a.spotlightScript, "_spotlight",
			a.role.spotlightCmd, false /*redirect*/); err != nil {
			return errors.Wrapf(err, "preparing script for spotlight")
		}
	}
	if a.role.cleanupCmd != "" {
		a.cleanupScript = filepath.Join(actionDir, "_cleanup.sh")
		if err := a.prepareScript(a.cleanupScript,
			"_cleanup", a.role.cleanupCmd, true /*redirect*/); err != nil {
			return errors.Wrap(err, "preparing script for cleanup")
		}
	}
	return nil
}

func (a *actor) prepareScript(scriptName, actName string, pCmd cmd, redirect bool) (err error) {
	// Ensure the new file is executable.
	f, err := os.OpenFile(scriptName, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	defer func() { err = f.Close() }()
	fmt.Fprintf(f, "#!%s\n", a.shellPath)
	// set -euao pipefail:
	//    -e fail commands on error
	//    -u fail command if a variable is not set
	//    -o pipefail   fail entire pipeline if one command fails
	//    -a auto-exports all variables
	f.WriteString("set -euao pipefail\n")
	// Ensure files are created from the working directory.
	fmt.Fprintf(f, "cd '%s'\n", a.workDir)
	fmt.Fprintf(f, "TMPDIR=$PWD HOME=$PWD/..\n")
	if redirect {
		// Output a timestamp in the log to distinguish runs.
		fmt.Fprintf(f, "TZ=UTC date +%%Y-%%m-%%dT%%H:%%M:%%SZ >>%s.log\n", actName)
		// Inform the shakespeare log of where the output is going
		fmt.Fprintf(f, "echo output redirected to %s/%s.log\n", a.workDir, actName)
		// Retain the stdout/stderr output to a per-action log.
		fmt.Fprintf(f, "exec >>%s.log 2>&1\n", actName)
	}
	// Trace the execution. We do this before setting the
	// environment so as to see the expanded values.
	f.WriteString("set -x\n")
	// If there is custom environment for this actor, include it.
	if a.extraEnv != "" {
		f.WriteString(a.extraEnv)
		f.WriteString("\n")
	}
	// Finally, the main command.
	f.WriteString(string(pCmd))
	f.WriteString("\n")
	return nil
}

func (a *actor) makeShCmd(ctx context.Context, bindCtx bool, script string) *exec.Cmd {
	var cmd *exec.Cmd
	if bindCtx {
		cmd = exec.CommandContext(ctx, script)
	} else {
		cmd = exec.Command(script)
	}

	// We want a separate process group.
	// On BSD (incl macOS) we also need a separate session ID for that.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: 0}

	cmd.Stdin = nil
	cmd.Dir = a.workDir
	return cmd
}

type res struct {
	line string
	err  error
}

// runReaderAsync reads lines of output from the command while it is
// running. The lines and any read errors are written to the lines
// chan.  It assumes that the underlying reader closes when the
// command is stopped. The lines chan is closed when the reader
// completes / is terminated.
func runReaderAsync(
	ctx context.Context,
	stopper *stop.Stopper,
	rd *bufio.Reader,
	lines chan<- res,
	readerDone <-chan struct{},
) {
	readCtx := logtags.AddTag(ctx, "reader", nil)
	runWorker(readCtx, stopper, func(ctx context.Context) {
		log.Info(readCtx, "<intrat>")
		defer func() {
			close(lines)
			log.Info(readCtx, "<exit>")
		}()
		// The reader runs asynchronously, until there is no more data to
		// read or the context is canceled.
		for {
			line, err := rd.ReadString('\n')
			if log.V(1) {
				log.Infof(readCtx, "data: %q, err = %v", line, err)
			}
			// line = strings.TrimSpace(line)
			if line != "" {
				select {
				case lines <- res{line, nil}:
				case <-readerDone:
					if log.V(1) {
						log.Info(readCtx, "reader was asked to stop")
					}
					return
				}
			}
			if err != nil {
				if p, ok := err.(*os.PathError); ok && errors.Is(p.Err, os.ErrClosed) {
					// The command has cleaned up the channel "under us".
					return
				}
				if !errors.Is(err, io.EOF) {
					select {
					case lines <- res{"", errors.WithContextTags(errors.WithStack(err), ctx)}:
					case <-readerDone:
						if log.V(1) {
							log.Info(readCtx, "reader was asked to stop during error: %v", err)
						}
						return
					}
				}
				return
			}
		}
	})
}
