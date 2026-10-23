package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const a2aInstructions = `agentiscale: to use an A2A agent on another node of your network, do not use the a2a CLI or hand-written requests. Find it with discover_remote_services (type "a2a"), then call send_task with its peer_id and srv_name, and follow the task with get_task until it reaches a terminal state (completed, rejected, cancelled, timeout or failed). cancel_task stops it. A node that is offline cannot be reached: there is no mailbox, so the call fails and you should tell the user.`

// maxWait bounds how long get_task holds one tool call, below the timeouts
// MCP clients put on a call.
const maxWait = 50 * time.Second

var serviceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// a2aTasks sends A2A tasks through this node's mesh proxy, which carries
// them to the receiving node over the network's encrypted connection.
type a2aTasks struct {
	http *http.Client
	base string // the node's local API
}

type taskTarget struct {
	PeerID  string `json:"peer_id" jsonschema:"peer ID of the node that serves the agent, from discover_remote_services"`
	Service string `json:"service" jsonschema:"the agent's service name (srv_name), as discovery lists it"`
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
		Name:        "send_task",
		Description: "Send one task to an A2A agent on another node of your agentiscale network, over its end-to-end encrypted connection. Returns at once with the task ID and state; follow it with get_task.",
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
	if _, err := peer.Decode(target.PeerID); err != nil {
		return nil, fmt.Errorf("peer_id %q is not a peer ID", target.PeerID)
	}
	if !serviceName.MatchString(target.Service) {
		return nil, fmt.Errorf("service %q is not a service name", target.Service)
	}
	url := fmt.Sprintf("%s/mesh/%s/a2a/%s", t.base, target.PeerID, target.Service)
	return a2aclient.NewFromEndpoints(ctx,
		[]*a2a.AgentInterface{a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)},
		a2aclient.WithJSONRPCTransport(t.http))
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
