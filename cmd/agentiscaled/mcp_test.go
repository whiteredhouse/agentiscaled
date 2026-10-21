package main

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
)

// A client speaking MCP over stdio reaches the tools of a Streamable HTTP
// server on a Unix socket through `agentiscaled mcp`, for a session of
// several requests.
func TestMCPBridgeReachesTheNodeOverItsSocket(t *testing.T) {
	// Unix socket paths are short; t.TempDir can be too long on macOS.
	dir, err := os.MkdirTemp("", "ais")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "node.sock")

	server := mcp.NewServer(&mcp.Implementation{Name: "node", Version: "test"}, nil)
	type echoIn struct {
		Text string `json:"text"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
	})
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
		bridged <- bridgeMCP(ctx, &mcp.IOTransport{Reader: toBridge, Writer: fromBridge}, socket)
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
	if want := []string{"cancel_task", "echo", "get_task", "send_task"}; !slices.Equal(names, want) {
		t.Fatalf("tools: %v, want %v", names, want)
	}
	for _, text := range []string{"one", "two"} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": text}})
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
	if err := runMCP(context.Background(), []string{"-data-dir", t.TempDir()}); err == nil {
		t.Fatal("no socket: want an error")
	}
}

// The node's instructions send clients to the a2a CLI for A2A agents; the
// bridge drops that paragraph for its own tools. The paragraph must still
// start the way the bridge looks for, in the fork it builds against.
func TestTheBridgeReplacesTheNodesA2AGuide(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "third_party", "agentmesh", "internal", "node", "mcp.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "\n\n"+nodeA2AGuide) {
		t.Fatalf("the node's instructions no longer have a paragraph starting %q", nodeA2AGuide)
	}
	in := "Intro.\n\n" + nodeA2AGuide + " are NOT called via call_remote_tool: use the a2a CLI.\n\nAuth."
	if got := withoutA2AGuide(in); got != "Intro.\n\nAuth." {
		t.Fatalf("got %q", got)
	}
}
