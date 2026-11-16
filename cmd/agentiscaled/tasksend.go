package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/a2aproject/a2a-go/v2/a2a"
	meshnode "github.com/google/agentmesh/export/node"
)

const taskSendUsage = `usage: agentiscaled task send [flags] AGENT [MESSAGE]

Sends MESSAGE to AGENT on your network through this machine's running node,
prints the agent's steps on standard error while it works, and prints its
answer on standard output. Without MESSAGE, or with -, the message is read
from standard input. It exits non-zero unless the task completes.

AGENT is an agent this node hosts, or one another node of your network
offers; give -peer when several nodes offer an agent of that name.

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
}

// runTaskSend is `agentiscaled task send`: it calls an agent from a script or
// a CI job and prints the answer.
func runTaskSend(ctx context.Context, args []string, stdin io.Reader, out, errOut io.Writer) error {
	var o taskSendOptions
	fs := flag.NewFlagSet("task send", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprint(errOut, taskSendUsage)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.dataDir, "data-dir", defaultDataDir(), "Data directory of the running node that sends the task")
	fs.StringVar(&o.peer, "peer", "", "Peer ID of the node that offers AGENT, when several do")
	fs.StringVar(&o.taskID, "task", "", "ID of a task waiting for your answer (input required): MESSAGE answers its question")
	fs.DurationVar(&o.timeout, "timeout", taskDeadline, "Give up and cancel the task after this long")
	fs.DurationVar(&o.waitAgent, "wait-agent", 0, "Keep looking this long for a remote AGENT that is not on the network yet, such as one that is joining")
	fs.BoolVar(&o.json, "json", false, "Print the task's ID, state and result as JSON instead of the answer alone")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		fs.Usage()
		return errors.New("task send takes an agent and a message")
	}
	agent := pos[0]
	if !serviceName.MatchString(agent) {
		return fmt.Errorf("%q is not an agent name", agent)
	}
	text := "-"
	if len(pos) == 2 {
		text = pos[1]
	}
	if text == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, maxMessageBytes+1))
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
	if o.timeout <= 0 {
		return errors.New("-timeout must be positive")
	}

	if h, err := loadHalt(o.dataDir); err != nil {
		return err
	} else if h != nil {
		return errors.New(h.Message)
	}
	socket := filepath.Join(o.dataDir, meshnode.DefaultSocketName)
	if err := waitForSocket(ctx, socket, nodeStartWait); err != nil {
		return fmt.Errorf("no running agentiscaled at %s: start it first (%w)", socket, err)
	}
	node := socketClient(socket)
	tasks := &a2aTasks{http: node, base: "http://agentiscaled", local: socketClient(filepath.Join(o.dataDir, localAgentSocketName))}
	return sendAndFollow(ctx, tasks, agent, text, o, out, errOut)
}

// sendAndFollow finds agent, sends it text and follows the task until it
// settles or o.timeout passes.
func sendAndFollow(ctx context.Context, tasks *a2aTasks, agent, text string, o taskSendOptions, out, errOut io.Writer) error {
	deadline := time.Now().Add(o.timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	target, where, err := findAgent(ctx, tasks, agent, o.peer, o.waitAgent)
	if err != nil {
		return timedOut(err, ctx, o.timeout)
	}
	_, sent, err := tasks.send(ctx, nil, sendTaskIn{taskTarget: target, Text: text, TaskID: o.taskID})
	if err != nil {
		return timedOut(err, ctx, o.timeout)
	}
	if o.taskID == "" {
		fmt.Fprintf(errOut, "sent task %s to %s %s\n", sent.TaskID, agent, where)
	} else {
		fmt.Fprintf(errOut, "answered task %s of %s %s\n", sent.TaskID, agent, where)
	}

	last := ""
	notify := func(step string) {
		if step = progressLine(terminalSafe(step)); step != "" && step != last {
			last = step
			fmt.Fprintf(errOut, "%s: %s\n", agent, step)
		}
	}
	task := sent
	for task.Terminal == "" && task.State != stateName(a2a.TaskStateInputRequired) {
		wait := min(time.Until(deadline), followChunk)
		if wait <= 0 {
			// Out of time: the deadline is ending the context too.
			<-ctx.Done()
			break
		}
		task, err = tasks.watch(ctx, target, a2a.TaskID(sent.TaskID), wait, notify)
		if err != nil {
			break
		}
	}
	if ctx.Err() != nil {
		// The caller is gone, or out of time: the agent should stop too.
		why := "interrupted"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			why = fmt.Sprintf("no answer within %s", o.timeout)
		}
		stopCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
		defer stop()
		if _, cancelled, cerr := tasks.cancel(stopCtx, nil, taskIn{taskTarget: target, TaskID: sent.TaskID}); cerr == nil {
			task = cancelled
			return report(out, o.json, task, fmt.Errorf("task %s: %s; cancelled it", sent.TaskID, why))
		}
		return report(out, o.json, task, fmt.Errorf("task %s: %s; cancelling it failed, it may still run", sent.TaskID, why))
	}
	if err != nil {
		return err
	}
	switch {
	case task.Terminal == "completed":
		return report(out, o.json, task, nil)
	case task.State == stateName(a2a.TaskStateInputRequired):
		answer := []string{"agentiscaled task send"}
		if o.dataDir != defaultDataDir() {
			answer = append(answer, "-data-dir", shellQuote(o.dataDir))
		}
		answer = append(answer, "-task", task.TaskID)
		if target.PeerID != "" {
			answer = append(answer, "-peer", target.PeerID)
		}
		answer = append(answer, agent, `"<your answer>"`)
		return report(out, o.json, task, fmt.Errorf("%s asks: %s\nanswer with: %s", agent, terminalSafe(task.Message), strings.Join(answer, " ")))
	default:
		why := task.Terminal
		if task.Message != "" {
			why += ": " + terminalSafe(task.Message)
		}
		return report(out, o.json, task, fmt.Errorf("task %s ended %s", task.TaskID, why))
	}
}

// report prints the task: its result alone, or with asJSON all of it, and
// returns err.
func report(out io.Writer, asJSON bool, task taskOut, err error) error {
	if asJSON {
		b, _ := json.MarshalIndent(task, "", "  ")
		fmt.Fprintf(out, "%s\n", b)
	} else if err == nil && task.Result != "" {
		fmt.Fprintln(out, strings.TrimRight(task.Result, "\n"))
	}
	return err
}

// timedOut names the timeout when it, rather than err itself, ended a call.
func timedOut(err error, ctx context.Context, timeout time.Duration) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("no answer within %s: %w", timeout, err)
	}
	return err
}

// findAgent picks the agent to call: one this node hosts, unless peer names
// another node; else the one node of the network that offers it. It says
// where the agent is, for the caller.
func findAgent(ctx context.Context, tasks *a2aTasks, agent, peer string, wait time.Duration) (taskTarget, string, error) {
	if peer != "" {
		return taskTarget{PeerID: peer, Service: agent}, "on node " + peer, nil
	}
	info, err := tasks.localInfo(ctx)
	if err == nil && slices.Contains(info.Services, agent) {
		return taskTarget{Service: agent}, "on this node", nil
	}
	until := time.Now().Add(wait)
	for {
		peers, err := discoverAgent(ctx, tasks.http, tasks.base, agent, info.PeerID)
		switch {
		case err != nil:
		case len(peers) == 1:
			return taskTarget{PeerID: peers[0], Service: agent}, "on node " + peers[0], nil
		case len(peers) > 1:
			return taskTarget{}, "", fmt.Errorf("%d nodes offer %s: pick one with -peer (%s)", len(peers), agent, strings.Join(peers, ", "))
		default:
			err = fmt.Errorf("no node of your network offers an agent named %s: it is not online or not shared (see agentiscaled agent list on its machine)", agent)
		}
		if !time.Now().Add(findEvery).Before(until) {
			return taskTarget{}, "", err
		}
		select {
		case <-ctx.Done():
			return taskTarget{}, "", fmt.Errorf("%w (%v)", err, ctx.Err())
		case <-time.After(findEvery):
		}
	}
}

// discoverAgent lists the other nodes that offer A2A agent name, through
// the node's discovery API.
func discoverAgent(ctx context.Context, client *http.Client, base, name, self string) ([]string, error) {
	q := url.Values{"type": {"a2a"}, "name": {name}, "limit": {"100"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/mesh/service/discover?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("look for %s on the network: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("look for %s on the network: %s: %s", name, resp.Status, strings.TrimSpace(string(b)))
	}
	var found []struct {
		PeerID  string `json:"peer_id"`
		SrvName string `json:"srv_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&found); err != nil {
		return nil, fmt.Errorf("look for %s on the network: %w", name, err)
	}
	var peers []string
	for _, p := range found {
		if p.SrvName == name && p.PeerID != "" && p.PeerID != self && !slices.Contains(peers, p.PeerID) {
			peers = append(peers, p.PeerID)
		}
	}
	slices.Sort(peers)
	return peers, nil
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
