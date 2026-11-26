package caller

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/whiteredhouse/agentiscaled/internal/cli"
	"github.com/whiteredhouse/agentiscaled/internal/nodestate"
)

const taskSendUsage = `usage: agentiscaled task send [flags] AGENT [MESSAGE]

Sends MESSAGE to AGENT on your network through this machine's running node,
prints the agent's steps on standard error while it works, and prints its
answer on standard output. Without MESSAGE, or with -, the message is read
from standard input. With -no-wait it prints the task's ID and returns at
once; agentiscaled task wait follows the task from there.

AGENT is an agent this node hosts, or one another node of your network
offers; give -peer when several nodes offer an agent of that name.
` + taskExitCodes + `
Flags:
`

// maxMessageBytes caps a message read from standard input.
const maxMessageBytes = 1 << 20

// followChunk is how long one watch of the task lasts before the next: a
// stream from the agent ends after a while, and each watch opens a new one.
var followChunk = 100 * time.Second

// findEvery is how often task send looks for a remote agent again while
// -wait-agent lasts.
var findEvery = 2 * time.Second

// cancelTimeout bounds the cancel task send sends when it gives up.
const cancelTimeout = 10 * time.Second

type taskSendOptions struct {
	dataDir   string
	peer      string
	taskID    string
	timeout   time.Duration
	waitAgent time.Duration
	json      bool
	noWait    bool
	// terminal is set when standard output is a terminal, which the
	// answer must not drive.
	terminal bool
}

// RunTaskSend is `agentiscaled task send`: it calls an agent from a script or
// a CI job and prints the answer.
func RunTaskSend(ctx context.Context, args []string, stdin io.Reader, out, errOut io.Writer) error {
	var o taskSendOptions
	fs := flag.NewFlagSet("task send", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprint(errOut, taskSendUsage)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.dataDir, "data-dir", nodestate.DefaultDataDir(), "Data directory of the running node that sends the task")
	fs.StringVar(&o.peer, "peer", "", "Peer ID of the node that offers AGENT, when several do")
	fs.StringVar(&o.taskID, "task", "", "ID of a task waiting for your answer (input required): MESSAGE answers its question")
	fs.DurationVar(&o.timeout, "timeout", DefaultTaskDeadline, "Give up and cancel the task after this long")
	fs.DurationVar(&o.waitAgent, "wait-agent", 0, "Keep looking this long for a remote AGENT that is not on the network yet, such as one that is joining")
	fs.BoolVar(&o.json, "json", false, "Print the task's ID, state and result as JSON instead of the answer alone")
	fs.BoolVar(&o.noWait, "no-wait", false, "Print the task's ID once the agent has it and return, leaving the task to run; follow it with agentiscaled task wait")
	pos, err := cli.ParseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		fs.Usage()
		return errors.New("task send takes an agent and a message")
	}
	agent := pos[0]
	if !ServiceName.MatchString(agent) {
		return fmt.Errorf("%q is not an agent name", agent)
	}
	text := "-"
	if len(pos) == 2 {
		text = pos[1]
	}
	if text == "-" {
		b, err := readMessage(ctx, stdin)
		if err != nil {
			return fmt.Errorf("read the message: %w", err)
		}
		if len(b) > maxMessageBytes {
			return errors.New("the message on standard input is larger than 1 MiB")
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("the message is empty")
	}
	if o.taskID != "" && !taskID.MatchString(o.taskID) {
		return fmt.Errorf("-task %q is not a task ID", o.taskID)
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
	// Errors carry the agent's words and the network's: shown on the
	// caller's terminal, they must not drive it.
	return safeError(sendAndFollow(ctx, deadline, tasks, agent, text, o, out, errOut))
}

// readMessage reads the message from standard input, or stops waiting for
// it when ctx ends, as on Ctrl-C.
func readMessage(ctx context.Context, stdin io.Reader) ([]byte, error) {
	type read struct {
		b   []byte
		err error
	}
	done := make(chan read, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(stdin, maxMessageBytes+1))
		done <- read{b, err}
	}()
	select {
	case r := <-done:
		return r.b, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// sendAndFollow finds agent, sends it text and follows the task until it
// settles or the deadline, which ends ctx, passes.
func sendAndFollow(ctx context.Context, deadline time.Time, tasks *Client, agent, text string, o taskSendOptions, out, errOut io.Writer) error {
	target, where, err := findAgent(ctx, tasks, agent, o.peer, o.waitAgent, true)
	if err != nil {
		return timedOut(err, ctx, o.timeout)
	}
	sent, err := tasks.Send(ctx, target, text, o.taskID)
	if err != nil {
		if ctx.Err() == nil {
			return err
		}
		// The agent may have taken the task before the caller went: stop
		// the task it knows, and say that a new one may still run.
		if o.taskID != "" {
			return stopTask(ctx, tasks, target, o, out, Task{TaskID: o.taskID}, err)
		}
		return withExit(timeoutExit(ctx), fmt.Errorf("%s before %s confirmed the task, which may still run: %w", endedBy(ctx, o.timeout), agent, err))
	}
	id := sent.TaskID
	if !taskID.MatchString(id) {
		// Not a task this command can name: stop it rather than leave it.
		stopCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
		defer stop()
		tasks.Cancel(stopCtx, target, id)
		return fmt.Errorf("%s answered with %q, which is not a task ID; cancelled it", agent, id)
	}
	if o.taskID == "" {
		fmt.Fprintf(errOut, "sent task %s to %s %s\n", id, agent, where)
	} else {
		fmt.Fprintf(errOut, "answered task %s of %s %s\n", id, agent, where)
	}

	if o.noWait {
		fmt.Fprintf(errOut, "follow it with: %s\n", waitCommand(o, target, id))
		if o.json {
			return report(out, o, sent, nil)
		}
		if _, err := fmt.Fprintln(out, id); err != nil {
			return fmt.Errorf("write the task ID: %w", err)
		}
		return nil
	}

	task, err := followTask(ctx, tasks, target, agent, sent, deadline, errOut)
	if ctx.Err() != nil {
		// The caller is gone, or out of time: the agent should stop too.
		return stopTask(ctx, tasks, target, o, out, task, nil)
	}
	if err != nil {
		return err
	}
	return settle(out, o, target, agent, task)
}

// followTask follows task until it ends or asks a question, the deadline
// passes or ctx ends, printing each new step of the agent's on errOut, and
// returns the task as last seen. Callers check ctx before the error.
func followTask(ctx context.Context, tasks *Client, target Target, agent string, task Task, deadline time.Time, errOut io.Writer) (Task, error) {
	last := ""
	notify := func(step string) {
		if step = ProgressLine(terminalSafe(step)); step != "" && step != last {
			last = step
			fmt.Fprintf(errOut, "%s: %s\n", agent, step)
		}
	}
	for task.Terminal == "" && task.State != StateName(a2a.TaskStateInputRequired) {
		wait := min(time.Until(deadline), followChunk)
		if wait <= 0 {
			// Out of time: the deadline is ending the context too.
			<-ctx.Done()
			break
		}
		next, err := tasks.Watch(ctx, target, a2a.TaskID(task.TaskID), wait, notify)
		if err != nil {
			if next.State != "" {
				task = next
			}
			return task, err
		}
		task = next
	}
	return task, nil
}

// settle reports a task that ended or asks a question: the answer of a
// completed task, else an error whose exit status says which.
func settle(out io.Writer, o taskSendOptions, target Target, agent string, task Task) error {
	switch {
	case task.Terminal == "completed":
		return report(out, o, task, nil)
	case task.State == StateName(a2a.TaskStateInputRequired):
		return report(out, o, task, withExit(exitInputRequired, fmt.Errorf("%s asks: %s\nanswer with: %s", agent, terminalSafe(task.Message), answerCommand(o, target, task.TaskID))))
	default:
		why := task.Terminal
		if task.Message != "" {
			why += ": " + terminalSafe(task.Message)
		}
		return report(out, o, task, withExit(exitNotCompleted, fmt.Errorf("task %s ended %s", terminalSafe(task.TaskID), why)))
	}
}

// taskID is the form of the task IDs task send takes and shows: agentiscaled
// agents name tasks with UUIDs. An ID in any other form, which only another
// A2A server would send, is refused rather than shown or put in a command.
var taskID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// answerCommand is the command that answers the question of task id, a
// plain taskID; -- keeps an answer that starts with a dash an answer.
func answerCommand(o taskSendOptions, target Target, id string) string {
	answer := []string{"agentiscaled task send"}
	if o.dataDir != nodestate.DefaultDataDir() {
		answer = append(answer, "-data-dir", cli.ShellQuote(o.dataDir))
	}
	answer = append(answer, "-task", id)
	if target.PeerID != "" {
		answer = append(answer, "-peer", target.PeerID)
	}
	return strings.Join(append(answer, target.Service, "--", `"<your answer>"`), " ")
}

// endedBy says what ended a call whose context is done.
func endedBy(ctx context.Context, timeout time.Duration) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("no answer within %s", timeout)
	}
	return "interrupted"
}

// stopTask cancels task once ctx is done, so that the agent stops when the
// caller does, and reports how that went; cause is what the caller was
// doing when it stopped, if it failed.
func stopTask(ctx context.Context, tasks *Client, target Target, o taskSendOptions, out io.Writer, task Task, cause error) error {
	why := endedBy(ctx, o.timeout)
	if cause != nil {
		why += fmt.Sprintf(" (%v)", cause)
	}
	id := terminalSafe(task.TaskID)
	code := timeoutExit(ctx)
	stopCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer stop()
	if cancelled, err := tasks.Cancel(stopCtx, target, task.TaskID); err == nil {
		return report(out, o, cancelled, withExit(code, fmt.Errorf("task %s: %s; cancelled it", id, why)))
	}
	return report(out, o, task, withExit(code, fmt.Errorf("task %s: %s; cancelling it failed, it may still run", id, why)))
}

// report prints the task: its result alone, or with -json all of it, and
// returns err, or else an error writing it. On a terminal the result loses
// the escape sequences that could drive it; a pipe or file gets it as the
// agent wrote it.
func report(out io.Writer, o taskSendOptions, task Task, err error) error {
	var werr error
	if o.json {
		b, _ := json.MarshalIndent(task, "", "  ")
		_, werr = fmt.Fprintf(out, "%s\n", b)
	} else if err == nil && task.Result != "" {
		result := task.Result
		if o.terminal {
			result = terminalSafe(result)
		}
		_, werr = fmt.Fprintln(out, strings.TrimRight(result, "\n"))
	}
	if err == nil && werr != nil {
		return fmt.Errorf("write the answer: %w", werr)
	}
	return err
}

// timedOut names the timeout when it, rather than err itself, ended a call.
func timedOut(err error, ctx context.Context, timeout time.Duration) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return withExit(exitTimeout, fmt.Errorf("no answer within %s: %w", timeout, err))
	}
	return err
}

// findAgent picks the agent to call: one this node hosts, unless peer names
// another node; else the one node of the network that offers it. It says
// where the agent is, for the caller. For a new task (fresh), an agent that
// is draining counts only when no other agent of the name takes tasks:
// the drained one then refuses the task with its owner's reason. A task
// that exists already stays with its agent, drained or not.
func findAgent(ctx context.Context, tasks *Client, agent, peer string, wait time.Duration, fresh bool) (Target, string, error) {
	if peer != "" {
		return Target{PeerID: peer, Service: agent}, "on node " + peer, nil
	}
	info, err := tasks.LocalInfo(ctx)
	_, localDrained := info.Draining[agent]
	local := err == nil && slices.Contains(info.Services, agent)
	if local && (!fresh || !localDrained) {
		return Target{Service: agent}, "on this node", nil
	}
	// -wait-agent bounds the search, lookups included, and leaves room for
	// a few of them however short it is.
	find, every := ctx, findEvery
	if wait > 0 {
		var cancel context.CancelFunc
		find, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
		every = min(findEvery, max(wait/5, 50*time.Millisecond))
	}
	var missing error
	for {
		peers, drained, err := discoverAgent(find, tasks.Mesh, tasks.BaseURL, agent, info.PeerID)
		if err == nil && fresh && len(peers) > len(drained) {
			peers = slices.DeleteFunc(peers, func(p string) bool { return slices.Contains(drained, p) })
		} else if err == nil && local {
			// Every other agent of the name drains too: this node's
			// refuses the task with its reason.
			return Target{Service: agent}, "on this node", nil
		}
		switch {
		case err != nil && find.Err() != nil && ctx.Err() == nil && missing != nil:
			// The search's own time ran out during a lookup.
			return Target{}, "", missing
		case err != nil && local:
			return Target{Service: agent}, "on this node", nil
		case err != nil:
		case len(peers) == 1:
			return Target{PeerID: peers[0], Service: agent}, "on node " + peers[0], nil
		case len(peers) > 1:
			return Target{}, "", fmt.Errorf("%d nodes offer %s: pick one with -peer (%s)", len(peers), agent, strings.Join(peers, ", "))
		default:
			err = fmt.Errorf("no node of your network offers an agent named %s: it is not online or not shared (see agentiscaled agent list on its machine)", agent)
			missing = err
		}
		if wait <= 0 || ctx.Err() != nil {
			return Target{}, "", err
		}
		select {
		case <-find.Done():
			return Target{}, "", err
		case <-time.After(every):
		}
	}
}

// discoverAgent lists the other nodes that offer A2A agent name, through
// the node's discovery API, and those of them whose agent is draining.
func discoverAgent(ctx context.Context, client *http.Client, base, name, self string) (peers, drained []string, err error) {
	found, err := readDiscovery(ctx, client, base, name)
	if err != nil {
		return nil, nil, fmt.Errorf("look for %s on the network: %w", name, err)
	}
	for _, row := range ListedAgents(LocalInfo{PeerID: self}, "", found).Agents {
		if row.Service == name {
			peers = append(peers, row.PeerID)
			if row.Draining != nil {
				drained = append(drained, row.PeerID)
			}
		}
	}
	return peers, drained, nil
}

// escapeSequence is a terminal escape sequence: CSI (colours, cursor and
// screen control) or OSC (titles, links, clipboard).
var escapeSequence = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)?`)

// terminalSafe is s without escape sequences or control characters, other
// than line breaks and tabs, so that an agent's words shown on a terminal
// cannot drive it.
func terminalSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, escapeSequence.ReplaceAllString(s, ""))
}
