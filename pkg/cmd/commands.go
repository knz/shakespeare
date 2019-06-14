package cmd

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/knz/shakespeare/pkg/crdb/log"
	"github.com/knz/shakespeare/pkg/crdb/log/logtags"
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
func (a *actor) runActorCommand(
	ctx context.Context, stopper *stop.Stopper, timeout time.Duration, interruptible bool, pCmd cmd,
) (outdata string, ps *os.ProcessState, err error) {
	cmd := a.makeShCmd(pCmd)
	log.Infof(ctx, "running: %s", strings.Join(cmd.Args, " "))

	if timeout != 0 {
		// If the command does not complete within the specified timeout, we'll terminate it.
		var cancel func()
		ctx, cancel = context.WithDeadline(ctx, timeutil.Now().Add(timeout))
		defer cancel()
	}

	stopRequested := stopper.ShouldStop()
	if !interruptible {
		// If not interruptible, we want to ignore the stopper.
		// Just override the channel.
		stopRequested = make(chan struct{})
	}

	// We're going to run a command monitor in a separate goroutine. To
	// ensure this goroutine exits cleanly in the common case when the
	// command terminates in a timely manner, we'll use this complete
	// channel and close it on the return path.
	complete := make(chan struct{})
	defer func() { close(complete) }()

	// Start the command monitor.
	stopCtx := logtags.AddTag(ctx, "monitor", nil)
	runWorker(stopCtx, stopper, func(ctx context.Context) {
		select {
		case <-complete:
			return
		case <-stopRequested:
			log.Info(ctx, "interrupted")
		case <-ctx.Done():
			log.Info(ctx, "canceled")
		}
		proc := cmd.Process
		if proc != nil {
			proc.Signal(os.Interrupt)
			time.Sleep(1)
			proc.Kill()
		}
	})

	// Run the command and collect its output.
	outdataB, err := cmd.CombinedOutput()
	outdata = string(outdataB)
	log.Infof(ctx, "done\n%s\n-- %s", outdata, cmd.ProcessState)
	return outdata, cmd.ProcessState, err
}

func (a *actor) makeShCmd(pcmd cmd) exec.Cmd {
	cmd := exec.Cmd{
		Path: a.shellPath,
		Dir:  a.workDir,
		// set -euxo pipefail:
		//    -e fail commands on error
		//    -x trace commands (and show variable expansions)
		//    -u fail command if a variable is not set
		//    -o pipefail   fail entire pipeline if one command fails
		// trap: terminate all the process group when the shell exits.
		Args: []string{
			a.shellPath,
			"-c",
			`set -euo pipefail; export TMPDIR=$PWD HOME=$PWD/..; shpid=$$; trap "set +x; kill -TERM -$shpid 2>/dev/null || true" EXIT; set -x;` + "\n" + string(pcmd)},
	}
	if a.extraEnv != "" {
		cmd.Path = "/usr/bin/env"
		cmd.Args = append([]string{"/usr/bin/env", "-S", a.extraEnv}, cmd.Args...)
	}
	return cmd
}
