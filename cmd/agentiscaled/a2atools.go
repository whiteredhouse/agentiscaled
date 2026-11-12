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

const a2aInstructions = `agentiscale: use list_local_services (type "a2a") to find agents hosted on this node; read their card with get_agent_card and call send_task with their name as service, omitting peer_id in both calls. Use discover_remote_services (type "a2a") to find agents on other nodes; inspect their current card with get_agent_card using peer_id and srv_name as service, then call send_task with the same target. Follow tasks with get_task until terminal (completed, rejected, cancelled, timeout or failed); cancel_task stops them. Use the same target for send/get/cancel. Do not use the a2a CLI or hand-written requests. An offline remote node cannot be reached and nothing is queued.`

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
}

type taskTarget struct {
	PeerID  string `json:"peer_id,omitempty" jsonschema:"Peer ID from discover_remote_services for a remote agent; omit for an agent from list_local_services on this node."`
	Service string `json:"service" jsonschema:"The agent's name from list_local_services, or srv_name from discover_remote_services"`
}

type sendTaskIn struct {
	taskTarget
	Text string `json:"text" jsonschema:"the request for the agent: what you want it to do"`
}

type taskIn struct {
	taskTarget
	TaskID string `json:"task_id" jsonschema:"the task ID send_task returned"`
}

type getTaskIn struct {
	taskIn
	WaitSeconds int `json:"wait_seconds,omitempty" jsonschema:"wait up to this many seconds (at most 50) for the task to end; 0 answers at once"`
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
		Description: "Send one task to an A2A agent on this node or another node of your agentiscale network. Remote calls use the network's end-to-end encrypted connection. Returns at once with the task ID and state; follow it with get_task.",
	}, t.send)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_task",
		Description: "Get the state and result of a task sent with send_task, optionally waiting for it to end.",
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
	// The sender is a person's agent, not a delegated one.
	msg.SetMeta(metaDelegationDepth, 0)
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

func (t *a2aTasks) get(ctx context.Context, _ *mcp.CallToolRequest, in getTaskIn) (*mcp.CallToolResult, taskOut, error) {
	c, err := t.client(ctx, in.taskTarget)
	if err != nil {
		return nil, taskOut{}, err
	}
	wait := min(time.Duration(max(in.WaitSeconds, 0))*time.Second, maxWait)
	deadline := time.Now().Add(wait)
	for {
		task, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(in.TaskID)})
		if err != nil {
			return nil, taskOut{}, unreachable(in.taskTarget, err)
		}
		if task.Status.State.Terminal() || !time.Now().Before(deadline) {
			return toolResult(describeTask(task))
		}
		select {
		case <-ctx.Done():
			return nil, taskOut{}, ctx.Err()
		case <-time.After(time.Second):
		}
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
