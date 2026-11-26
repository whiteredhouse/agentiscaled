package caller

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/whiteredhouse/agentiscaled/internal/cli"
	"github.com/whiteredhouse/agentiscaled/internal/nodestate"
)

const taskCancelUsage = `usage: agentiscaled task cancel [flags] AGENT TASK...

Cancels tasks of AGENT that this node sent, such as ones sent with task send
-no-wait that a script no longer waits for, so that the agent stops working
on them and can take other tasks. A task that has already ended is left as
it is. An empty TASK is skipped, so that a cleanup step can pass the IDs of
tasks that may not have been sent. Put -- before AGENT when a TASK could
start with a dash, so that it is taken as a task ID, not a flag.

The tasks are cancelled together, so a slow answer about one does not hold
up the others. It prints one line for each task, and exits 0 when each task
is cancelled or had already ended, else 1.

Flags:
`

// cancelWait is the default of task cancel's -timeout.
const cancelWait = 30 * time.Second

// RunTaskCancel is `agentiscaled task cancel`: it stops tasks this node sent.
func RunTaskCancel(ctx context.Context, args []string, out, errOut io.Writer) error {
	var o taskSendOptions
	fs := flag.NewFlagSet("task cancel", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprint(errOut, taskCancelUsage)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.dataDir, "data-dir", nodestate.DefaultDataDir(), "Data directory of the running node that sent the tasks")
	fs.StringVar(&o.peer, "peer", "", "Peer ID of the node that offers AGENT, when several do")
	fs.DurationVar(&o.timeout, "timeout", cancelWait, "Give up after this long")
	fs.BoolVar(&o.json, "json", false, "Print each task's ID, state and result as a JSON array")
	pos, err := cli.ParseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		fs.Usage()
		return errors.New("task cancel takes an agent and one or more task IDs")
	}
	agent := pos[0]
	if !ServiceName.MatchString(agent) {
		return fmt.Errorf("%q is not an agent name", agent)
	}
	var ids []string
	for _, id := range pos[1:] {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if o.timeout <= 0 {
		return errors.New("-timeout must be positive")
	}
	if len(ids) == 0 {
		fmt.Fprintln(errOut, "no task to cancel")
		if o.json {
			fmt.Fprintln(out, "[]")
		}
		return nil
	}

	if err := nodestate.CheckHalt(o.dataDir); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	tasks, err := openClient(ctx, o.dataDir)
	if err != nil {
		return err
	}
	return safeError(cancelTasks(ctx, tasks, agent, ids, o, out))
}

// cancelled is what task cancel prints about a task with -json: the task,
// or why it could not be cancelled.
type cancelled struct {
	Task
	Error string `json:"error,omitempty"`
}

// cancelTasks finds agent and cancels each of its tasks ids, all at once,
// reporting each in order; it fails if any task could not be cancelled.
func cancelTasks(ctx context.Context, tasks *Client, agent string, ids []string, o taskSendOptions, out io.Writer) error {
	target, _, err := findAgent(ctx, tasks, agent, o.peer, 0, false)
	if err != nil {
		return err
	}
	shown := make([]cancelled, len(ids))
	lines := make([]string, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !taskID.MatchString(id) {
				// Not an ID this command can name: the others still go.
				shown[i] = cancelled{Task{TaskID: id}, "not a task ID"}
				lines[i] = fmt.Sprintf("%q is not a task ID", id)
				return
			}
			task, line, err := cancelOne(ctx, tasks, target, id)
			if err != nil {
				shown[i] = cancelled{Task{TaskID: id}, err.Error()}
				lines[i] = fmt.Sprintf("task %s: %v", id, err)
				return
			}
			shown[i], lines[i] = cancelled{Task: task}, line
		}()
	}
	wg.Wait()
	failed := 0
	for i := range ids {
		if shown[i].Error != "" {
			failed++
		}
		if !o.json {
			fmt.Fprintln(out, terminalSafe(lines[i]))
		}
	}
	if o.json {
		b, _ := json.MarshalIndent(shown, "", "  ")
		fmt.Fprintf(out, "%s\n", b)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d tasks not cancelled", failed, len(ids))
	}
	return nil
}

// cancelOne cancels task id, or finds that it has already ended, and says
// which.
func cancelOne(ctx context.Context, tasks *Client, target Target, id string) (Task, string, error) {
	task, cerr := tasks.Cancel(ctx, target, id)
	if cerr == nil {
		line := fmt.Sprintf("task %s cancelled", id)
		if task.ExecutionBegun != nil && !*task.ExecutionBegun {
			line += " before the agent started on it"
		}
		return task, line, nil
	}
	if errors.Is(cerr, a2a.ErrTaskNotFound) {
		return Task{}, "", cerr
	}
	// The agent refuses to cancel a task that has ended: read it to tell.
	now, err := tasks.Watch(ctx, target, a2a.TaskID(id), 0, nil)
	if err != nil {
		// Why the cancel failed comes first; the read only adds to it.
		return Task{}, "", fmt.Errorf("%w (reading the task failed too: %v)", cerr, err)
	}
	if now.Terminal != "" {
		return now, fmt.Sprintf("task %s had already ended: %s", id, now.Terminal), nil
	}
	return Task{}, "", cerr
}
