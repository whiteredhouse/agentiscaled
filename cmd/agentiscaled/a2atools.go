package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const a2aInstructions = `agentiscale: use list_local_services (type "a2a") to find agents hosted on this node; read their card with get_agent_card and call send_task with their name as service, omitting peer_id in both calls. Use discover_remote_services (type "a2a") to find agents on other nodes; inspect their current card with get_agent_card using peer_id and srv_name as service, then call send_task with the same target. Follow tasks with get_task until terminal (completed, rejected, cancelled, timeout or failed); while a task is working, its message is the agent's latest step. cancel_task stops them. A task in state input_required waits for your answer to the agent's question (its message): answer with send_task, giving the task's task_id and your answer as text, then follow the task again. Use the same target for send/get/cancel. Do not use the a2a CLI or hand-written requests. An offline remote node cannot be reached and nothing is queued.`

// maxWait bounds how long get_task holds one tool call, below the timeouts
// MCP clients put on a call.
const maxWait = 50 * time.Second

// Match the node's remote-card ingestion budget, with a separate call deadline.
const maxAgentCardBytes = 1 << 20
const agentCardTimeout = 30 * time.Second

var serviceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// a2aTasks selects agentiscaled's owner socket for local tasks and the
// unchanged Agent Mesh proxy for tasks sent to other nodes.
type a2aTasks struct {
	http  *http.Client
	base  string       // the node's local API
	local *http.Client // owner-only agent socket; nil for older nodes
	// depth is the delegation depth of the tasks sent: 0 for a person's
	// client, one more than its own task's for a hosted agent (taskNetwork).
	depth int
	// self is the hosted agent these tools serve, which may not send tasks
	// to itself; empty for a person's client.
	self string
}

type taskTarget struct {
	PeerID  string `json:"peer_id,omitempty" jsonschema:"Peer ID from discover_remote_services for a remote agent; omit for an agent from list_local_services on this node."`
	Service string `json:"service" jsonschema:"The agent's name from list_local_services, or srv_name from discover_remote_services"`
}

type sendTaskIn struct {
	taskTarget
	Text string `json:"text" jsonschema:"the request for the agent: what you want it to do; or, with task_id, your answer to its question"`
	// TaskID answers the question of a task in state input_required.
	TaskID string `json:"task_id,omitempty" jsonschema:"only to answer the question of a task in state input_required: that task's ID. Omit to send a new task."`
}

type taskIn struct {
	taskTarget
	TaskID string `json:"task_id" jsonschema:"the task ID send_task returned"`
}

type getTaskIn struct {
	taskIn
	WaitSeconds int `json:"wait_seconds,omitempty" jsonschema:"wait up to this many seconds (at most 50) for the task to end or ask a question; 0 answers at once"`
}

// taskOut is what the tools return about a task.
type taskOut struct {
	TaskID string `json:"task_id"`
	State  string `json:"state"`
	// Terminal is set once the task has ended: completed, rejected,
	// cancelled, timeout or failed.
	Terminal       string `json:"terminal,omitempty"`
	Message        string `json:"message,omitempty"`
	Result         string `json:"result,omitempty"`
	ExecutionBegun *bool  `json:"execution_begun,omitempty"`
}

func addA2ATools(server *mcp.Server, t *a2aTasks) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_agent_card",
		Description: "Read the current card of an A2A agent. For a local agent use its service name and omit peer_id; for a remote agent use its discovered peer_id and service name (srv_name). Remote reads use the encrypted mesh connection. An unavailable or refused service returns an error; this does not send a task.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.card)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "send_task",
		Description: "Send one task to an A2A agent on this node or another node of your agentiscale network, or with task_id answer the question of a task in state input_required. Remote calls use the network's end-to-end encrypted connection. Returns at once with the task ID and state; follow it with get_task.",
	}, t.send)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_task",
		Description: "Get the state and result of a task sent with send_task, optionally waiting for it to end or to ask you a question (state input_required, the question in message). While the task is working, message is the agent's latest step, such as the program it runs or the file it edits.",
	}, t.get)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_task",
		Description: "Cancel a task sent with send_task. The answer says whether the agent had already started on it.",
	}, t.cancel)
}

func (t *a2aTasks) client(ctx context.Context, target taskTarget) (*a2aclient.Client, error) {
	endpoint, client, err := t.route(ctx, target)
	if err != nil {
		return nil, err
	}
	return a2aclient.NewFromEndpoints(ctx,
		[]*a2a.AgentInterface{a2a.NewAgentInterface(endpoint, a2a.TransportProtocolJSONRPC)},
		a2aclient.WithJSONRPCTransport(client))
}

// route keeps card reads and task operations on the same authenticated path.
func (t *a2aTasks) route(ctx context.Context, target taskTarget) (string, *http.Client, error) {
	if _, err := peer.Decode(target.PeerID); target.PeerID != "" && err != nil {
		return "", nil, fmt.Errorf("peer_id %q is not a peer ID", target.PeerID)
	}
	if !serviceName.MatchString(target.Service) {
		return "", nil, fmt.Errorf("service %q is not a service name", target.Service)
	}
	if t.local != nil {
		info, err := t.localInfo(ctx)
		if err == nil && (target.PeerID == "" || target.PeerID == info.PeerID) {
			if !slices.Contains(info.Services, target.Service) {
				return "", nil, fmt.Errorf("this node does not host agent %q", target.Service)
			}
			if target.Service == t.self {
				return "", nil, fmt.Errorf("agent %q is you: an agent cannot send tasks to itself", target.Service)
			}
			return fmt.Sprintf("http://agentiscaled/a2a/%s/%s", info.PeerID, target.Service), t.local, nil
		} else if target.PeerID == "" {
			return "", nil, fmt.Errorf("local agent unavailable: %w", err)
		}
	} else if target.PeerID == "" {
		return "", nil, errors.New("no local agent API available")
	}
	return fmt.Sprintf("%s/mesh/%s/a2a/%s", t.base, target.PeerID, target.Service), t.http, nil
}

// localInfoTimeout bounds the local agent lookup that precedes every call,
// so a stuck local socket cannot hold up a call meant for another node.
var localInfoTimeout = 2 * time.Second

func (t *a2aTasks) localInfo(ctx context.Context) (localAgentInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, localInfoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agentiscaled/info", nil)
	if err != nil {
		return localAgentInfo{}, err
	}
	response, err := t.local.Do(req)
	if err != nil {
		return localAgentInfo{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return localAgentInfo{}, fmt.Errorf("local agent API: %s", response.Status)
	}
	var info localAgentInfo
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&info); err != nil {
		return info, err
	}
	if _, err := peer.Decode(info.PeerID); err != nil {
		return info, fmt.Errorf("invalid local peer ID: %w", err)
	}
	for _, name := range info.Services {
		if !serviceName.MatchString(name) {
			return info, errors.New("invalid local service name")
		}
	}
	return info, nil
}

func (t *a2aTasks) card(ctx context.Context, _ *mcp.CallToolRequest, target taskTarget) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, agentCardTimeout)
	defer cancel()
	endpoint, transport, err := t.route(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/.well-known/agent-card.json", nil)
	if err != nil {
		return nil, nil, err
	}
	// Only the selected local/mesh route may answer. A redirect must not
	// become a request to a different route, host or advertised interface.
	client := *transport
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("get agent card of %s on node %s: %w", target.Service, target.PeerID, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("get agent card of %s on node %s: HTTP %d; the service is unavailable or refused", target.Service, target.PeerID, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAgentCardBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read agent card: %w", err)
	}
	if len(data) > maxAgentCardBytes {
		return nil, nil, errors.New("agent card exceeds the size limit")
	}
	var card *a2a.AgentCard
	if err := json.Unmarshal(data, &card); err != nil {
		return nil, nil, fmt.Errorf("invalid agent card: %w", err)
	}
	if card == nil || card.Name == "" || len(card.SupportedInterfaces) == 0 {
		return nil, nil, errors.New("invalid agent card: name and supported interfaces are required")
	}
	data, err = json.Marshal(card)
	if err != nil {
		return nil, nil, fmt.Errorf("encode agent card: %w", err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
}

func (t *a2aTasks) send(ctx context.Context, _ *mcp.CallToolRequest, in sendTaskIn) (*mcp.CallToolResult, taskOut, error) {
	if strings.TrimSpace(in.Text) == "" {
		return nil, taskOut{}, errors.New("text is empty")
	}
	c, err := t.client(ctx, in.taskTarget)
	if err != nil {
		return nil, taskOut{}, err
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(in.Text))
	msg.TaskID = a2a.TaskID(in.TaskID)
	// A person's client sends at depth 0, a hosted agent one deeper than
	// its own task.
	msg.SetMeta(metaDelegationDepth, t.depth)
	res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		return nil, taskOut{}, unreachable(in.taskTarget, err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		return nil, taskOut{}, fmt.Errorf("the agent answered with %T, not a task", res)
	}
	return toolResult(describeTask(task))
}

// get reads a task, waiting as asked. While it waits, each new step of
// the agent goes to the client as a progress notification, for clients that
// asked for them (a progress token) and show them.
func (t *a2aTasks) get(ctx context.Context, req *mcp.CallToolRequest, in getTaskIn) (*mcp.CallToolResult, taskOut, error) {
	var notify func(string)
	if req != nil && req.Session != nil && req.Params != nil {
		if token := req.Params.GetProgressToken(); token != nil {
			n := 0
			notify = func(step string) {
				n++
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: float64(n), Message: step})
			}
		}
	}
	wait := min(time.Duration(max(in.WaitSeconds, 0))*time.Second, maxWait)
	out, err := t.watch(ctx, in.taskTarget, a2a.TaskID(in.TaskID), wait, notify)
	if err != nil {
		return nil, taskOut{}, err
	}
	return toolResult(out)
}

// watch waits up to wait for task id to settle, passes each new step of
// its work to notify when set, and returns the task.
func (t *a2aTasks) watch(ctx context.Context, target taskTarget, id a2a.TaskID, wait time.Duration, notify func(string)) (taskOut, error) {
	c, err := t.client(ctx, target)
	if err != nil {
		return taskOut{}, err
	}
	deadline := time.Now().Add(wait)
	// The task's history holds every step; only its status is read.
	none := 0
	var step string
	report := func(status a2a.TaskStatus) {
		// Each step is a new status message; two steps can read the same.
		if m := status.Message; notify != nil && m != nil && status.State == a2a.TaskStateWorking && m.ID != step {
			step = m.ID
			notify(messageText(m))
		}
	}
	followed := false
	for {
		task, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: id, HistoryLength: &none})
		if err != nil {
			if ctx.Err() != nil {
				return taskOut{}, ctx.Err()
			}
			return taskOut{}, unreachable(target, err)
		}
		if settled(task.Status.State) || !time.Now().Before(deadline) {
			return describeTask(task), nil
		}
		report(task.Status)
		if !followed {
			// The agent sends the task's events as they happen, until the
			// task settles. Should the stream fail or stop early, watch
			// reads the task again every second.
			followed = true
			follow(ctx, c, task.ID, deadline, report)
			continue
		}
		select {
		case <-ctx.Done():
			return taskOut{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// settled is a state the task stays in until the caller acts, if ever.
func settled(s a2a.TaskState) bool {
	return s.Terminal() || s == a2a.TaskStateInputRequired
}

// streamCheckEvery is how often follow reads the task while it streams, so
// that a stream that stalls, as one through a buffering proxy can, does not
// hold get_task after the task settled.
var streamCheckEvery = 5 * time.Second

// follow streams the events of task id until it settles, the stream ends or
// fails, or the deadline passes, and reports each status it carries.
func follow(ctx context.Context, c *a2aclient.Client, id a2a.TaskID, deadline time.Time, report func(a2a.TaskStatus)) {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	go func() {
		none := 0
		t := time.NewTicker(streamCheckEvery)
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

func (t *a2aTasks) cancel(ctx context.Context, _ *mcp.CallToolRequest, in taskIn) (*mcp.CallToolResult, taskOut, error) {
	c, err := t.client(ctx, in.taskTarget)
	if err != nil {
		return nil, taskOut{}, err
	}
	task, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(in.TaskID)})
	if err != nil {
		return nil, taskOut{}, unreachable(in.taskTarget, err)
	}
	return toolResult(describeTask(task))
}

// unreachable reports a call that did not get an answer from the agent. The
// mesh keeps no mailbox, so nothing will deliver it later.
func unreachable(target taskTarget, err error) error {
	return fmt.Errorf("failed: no answer from %s on node %s (%v). If that node is offline the task cannot be delivered or followed; nothing is queued", target.Service, target.PeerID, err)
}

func describeTask(task *a2a.Task) taskOut {
	out := taskOut{TaskID: string(task.ID), State: stateName(task.Status.State)}
	if task.Status.State.Terminal() {
		out.Terminal = out.State
		if v, ok := task.Metadata[metaTerminal].(string); ok {
			out.Terminal = v
		}
	}
	if m := task.Status.Message; m != nil {
		out.Message = messageText(m)
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
	if v, ok := task.Metadata[metaExecutionBegun].(bool); ok {
		out.ExecutionBegun = &v
	}
	return out
}

func stateName(s a2a.TaskState) string {
	switch s {
	case a2a.TaskStateCanceled:
		return "cancelled"
	default:
		return strings.ToLower(strings.TrimPrefix(string(s), "TASK_STATE_"))
	}
}

func toolResult(out taskOut) (*mcp.CallToolResult, taskOut, error) {
	b, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, out, nil
}
