package caller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/whiteredhouse/agentiscaled/internal/nodeclient"
)

const Instructions = `agentiscale: use list_local_services (type "a2a") to find agents hosted on this node; read their card with get_agent_card and call send_task with their name as service, omitting peer_id in both calls. Use discover_remote_services (type "a2a") to find agents on other nodes; inspect their current card with get_agent_card using peer_id and srv_name as service, then call send_task with the same target. Follow tasks with get_task until terminal (completed, rejected, cancelled, timeout or failed); while a task is working, its message is the agent's latest step. cancel_task stops them. A task in state input_required waits for your answer to the agent's question (its message): answer with send_task, giving the task's task_id and your answer as text, then follow the task again. Use the same target for send/get/cancel. Do not use the a2a CLI or hand-written requests. An offline remote node cannot be reached and nothing is queued.`

// maxWait bounds how long get_task holds one tool call, below the timeouts
// MCP clients put on a call.
const maxWait = 50 * time.Second

type SendInput struct {
	Target
	Text string `json:"text" jsonschema:"the request for the agent: what you want it to do; or, with task_id, your answer to its question"`
	// TaskID answers the question of a task in state input_required.
	TaskID string `json:"task_id,omitempty" jsonschema:"only to answer the question of a task in state input_required: that task's ID. Omit to send a new task."`
}

type TaskInput struct {
	Target
	TaskID string `json:"task_id" jsonschema:"the task ID send_task returned"`
}

type GetInput struct {
	TaskInput
	WaitSeconds int `json:"wait_seconds,omitempty" jsonschema:"wait up to this many seconds (at most 50) for the task to end or ask a question; 0 answers at once"`
}

func AddTools(server *mcp.Server, t *Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_agent_card",
		Description: "Read the current card of an A2A agent. For a local agent use its service name and omit peer_id; for a remote agent use its discovered peer_id and service name (srv_name). Remote reads use the encrypted mesh connection. An unavailable or refused service returns an error; this does not send a task.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.CardTool)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "send_task",
		Description: "Send one task to an A2A agent on this node or another node of your agentiscale network, or with task_id answer the question of a task in state input_required. Remote calls use the network's end-to-end encrypted connection. Returns at once with the task ID and state; follow it with get_task.",
	}, t.SendTool)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_task",
		Description: "Get the state and result of a task sent with send_task, optionally waiting for it to end or to ask you a question (state input_required, the question in message). While the task is working, message is the agent's latest step, such as the program it runs or the file it edits.",
	}, t.GetTool)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_task",
		Description: "Cancel a task sent with send_task. The answer says whether the agent had already started on it.",
	}, t.CancelTool)
}

func (t *Client) CardTool(ctx context.Context, _ *mcp.CallToolRequest, target Target) (*mcp.CallToolResult, any, error) {
	data, err := t.Card(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
}

func (t *Client) SendTool(ctx context.Context, _ *mcp.CallToolRequest, in SendInput) (*mcp.CallToolResult, Task, error) {
	return toolResult(t.Send(ctx, in.Target, in.Text, in.TaskID))
}

func (t *Client) CancelTool(ctx context.Context, _ *mcp.CallToolRequest, in TaskInput) (*mcp.CallToolResult, Task, error) {
	return toolResult(t.Cancel(ctx, in.Target, in.TaskID))
}

// get reads a task, waiting as asked. While it waits, each new step of
// the agent goes to the client as a progress notification, for clients that
// asked for them (a progress token) and show them.
func (t *Client) GetTool(ctx context.Context, req *mcp.CallToolRequest, in GetInput) (*mcp.CallToolResult, Task, error) {
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
	return toolResult(t.Watch(ctx, in.Target, a2a.TaskID(in.TaskID), wait, notify))
}

func toolResult(out Task, err error) (*mcp.CallToolResult, Task, error) {
	if err != nil {
		return nil, Task{}, err
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, out, nil
}

// NodeA2AGuide starts the paragraph of the node's instructions that sends
// clients to the a2a CLI for A2A agents; the bridge's send_task replaces it.
const NodeA2AGuide = "A2A agents ('a2a://...')"

// WithoutA2AGuide drops that paragraph from the node's instructions, so that a
// client is not told both to install the a2a CLI and not to use it.
func WithoutA2AGuide(instructions string) string {
	paras := strings.Split(instructions, "\n\n")
	kept := paras[:0]
	for _, p := range paras {
		if !strings.HasPrefix(p, NodeA2AGuide) {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

// BridgeMCP serves an MCP server on the client transport that offers every
// tool of the node's MCP server, reached over Streamable HTTP on its Unix
// socket, plus the A2A task tools, until the client closes.
func BridgeMCP(ctx context.Context, client mcp.Transport, socket string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpClient := nodeclient.SocketClient(socket)
	// The host is never dialled; the socket is.
	node, err := mcp.NewClient(&mcp.Implementation{Name: "agentiscaled-mcp", Version: "0.1.0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://agentiscaled/mcp", HTTPClient: httpClient}, nil)
	if err != nil {
		return fmt.Errorf("connect to the node: %w", err)
	}
	defer node.Close()

	instructions := Instructions
	if init := node.InitializeResult(); init != nil {
		instructions = WithoutA2AGuide(init.Instructions) + "\n\n" + instructions
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "agentiscale", Version: "0.1.0"},
		&mcp.ServerOptions{Instructions: instructions})
	var cursor string
	for {
		page, err := node.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return fmt.Errorf("list the node's tools: %w", err)
		}
		for _, tool := range page.Tools {
			name := tool.Name
			server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return node.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: req.Params.Arguments})
			})
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	AddTools(server, &Client{Mesh: httpClient, BaseURL: "http://agentiscaled", Local: nodeclient.SocketClient(filepath.Join(filepath.Dir(socket), nodeclient.AgentSocketName))})

	// The client closing its end is the normal way to stop.
	if err := server.Run(ctx, client); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, mcp.ErrConnectionClosed) && ctx.Err() == nil {
		return err
	}
	return nil
}
