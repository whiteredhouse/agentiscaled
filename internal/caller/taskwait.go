package caller

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/whiteredhouse/agentiscaled/internal/cli"
	"github.com/whiteredhouse/agentiscaled/internal/nodestate"
)

const taskWaitUsage = `usage: agentiscaled task wait [flags] TASK

Waits for task TASK, such as one sent with task send -no-wait, to end or to
ask a question, printing the agent's steps on standard error while it works
and its answer on standard output. A task that has already ended or asks a
question returns at once. Waiting never stops the task: when -timeout passes
or you press Ctrl-C, the task goes on and you can wait for it again.

TASK is the reference task send printed, PEER/AGENT/TASK-ID: it names the
agent and the node it runs on. Only the node that sent a task can follow it.
` + taskExitCodes + `
Flags:
`

// taskExitCodes is the part of the usage of task send and task wait that
// explains their exit status.
const taskExitCodes = `
Exit status:
  0  the task completed
  1  the command failed, such as when the agent cannot be reached
  2  the task ended without completing: failed, rejected or cancelled
  3  the agent asks a question (input required): answer with task send -task
  4  -timeout passed before the task settled
`

// The exit statuses of task send and task wait, besides 0 (completed) and
// 1 (the command failed).
const (
	exitNotCompleted  = 2
	exitInputRequired = 3
	exitTimeout       = 4
)

// exitError is an error that ends agentiscaled with a given exit status.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

// withExit gives err the exit status code; a nil err stays nil.
func withExit(code int, err error) error {
	if err == nil || code == 1 {
		return err
	}
	return exitError{code, err}
}

// ExitCode is the exit status err ends agentiscaled with.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e exitError
	if errors.As(err, &e) {
		return e.code
	}
	return 1
}

// timeoutExit is the exit status of a call whose context is done: timeout
// when its deadline passed, else (an interrupt) failure.
func timeoutExit(ctx context.Context) int {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return exitTimeout
	}
	return 1
}

// safeError is err without what could drive a terminal, keeping its exit
// status.
func safeError(err error) error {
	if err == nil {
		return nil
	}
	return withExit(ExitCode(err), errors.New(terminalSafe(err.Error())))
}

// RunTaskWait is `agentiscaled task wait`: it follows a task the node sent
// until the task ends or asks a question.
func RunTaskWait(ctx context.Context, args []string, out, errOut io.Writer) error {
	var o taskSendOptions
	fs := flag.NewFlagSet("task wait", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprint(errOut, taskWaitUsage)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.dataDir, "data-dir", nodestate.DefaultDataDir(), "Data directory of the running node that sent the task")
	fs.DurationVar(&o.timeout, "timeout", DefaultTaskDeadline, "Stop waiting after this long; the task goes on")
	fs.BoolVar(&o.json, "json", false, "Print the task's reference, ID, state and result as JSON instead of the answer alone")
	pos, err := cli.ParseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errors.New("task wait takes a task's reference, as task send printed it")
	}
	ref, err := ParseTaskRef(pos[0])
	if err != nil {
		return err
	}
	if o.timeout <= 0 {
		return errors.New("-timeout must be positive")
	}

	if err := nodestate.CheckHalt(o.dataDir); err != nil {
		return err
	}
	// -timeout counts from now, waiting for a node that is starting too.
	deadline := time.Now().Add(o.timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	tasks, err := openClient(ctx, o.dataDir)
	if err != nil {
		return timedOut(err, ctx, o.timeout)
	}
	if f, ok := out.(*os.File); ok {
		o.terminal = cli.IsTerminal(f)
	}
	return safeError(waitForTask(ctx, deadline, tasks, ref, o, out, errOut))
}

// lastReadTimeout bounds the read of a task's state after a wait ends.
var lastReadTimeout = cancelTimeout

// waitForTask follows task ref until it settles or the deadline, which ends
// ctx, passes, and reports it.
func waitForTask(ctx context.Context, deadline time.Time, tasks *Client, ref TaskRef, o taskSendOptions, out, errOut io.Writer) error {
	target, agent, id := ref.Target, ref.Service, ref.ID
	o.ref = ref.String()
	task, err := followTask(ctx, tasks, target, agent, Task{TaskID: id}, deadline, errOut)
	task.TaskID = id
	if ctx.Err() != nil {
		why := endedBy(ctx, o.timeout)
		// The wait ended in the middle of a watch: read where the task is,
		// to report it.
		readCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), lastReadTimeout)
		defer stop()
		if now, err := tasks.Watch(readCtx, target, a2a.TaskID(id), 0, nil); err == nil {
			task = now
		}
		if task.Terminal != "" || task.State == StateName(a2a.TaskStateInputRequired) {
			// It settled as time ran out.
			return settle(out, o, target, agent, task)
		}
		state := "has not settled"
		if task.State != "" {
			state = "is still " + task.State
		}
		// The task's ID and the command to wait again, even when its state
		// could not be read: a script goes on from them.
		return report(out, o, task, withExit(timeoutExit(ctx), fmt.Errorf("task %s %s: %s; it goes on, wait again with: %s\nor cancel it with: %s", id, state, why, waitCommand(o, ref), taskCommand("cancel", o, ref))))
	}
	if err != nil {
		return err
	}
	return settle(out, o, target, agent, task)
}

// waitCommand is the command that waits for task ref, whose ID is a plain
// taskID.
func waitCommand(o taskSendOptions, ref TaskRef) string {
	return taskCommand("wait", o, ref)
}

// taskCommand is the task subcommand cmd (wait or cancel) for task ref,
// whose ID is a plain taskID.
func taskCommand(cmd string, o taskSendOptions, ref TaskRef) string {
	wait := []string{"agentiscaled task " + cmd}
	if o.dataDir != nodestate.DefaultDataDir() {
		wait = append(wait, "-data-dir", cli.ShellQuote(o.dataDir))
	}
	return strings.Join(append(wait, ref.String()), " ")
}
