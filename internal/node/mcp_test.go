package node

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/whiteredhouse/agentiscaled/internal/caller"
	"github.com/whiteredhouse/agentiscaled/internal/nodeclient"
)

// A client speaking MCP over stdio reaches the tools of a Streamable HTTP
// server on a Unix socket through `agentiscaled mcp`, for a session of
// several requests, with the bridge's shorter descriptions and instructions.
func TestMCPBridgeReachesTheNodeOverItsSocket(t *testing.T) {
	// Unix socket paths are short; t.TempDir can be too long on macOS.
	dir, err := os.MkdirTemp("", "ais")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "node.sock")

	server := mcp.NewServer(&mcp.Implementation{Name: "node", Version: "test"}, &mcp.ServerOptions{Instructions: "Use find_remote_tools for remote MCP tools."})
	type echoIn struct {
		Text string `json:"text"`
	}
	echo := func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
	}
	// One of the node's network tools, and one of its remote MCP tools.
	mcp.AddTool(server, &mcp.Tool{Name: "get_mesh_info", Description: "echo"}, echo)
	mcp.AddTool(server, &mcp.Tool{Name: "find_remote_tools", Description: "echo"}, echo)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	// The node serves MCP at /mcp, like agentmesh-node's NewMCPHandler.
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	// The client's stdin and stdout, crossed over to the bridge.
	toBridge, fromClient := io.Pipe()
	toClient, fromBridge := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bridged := make(chan error, 1)
	go func() {
		bridged <- caller.BridgeMCP(ctx, &mcp.IOTransport{Reader: toBridge, Writer: fromBridge}, socket)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: toClient, Writer: fromClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	// The node's tools, plus the A2A task tools the bridge adds.
	if want := []string{"cancel_task", "find_remote_tools", "get_agent_card", "get_mesh_info", "get_task", "send_task"}; !slices.Equal(names, want) {
		t.Fatalf("tools: %v, want %v", names, want)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "find_remote_tools" && !strings.Contains(tool.Description, "mcp://service/tool") {
			t.Fatalf("find_remote_tools kept the node's description: %q", tool.Description)
		}
	}
	if got := session.InitializeResult().Instructions; got != caller.BridgeInstructions {
		t.Fatalf("instructions: %q", got)
	}
	for _, text := range []string{"one", "two"} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_mesh_info", Arguments: map[string]any{"text": text}})
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Content[0].(*mcp.TextContent).Text; got != text {
			t.Fatalf("echo %q: got %q", text, got)
		}
	}

	// Closing the client ends the bridge cleanly.
	session.Close()
	select {
	case err := <-bridged:
		if err != nil {
			t.Fatalf("bridge: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the bridge did not stop when the client closed")
	}
}

func TestMCPNeedsARunningNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := caller.RunMCP(ctx, []string{"-data-dir", t.TempDir()}); err == nil {
		t.Fatal("no socket: want an error")
	}
}

// At login the client may start before the node's background service has
// opened its socket, and a socket file may be left from before a restart:
// the wait ends only when the node accepts connections.
func TestMCPWaitsForTheNodeToStart(t *testing.T) {
	dir, err := os.MkdirTemp("", "ais")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	stale, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("stale socket file: %v", err)
	}

	started := make(chan net.Listener, 1)
	go func() {
		time.Sleep(500 * time.Millisecond)
		os.Remove(socket)
		ln, _ := net.Listen("unix", socket)
		started <- ln
	}()
	t.Cleanup(func() {
		if ln := <-started; ln != nil {
			ln.Close()
		}
	})
	start := time.Now()
	if err := nodeclient.WaitForSocket(context.Background(), socket, 5*time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Fatal("a stale socket file counted as a running node")
	}
	if err := nodeclient.WaitForSocket(context.Background(), filepath.Join(dir, "none.sock"), 300*time.Millisecond); err == nil {
		t.Fatal("no node: want an error")
	}
}
