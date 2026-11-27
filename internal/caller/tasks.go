// Package caller calls A2A agents through the running node.
package caller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Match the node's remote-card ingestion budget, with a separate call deadline.
const MaxAgentCardBytes = 1 << 20
const agentCardTimeout = 30 * time.Second

const (
	MetaTerminal       = "agentiscale.terminal"
	MetaExecutionBegun = "agentiscale.execution_begun"
)

const DefaultLocalInfoTimeout = 2 * time.Second

// Client reaches agents through the mesh API and the local agent API.
// The two HTTP clients carry the identity of the owner or the delegated agent.
type Client struct {
	Mesh    *http.Client
	BaseURL string
	Local   *http.Client
	Self    AgentName
	Sent    SentTasks

	// Zero uses the default for each interval.
	LocalInfoTimeout time.Duration
	StreamCheckEvery time.Duration
}

// SentTasks records the child tasks a delegated agent may follow, answer or cancel.
// An owner's client leaves Sent nil.
type SentTasks interface {
	Check(id string, target Target) error
	Reserve() (record func(string, Target), release func(), err error)
}

// mayUse refuses a hosted agent a task it did not send.
func (t *Client) mayUse(target Target, id string) error {
	if t.Sent == nil {
		return nil
	}
	return t.Sent.Check(id, target)
}

type Target struct {
	PeerID  string    `json:"peer_id,omitempty" jsonschema:"Peer ID from discover_remote_services for a remote agent; omit for an agent from list_local_services on this node."`
	Service AgentName `json:"service" jsonschema:"The agent's name from list_local_services, or srv_name from discover_remote_services"`
}

// Task is what the tools return about a task.
type Task struct {
	// Ref is where the task is, PEER/AGENT/TASK-ID (TaskRef), which the
	// task commands print and take; the tools leave it out.
	Ref    string `json:"ref,omitempty"`
	TaskID string `json:"task_id"`
	State  string `json:"state"`
	// Terminal is set once the task has ended: completed, rejected,
	// cancelled, timeout or failed.
	Terminal       string `json:"terminal,omitempty"`
	Message        string `json:"message,omitempty"`
	Result         string `json:"result,omitempty"`
	ExecutionBegun *bool  `json:"execution_begun,omitempty"`
}

// LocalInfo is what the local agent API says about the node: its peer
// ID, the names of the agents it serves, and the drains of those that take
// no new tasks.
type LocalInfo struct {
	PeerID   string              `json:"peer_id"`
	Services []AgentName         `json:"services"`
	Draining map[AgentName]Drain `json:"draining,omitempty"`
	// Runtimes names what runs each agent (codex, claude, acp).
	Runtimes map[AgentName]string `json:"runtimes,omitempty"`
}

func (t *Client) TaskClient(ctx context.Context, target Target) (*a2aclient.Client, error) {
	endpoint, client, err := t.Route(ctx, target)
	if err != nil {
		return nil, err
	}
	return a2aclient.NewFromEndpoints(ctx,
		[]*a2a.AgentInterface{a2a.NewAgentInterface(endpoint, a2a.TransportProtocolJSONRPC)},
		a2aclient.WithJSONRPCTransport(client))
}

// Route keeps card reads and task operations on the same authenticated path.
// Only that route may answer: the client it returns follows no redirect,
// since one could turn a call into a request through the node to another
// service, with this caller's credential.
func (t *Client) Route(ctx context.Context, target Target) (string, *http.Client, error) {
	endpoint, client, err := t.selectRoute(ctx, target)
	if err != nil {
		return "", nil, err
	}
	noRedirects := *client
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return endpoint, &noRedirects, nil
}

func (t *Client) selectRoute(ctx context.Context, target Target) (string, *http.Client, error) {
	if _, err := peer.Decode(target.PeerID); target.PeerID != "" && err != nil {
		return "", nil, fmt.Errorf("peer_id %q is not a peer ID", target.PeerID)
	}
	if !ValidAgentName(string(target.Service)) {
		return "", nil, fmt.Errorf("service %s is not a service name", ShownAgentName(string(target.Service)))
	}
	if t.Local != nil {
		info, err := t.LocalInfo(ctx)
		if err == nil && (target.PeerID == "" || target.PeerID == info.PeerID) {
			if !slices.Contains(info.Services, target.Service) {
				return "", nil, fmt.Errorf("this node does not host agent %q", target.Service)
			}
			if target.Service == t.Self {
				return "", nil, fmt.Errorf("agent %q is you: an agent cannot send tasks to itself", target.Service)
			}
			return fmt.Sprintf("http://agentiscaled/a2a/%s/%s", info.PeerID, target.Service), t.Local, nil
		} else if target.PeerID == "" {
			return "", nil, fmt.Errorf("local agent unavailable: %w", err)
		}
	} else if target.PeerID == "" {
		return "", nil, errors.New("no local agent API available")
	}
	return fmt.Sprintf("%s/mesh/%s/a2a/%s", t.BaseURL, target.PeerID, target.Service), t.Mesh, nil
}

func (t *Client) LocalInfo(ctx context.Context) (LocalInfo, error) {
	timeout := t.LocalInfoTimeout
	if timeout == 0 {
		timeout = DefaultLocalInfoTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agentiscaled/info", nil)
	if err != nil {
		return LocalInfo{}, err
	}
	response, err := t.Local.Do(req)
	if err != nil {
		return LocalInfo{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return LocalInfo{}, fmt.Errorf("local agent API: %s", response.Status)
	}
	const maxLocalInfoBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxLocalInfoBytes+1))
	if err != nil {
		return LocalInfo{}, fmt.Errorf("read local agent catalog: %w", err)
	}
	if len(data) > maxLocalInfoBytes {
		return LocalInfo{}, errors.New("local agent catalog exceeds 1 MiB")
	}
	// The names are parsed one by one: a name outside the rule (from a
	// node older than it) is left out on its own, so that the node's other
	// agents stay reachable. Runtimes only inform display, so they are read
	// apart: a malformed one is left out and never fails the catalog.
	var raw struct {
		PeerID   string           `json:"peer_id"`
		Services []string         `json:"services"`
		Draining map[string]Drain `json:"draining"`
		Runtimes json.RawMessage  `json:"runtimes"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return LocalInfo{}, fmt.Errorf("invalid local agent catalog: %w", err)
	}
	info := LocalInfo{PeerID: raw.PeerID, Services: ParseAgentNames(raw.Services)}
	for name, d := range raw.Draining {
		if n, err := ParseAgentName(name); err == nil {
			if info.Draining == nil {
				info.Draining = map[AgentName]Drain{}
			}
			info.Draining[n] = d
		}
	}
	for name, rt := range localRuntimes(raw.Runtimes) {
		if n, err := ParseAgentName(name); err == nil && ValidRuntime(rt) {
			if info.Runtimes == nil {
				info.Runtimes = map[AgentName]string{}
			}
			info.Runtimes[n] = rt
		}
	}
	if _, err := peer.Decode(info.PeerID); err != nil {
		return info, fmt.Errorf("invalid local peer ID: %w", err)
	}
	return info, nil
}

// localRuntimes reads the local catalog's runtimes entry by entry, leaving
// out any that is not a string, and all of them when they are not an
// object.
func localRuntimes(data json.RawMessage) map[string]string {
	var entries map[string]json.RawMessage
	if json.Unmarshal(data, &entries) != nil {
		return nil
	}
	runtimes := make(map[string]string, len(entries))
	for name, value := range entries {
		var rt string
		if json.Unmarshal(value, &rt) == nil {
			runtimes[name] = rt
		}
	}
	return runtimes
}

func (t *Client) Card(ctx context.Context, target Target) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, agentCardTimeout)
	defer cancel()
	endpoint, transport, err := t.Route(ctx, target)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/.well-known/agent-card.json", nil)
	if err != nil {
		return nil, err
	}
	response, err := transport.Do(request)
	if err != nil {
		return nil, fmt.Errorf("get agent card of %s on node %s: %w", target.Service, target.PeerID, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get agent card of %s on node %s: HTTP %d; the service is unavailable or refused", target.Service, target.PeerID, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxAgentCardBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read agent card: %w", err)
	}
	if len(data) > MaxAgentCardBytes {
		return nil, errors.New("agent card exceeds the size limit")
	}
	var card *a2a.AgentCard
	if err := json.Unmarshal(data, &card); err != nil {
		return nil, fmt.Errorf("invalid agent card: %w", err)
	}
	if card == nil || card.Name == "" || len(card.SupportedInterfaces) == 0 {
		return nil, errors.New("invalid agent card: name and supported interfaces are required")
	}
	data, err = json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("encode agent card: %w", err)
	}
	return data, nil
}

func (t *Client) Send(ctx context.Context, target Target, text, taskID string) (Task, error) {
	if strings.TrimSpace(text) == "" {
		return Task{}, errors.New("text is empty")
	}
	// A hosted agent's new task takes its place before it goes out.
	sent, drop := func(string, Target) {}, func() {}
	if taskID != "" {
		if err := t.mayUse(target, taskID); err != nil {
			return Task{}, err
		}
	} else if t.Sent != nil {
		var err error
		if sent, drop, err = t.Sent.Reserve(); err != nil {
			return Task{}, err
		}
	}
	defer drop()
	c, err := t.TaskClient(ctx, target)
	if err != nil {
		return Task{}, err
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID = a2a.TaskID(taskID)
	res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		return Task{}, unreachable(target, err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		return Task{}, fmt.Errorf("the agent answered with %T, not a task", res)
	}
	if taskID == "" {
		sent(string(task.ID), target)
	}
	return DescribeTask(task), nil
}

// Watch waits up to wait for task id to settle, passes each new step of
// its work to notify when set, and returns the task. When ctx ends first,
// it returns the task as last read, if it read it, with ctx's error.
func (t *Client) Watch(ctx context.Context, target Target, id a2a.TaskID, wait time.Duration, notify func(string)) (Task, error) {
	if err := t.mayUse(target, string(id)); err != nil {
		return Task{}, err
	}
	c, err := t.TaskClient(ctx, target)
	if err != nil {
		return Task{}, err
	}
	deadline := time.Now().Add(wait)
	// The task's history holds every step; only its status is read.
	none := 0
	var step string
	report := func(status a2a.TaskStatus) {
		// Each step is a new status message; two steps can read the same.
		if m := status.Message; notify != nil && m != nil && status.State == a2a.TaskStateWorking && m.ID != step {
			step = m.ID
			notify(MessageText(m))
		}
	}
	followed := false
	var last Task
	for {
		task, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: id, HistoryLength: &none})
		if err != nil {
			if ctx.Err() != nil {
				return last, ctx.Err()
			}
			if errors.Is(err, a2a.ErrTaskNotFound) {
				return Task{}, taskNotFound(target, string(id))
			}
			return Task{}, unreachable(target, err)
		}
		if settled(task.Status.State) || !time.Now().Before(deadline) {
			return DescribeTask(task), nil
		}
		last = DescribeTask(task)
		report(task.Status)
		if !followed {
			// The agent sends the task's events as they happen, until the
			// task settles. Should the stream fail or stop early, watch
			// reads the task again every second.
			followed = true
			t.follow(ctx, c, task.ID, deadline, report)
			continue
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// settled is a state the task stays in until the caller acts, if ever.
func settled(s a2a.TaskState) bool {
	return s.Terminal() || s == a2a.TaskStateInputRequired
}

// DefaultStreamCheckEvery is how often follow reads the task while it streams, so
// that a stream that stalls, as one through a buffering proxy can, does not
// hold get_task after the task settled.
const DefaultStreamCheckEvery = 5 * time.Second

// follow streams the events of task id until it settles, the stream ends or
// fails, or the deadline passes, and reports each status it carries.
func (t *Client) follow(ctx context.Context, c *a2aclient.Client, id a2a.TaskID, deadline time.Time, report func(a2a.TaskStatus)) {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	// The check below ends with follow, not after it.
	checked := make(chan struct{})
	defer func() { cancel(); <-checked }()
	every := t.StreamCheckEvery
	if every == 0 {
		every = DefaultStreamCheckEvery
	}
	go func() {
		defer close(checked)
		none := 0
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if task, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: id, HistoryLength: &none}); err == nil && settled(task.Status.State) {
					cancel()
					return
				}
			}
		}
	}()
	for ev, err := range c.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: id}) {
		if err != nil {
			return
		}
		var status a2a.TaskStatus
		switch e := ev.(type) {
		case *a2a.Task:
			status = e.Status
		case *a2a.TaskStatusUpdateEvent:
			status = e.Status
		default:
			continue
		}
		if settled(status.State) {
			return
		}
		report(status)
	}
}

func (t *Client) Cancel(ctx context.Context, target Target, id string) (Task, error) {
	if err := t.mayUse(target, id); err != nil {
		return Task{}, err
	}
	c, err := t.TaskClient(ctx, target)
	if err != nil {
		return Task{}, err
	}
	task, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(id)})
	if err != nil {
		if errors.Is(err, a2a.ErrTaskNotFound) {
			return Task{}, taskNotFound(target, id)
		}
		return Task{}, unreachable(target, err)
	}
	return DescribeTask(task), nil
}

// taskNotFound reports a task the agent does not show the caller.
func taskNotFound(target Target, id string) error {
	return fmt.Errorf("task %s not found at %s: an agent shows a task only to whoever sent it (%w)", id, target.Service, a2a.ErrTaskNotFound)
}

// unreachable reports a call that did not get an answer from the agent. The
// mesh keeps no mailbox, so nothing will deliver it later.
func unreachable(target Target, err error) error {
	return fmt.Errorf("failed: no answer from %s on node %s (%v). If that node is offline the task cannot be delivered or followed; nothing is queued", target.Service, target.PeerID, err)
}

func DescribeTask(task *a2a.Task) Task {
	out := Task{TaskID: string(task.ID), State: StateName(task.Status.State)}
	if task.Status.State.Terminal() {
		out.Terminal = out.State
		if v, ok := task.Metadata[MetaTerminal].(string); ok {
			out.Terminal = v
		}
	}
	if m := task.Status.Message; m != nil {
		out.Message = MessageText(m)
	}
	var results []string
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			if text := p.Text(); text != "" {
				results = append(results, text)
			}
		}
	}
	out.Result = strings.Join(results, "\n")
	if v, ok := task.Metadata[MetaExecutionBegun].(bool); ok {
		out.ExecutionBegun = &v
	}
	return out
}

func StateName(s a2a.TaskState) string {
	switch s {
	case a2a.TaskStateCanceled:
		return "cancelled"
	default:
		return strings.ToLower(strings.TrimPrefix(string(s), "TASK_STATE_"))
	}
}

func MessageText(m *a2a.Message) string {
	var parts []string
	for _, p := range m.Parts {
		if t := p.Text(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}
