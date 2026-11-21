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
	"github.com/google/agentmesh/api"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const a2aInstructions = `Use list_agents to discover agents in your network, optionally filtering by location (local, mesh, or cloud). Use the returned service and peer_id for get_agent_card, send_task, get_task, and cancel_task. Omit peer_id for a local agent. Cloud agents can accept tasks while their workers sleep. Follow a submitted task with get_task until it finishes or asks for input. If its state is input_required, send_task with the same target, task_id, and your answer as text.`

// maxWait bounds how long get_task holds one tool call, below the timeouts
// MCP clients put on a call.
const maxWait = 50 * time.Second

// Match the node's remote-card ingestion budget, with a separate call deadline.
const maxAgentCardBytes = 1 << 20
const agentCardTimeout = 30 * time.Second

var serviceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// a2aTasks uses the owner socket for local and cloud agents and the mesh
// proxy for tasks sent to a remote peer.
type a2aTasks struct {
	http  *http.Client
	base  string       // the node's local API
	local *http.Client // owner-only agent socket; nil for older nodes
	// Granted tasks may enumerate A2A service metadata through the node.
	// Their A2A-only credential cannot invoke the mesh's system catalog.
	meshCatalog func(context.Context) ([]*api.DiscoveredProvider, error)
	// self is the hosted agent these tools serve, which may not send tasks
	// to itself; empty for a person's client.
	self string
	// sent, for a hosted agent, holds the tasks its task has sent, the only
	// ones it may follow, answer or cancel; nil for a person's client.
	sent *sentTasks
}

// mayUse refuses a hosted agent a task it did not send.
func (t *a2aTasks) mayUse(target taskTarget, id string) error {
	if t.sent == nil {
		return nil
	}
	return t.sent.check(id, target)
}

type taskTarget struct {
	PeerID  string `json:"peer_id,omitempty" jsonschema:"Target from list_agents. Use the mesh peer ID or cloud:UUID returned for the agent. Omit for a local agent."`
	Service string `json:"service" jsonschema:"The service name from list_agents"`
}

type sendTaskIn struct {
	taskTarget
	Text      string `json:"text" jsonschema:"the request for the agent: what you want it to do; or, with task_id, your answer to its question"`
	RequestID string `json:"request_id,omitempty" jsonschema:"Stable UUID for retrying a cloud submission or follow-up after an uncertain response. Omit to generate one."`
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
		Name:        "list_agents",
		Description: "List agents visible to this caller. Filter by location to list agents on this node (local), on other nodes (mesh), or in the cloud (cloud). Use the service and peer_id from a result with the card and task tools.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.listAgents)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_agent_card",
		Description: "Read the current card of an A2A agent. Use the service and peer_id from list_agents. Omit peer_id for a local agent. A remote card comes from the mesh peer or the authorized cloud catalog.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.card)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "send_task",
		Description: "Send one task to an A2A agent in your network, or with task_id answer the question of a task in state input_required. Peer calls use the encrypted mesh. Cloud tasks are durably accepted by the control service over HTTPS. Returns at once with the task ID and state; follow it with get_task.",
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
// Only that route may answer: the client it returns follows no redirect,
// since one could turn a call into a request through the node to another
// service, with this caller's credential.
func (t *a2aTasks) route(ctx context.Context, target taskTarget) (string, *http.Client, error) {
	endpoint, client, err := t.selectRoute(ctx, target)
	if err != nil {
		return "", nil, err
	}
	noRedirects := *client
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return endpoint, &noRedirects, nil
}

func (t *a2aTasks) selectRoute(ctx context.Context, target taskTarget) (string, *http.Client, error) {
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
	if strings.HasPrefix(target.PeerID, cloudTargetPrefix) {
		return t.hostedCard(ctx, target)
	}
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
	response, err := transport.Do(request)
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
	// A hosted agent's new task takes its place before it goes out.
	sent, drop := func(string, taskTarget) {}, func() {}
	if in.TaskID != "" {
		if err := t.mayUse(in.taskTarget, in.TaskID); err != nil {
			return nil, taskOut{}, err
		}
	} else if t.sent != nil {
		var err error
		if sent, drop, err = t.sent.reserve(); err != nil {
			return nil, taskOut{}, err
		}
	}
	defer drop()
	if strings.HasPrefix(in.PeerID, cloudTargetPrefix) {
		if in.TaskID == "" {
			if _, err := t.hostedAgent(ctx, in.taskTarget); err != nil {
				return nil, taskOut{}, err
			}
		}
		id := in.RequestID
		if id == "" {
			id = uuid.NewString()
		}
		result, err := t.hostedTask(ctx, in.taskTarget, "", http.MethodPost, false, &hostedSubmission{RequestID: id, TaskID: in.TaskID, Text: in.Text})
		if err != nil {
			return nil, taskOut{}, err
		}
		if in.TaskID == "" {
			sent(result.TaskID, in.taskTarget)
		}
		return toolResult(result)
	}
	if in.RequestID != "" {
		return nil, taskOut{}, errors.New("request_id applies only to hosted agents")
	}
	c, err := t.client(ctx, in.taskTarget)
	if err != nil {
		return nil, taskOut{}, err
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(in.Text))
	msg.TaskID = a2a.TaskID(in.TaskID)
	res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		return nil, taskOut{}, unreachable(in.taskTarget, err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		return nil, taskOut{}, fmt.Errorf("the agent answered with %T, not a task", res)
	}
	if in.TaskID == "" {
		sent(string(task.ID), in.taskTarget)
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
	if err := t.mayUse(in.taskTarget, in.TaskID); err != nil {
		return nil, taskOut{}, err
	}
	wait := min(time.Duration(max(in.WaitSeconds, 0))*time.Second, maxWait)
	out, err := t.watch(ctx, in.taskTarget, a2a.TaskID(in.TaskID), wait, notify)
	if err != nil {
		return nil, taskOut{}, err
	}
	return toolResult(out)
}

// watch waits up to wait for task id to settle, passes each new step of
// its work to notify when set, and returns the task. When ctx ends first,
// it returns the task as last read, if it read it, with ctx's error.
func (t *a2aTasks) watch(ctx context.Context, target taskTarget, id a2a.TaskID, wait time.Duration, notify func(string)) (taskOut, error) {
	if strings.HasPrefix(target.PeerID, cloudTargetPrefix) {
		return t.watchHosted(ctx, target, string(id), wait, notify)
	}
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
	var last taskOut
	for {
		task, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: id, HistoryLength: &none})
		if err != nil {
			if ctx.Err() != nil {
				return last, ctx.Err()
			}
			if errors.Is(err, a2a.ErrTaskNotFound) {
				return taskOut{}, taskNotFound(target, string(id))
			}
			return taskOut{}, unreachable(target, err)
		}
		if settled(task.Status.State) || !time.Now().Before(deadline) {
			return describeTask(task), nil
		}
		last = describeTask(task)
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
			return last, ctx.Err()
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
	// The check below ends with follow, not after it.
	checked := make(chan struct{})
	defer func() { cancel(); <-checked }()
	every := streamCheckEvery
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

func (t *a2aTasks) cancel(ctx context.Context, _ *mcp.CallToolRequest, in taskIn) (*mcp.CallToolResult, taskOut, error) {
	if err := t.mayUse(in.taskTarget, in.TaskID); err != nil {
		return nil, taskOut{}, err
	}
	if strings.HasPrefix(in.PeerID, cloudTargetPrefix) {
		out, err := t.hostedTask(ctx, in.taskTarget, in.TaskID, http.MethodPost, true, nil)
		if err != nil {
			return nil, taskOut{}, err
		}
		if out.State != "cancelled" {
			return nil, taskOut{}, errors.New("hosted task has already ended")
		}
		return toolResult(out)
	}
	c, err := t.client(ctx, in.taskTarget)
	if err != nil {
		return nil, taskOut{}, err
	}
	task, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(in.TaskID)})
	if err != nil {
		if errors.Is(err, a2a.ErrTaskNotFound) {
			return nil, taskOut{}, taskNotFound(in.taskTarget, in.TaskID)
		}
		return nil, taskOut{}, unreachable(in.taskTarget, err)
	}
	return toolResult(describeTask(task))
}

// taskNotFound reports a task the agent does not show the caller.
func taskNotFound(target taskTarget, id string) error {
	return fmt.Errorf("task %s not found at %s: an agent shows a task only to whoever sent it (%w)", id, target.Service, a2a.ErrTaskNotFound)
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
