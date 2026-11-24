package caller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/whiteredhouse/agentiscaled/internal/cli"
	"github.com/whiteredhouse/agentiscaled/internal/nodeclient"
	"github.com/whiteredhouse/agentiscaled/internal/nodestate"
)

const toolUsageLines = `       agentiscaled network tool find [flags] [NAME]
       agentiscaled network tool describe [flags] TOOL
       agentiscaled network tool call [flags] TOOL [ARGUMENTS|-]`

var toolUsage = "usage: " + strings.TrimLeft(toolUsageLines, " ")

const (
	toolFindUsage = `usage: agentiscaled network tool find [flags] [NAME]
Find the MCP tools that other nodes of your network offer. NAME narrows the
search to tools with that exact name, such as review_pr.
`
	toolDescribeUsage = `usage: agentiscaled network tool describe [flags] TOOL
Print a remote MCP tool's description and input and output schemas as JSON.
TOOL is a name from network tool find, such as mcp://code-reviewer/review_pr;
code-reviewer/review_pr is short for it.
`
	toolCallUsage = `usage: agentiscaled network tool call [flags] TOOL [ARGUMENTS|-]
Call a remote MCP tool and print its result. ARGUMENTS is a JSON object whose
keys match the tool's input schema (see network tool describe); - reads it from
standard input, and no ARGUMENTS calls the tool without any.
`
)

// The node's MCP tools for the MCP tools of other nodes.
const (
	findRemoteTools    = "find_remote_tools"
	describeRemoteTool = "describe_remote_tool"
	callRemoteTool     = "call_remote_tool"
)

// RemoteTool is one row of find_remote_tools: a tool on a node, or, with
// Error set, a node whose tools could not be read.
type RemoteTool struct {
	PeerID      string            `json:"peer_id"`
	ToolName    string            `json:"tool_name,omitempty"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// RemoteToolList is what network tool find prints with -json.
type RemoteToolList struct {
	Tools []RemoteTool `json:"tools"`
}

// RunTool is `agentiscaled network tool`: it finds, describes and calls the MCP
// tools of other nodes through the node's own MCP server.
func RunTool(ctx context.Context, args []string, stdin io.Reader, out, errOut io.Writer) error {
	if len(args) == 0 || !slices.Contains([]string{"find", "describe", "call"}, args[0]) {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			fmt.Fprintln(errOut, toolUsage)
			return nil
		}
		return errors.New(toolUsage)
	}
	cmd := args[0]
	fs := flag.NewFlagSet("network tool "+cmd, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprint(errOut, map[string]string{"find": toolFindUsage, "describe": toolDescribeUsage, "call": toolCallUsage}[cmd])
		fs.PrintDefaults()
	}
	dir := fs.String("data-dir", nodestate.DefaultDataDir(), "Data directory of the running node")
	asJSON := fs.Bool("json", false, "Print JSON")
	timeout := 30 * time.Second
	var peer, service, labels string
	switch cmd {
	case "find":
		fs.StringVar(&peer, "peer", "", "Search only the node with this peer ID")
		fs.StringVar(&service, "service", "", "Show only the tools of this MCP service, such as code-reviewer")
	case "describe":
		fs.StringVar(&peer, "peer", "", "Peer ID of the node that offers TOOL; needed when several do")
	case "call":
		fs.StringVar(&peer, "peer", "", "Peer ID of the node that offers TOOL; needed when several do")
		fs.StringVar(&labels, "labels", "", "Call only a node attested with all these labels: key=value pairs separated by commas")
		fs.DurationVar(&timeout, "timeout", 5*time.Minute, "Give up on the call after this long")
	}
	pos, err := cli.ParseInterleaved(fs, args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case cmd == "find" && len(pos) > 1,
		cmd == "describe" && len(pos) != 1,
		cmd == "call" && (len(pos) < 1 || len(pos) > 2):
		return errors.New(toolUsage)
	}
	var tool, name string
	if cmd == "find" {
		if len(pos) == 1 {
			name = pos[0]
		}
	} else if tool, err = toolName(pos[0]); err != nil {
		return err
	}
	// The deadline covers reading the arguments from standard input too.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var arguments json.RawMessage
	if cmd == "call" && len(pos) == 2 {
		if arguments, err = readArguments(ctx, pos[1], stdin); err != nil {
			return err
		}
	}
	if err := nodestate.CheckHalt(*dir); err != nil {
		return err
	}
	socket, err := readySocket(ctx, *dir)
	if err != nil {
		return err
	}
	node, err := connectNode(ctx, socket)
	if err != nil {
		return err
	}
	defer node.Close()

	if cmd == "find" {
		return safeError(findTools(ctx, node, peer, service, name, *asJSON, out))
	}
	if peer == "" {
		if peer, err = findToolPeer(ctx, node, tool); err != nil {
			return safeError(err)
		}
	}
	if cmd == "describe" {
		return safeError(describeTool(ctx, node, peer, tool, *asJSON, out))
	}
	params := map[string]any{"peer_id": peer, "tool_name": tool}
	if arguments != nil {
		params["arguments"] = arguments
	}
	if labels != "" {
		params["required_labels"] = labels
	}
	return safeError(callTool(ctx, node, params, *asJSON, out))
}

// connectNode opens an MCP session with the node's own MCP server on its
// socket.
func connectNode(ctx context.Context, socket string) (*mcp.ClientSession, error) {
	// The host is never dialled; the socket is.
	node, err := mcp.NewClient(&mcp.Implementation{Name: "agentiscaled-tool", Version: "0.1.0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://agentiscaled/mcp", HTTPClient: nodeclient.SocketClient(socket)}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to the node: %w", err)
	}
	return node, nil
}

// toolName gives TOOL its namespace: code-reviewer/review_pr is
// mcp://code-reviewer/review_pr.
func toolName(s string) (string, error) {
	if !strings.Contains(s, "://") {
		s = api.MCPServicePrefix + s
	}
	if _, _, err := api.SplitToolName(s); err != nil {
		return "", fmt.Errorf("%q is not a tool name such as mcp://code-reviewer/review_pr (see agentiscaled network tool find)", s)
	}
	return s, nil
}

// maxArgumentBytes bounds the arguments read from standard input.
const maxArgumentBytes = 16 << 20

// readArguments returns ARGUMENTS, or with "-" standard input, as the JSON
// object it is. It stays raw JSON all the way to the remote tool, so that
// numbers keep every digit: decoded into Go values, an integer above 2^53
// would change.
func readArguments(ctx context.Context, s string, stdin io.Reader) (json.RawMessage, error) {
	data := []byte(s)
	if s == "-" {
		var err error
		if data, err = readStdin(ctx, stdin); err != nil {
			return nil, err
		}
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' || !json.Valid(data) {
		return nil, errors.New(`ARGUMENTS must be a JSON object, such as '{"pr": 42}'`)
	}
	return data, nil
}

// readStdin reads standard input until it ends, at most maxArgumentBytes of
// it, or until ctx is done: a read blocked on a terminal or a pipe nobody
// writes to ends with the deadline or an interrupt.
func readStdin(ctx context.Context, stdin io.Reader) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(stdin, maxArgumentBytes+1))
		done <- result{data, err}
	}()
	select {
	case <-ctx.Done():
		return nil, withExit(timeoutExit(ctx), fmt.Errorf("read the arguments from standard input: %w", ctx.Err()))
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("read the arguments from standard input: %w", r.err)
		}
		if len(r.data) > maxArgumentBytes {
			return nil, fmt.Errorf("the arguments on standard input exceed %d MiB", maxArgumentBytes>>20)
		}
		return r.data, nil
	}
}

// callNode calls one of the node's tools and returns its text, or the
// tool's own error.
func callNode(ctx context.Context, node *mcp.ClientSession, name string, params map[string]any) (*mcp.CallToolResult, error) {
	res, err := node.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: params})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, errors.New(resultText(res))
	}
	return res, nil
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func readRemoteTools(ctx context.Context, node *mcp.ClientSession, params map[string]any) ([]RemoteTool, error) {
	res, err := callNode(ctx, node, findRemoteTools, params)
	if err != nil {
		return nil, fmt.Errorf("find tools: %w", err)
	}
	var rows []RemoteTool
	if err := json.Unmarshal([]byte(resultText(res)), &rows); err != nil {
		return nil, fmt.Errorf("find tools: read the node's answer: %w", err)
	}
	return rows, nil
}

func findTools(ctx context.Context, node *mcp.ClientSession, peer, service, name string, asJSON bool, out io.Writer) error {
	params := map[string]any{}
	if peer != "" {
		params["peer_id"] = peer
	}
	if service != "" {
		params["service_name"] = service
	}
	if name != "" {
		params["tool_name"] = name
	}
	rows, err := readRemoteTools(ctx, node, params)
	if err != nil {
		return err
	}
	// Tools first, by name and node; then the nodes that did not answer.
	slices.SortFunc(rows, func(a, b RemoteTool) int {
		if (a.Error == "") != (b.Error == "") {
			if a.Error == "" {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a.ToolName, b.ToolName); c != 0 {
			return c
		}
		return strings.Compare(a.PeerID, b.PeerID)
	})
	if asJSON {
		if rows == nil {
			rows = []RemoteTool{}
		}
		return json.NewEncoder(out).Encode(RemoteToolList{Tools: rows})
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(out, "No tools found.")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "TOOL\tPEER\tDESCRIPTION"); err != nil {
		return err
	}
	for _, row := range rows {
		tool, description := row.ToolName, row.Description
		if row.Error != "" {
			tool, description = "-", "unavailable: "+row.Error
		}
		description = strings.Join(strings.Fields(terminalSafe(description)), " ")
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", terminalSafe(tool), row.PeerID, description); err != nil {
			return err
		}
	}
	return w.Flush()
}

// findToolPeer finds the one node that offers tool.
func findToolPeer(ctx context.Context, node *mcp.ClientSession, tool string) (string, error) {
	service, name, _ := api.SplitToolName(tool)
	rows, err := readRemoteTools(ctx, node, map[string]any{"service_name": service, "tool_name": name})
	if err != nil {
		return "", err
	}
	var peers []string
	for _, row := range rows {
		if row.Error == "" && row.ToolName == tool && !slices.Contains(peers, row.PeerID) {
			peers = append(peers, row.PeerID)
		}
	}
	switch len(peers) {
	case 1:
		return peers[0], nil
	case 0:
		return "", fmt.Errorf("no node of your network offers %s (see agentiscaled network tool find)", tool)
	}
	slices.Sort(peers)
	return "", fmt.Errorf("%d nodes offer %s: pick one with -peer (%s)", len(peers), tool, strings.Join(peers, ", "))
}

func describeTool(ctx context.Context, node *mcp.ClientSession, peer, tool string, asJSON bool, out io.Writer) error {
	res, err := callNode(ctx, node, describeRemoteTool, map[string]any{"peer_id": peer, "tool_name": tool})
	if err != nil {
		return fmt.Errorf("describe %s: %w", tool, err)
	}
	data := json.RawMessage(resultText(res))
	if !json.Valid(data) {
		return fmt.Errorf("describe %s: the node's answer is not JSON", tool)
	}
	enc := json.NewEncoder(out)
	if !asJSON {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(data)
}

// callTool prints the tool's result: its text, and any other content or
// structured result as JSON, or with asJSON the whole result. A result the
// tool marks as an error is the command's error.
func callTool(ctx context.Context, node *mcp.ClientSession, params map[string]any, asJSON bool, out io.Writer) error {
	tool := params["tool_name"]
	res, err := node.CallTool(ctx, &mcp.CallToolParams{Name: callRemoteTool, Arguments: params})
	if err != nil {
		return fmt.Errorf("call %s: %w", tool, err)
	}
	switch {
	case asJSON:
		if err := json.NewEncoder(out).Encode(res); err != nil {
			return err
		}
	case res.IsError:
	case len(res.Content) == 0 && res.StructuredContent != nil:
		if err := json.NewEncoder(out).Encode(res.StructuredContent); err != nil {
			return err
		}
	default:
		for _, c := range res.Content {
			var line string
			if text, ok := c.(*mcp.TextContent); ok {
				line = strings.TrimSuffix(terminalSafe(text.Text), "\n")
			} else {
				b, err := json.Marshal(c)
				if err != nil {
					return err
				}
				line = string(b)
			}
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
		}
	}
	if res.IsError {
		return fmt.Errorf("%s failed: %s", tool, resultText(res))
	}
	return nil
}
