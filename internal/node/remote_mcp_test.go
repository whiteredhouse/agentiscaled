package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/export/standalone"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A client of `agentiscaled mcp`, over stdio, finds an MCP tool that a
// service configured on another node offers, reads its schema and calls it.
func TestRemoteMCPToolThroughStdio(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("starts a mesh and two nodes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	mesh, err := standalone.New(standalone.Options{BindAddress: "127.0.0.1:0", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := mesh.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer mesh.Close()
	joinToken := filepath.Join(t.TempDir(), "join-token")
	if err := os.WriteFile(joinToken, []byte(mesh.JoinToken()), 0o600); err != nil {
		t.Fatal(err)
	}

	// B's owner configures an MCP server with one tool as a service.
	backend := mcp.NewServer(&mcp.Implementation{Name: "tools", Version: "1"}, nil)
	type echoIn struct {
		Message string `json:"message" jsonschema:"what to echo"`
	}
	mcp.AddTool(backend, &mcp.Tool{Name: "echo_arg", Description: "Echo a message"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + in.Message}}}, nil, nil
	})
	backendSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backend }, nil))
	defer backendSrv.Close()
	nodeConfig := filepath.Join(t.TempDir(), "node.yaml")
	yaml := fmt.Sprintf("version: \"v1alpha1\"\nservices:\n  - type: \"mcp\"\n    name: \"tools\"\n    description: \"Workspace tools\"\n    target_url: %q\n", backendSrv.URL)
	if err := os.WriteFile(nodeConfig, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	node := func(name, configFile string) string {
		t.Helper()
		dir := shortTempDir(t)
		c := config{dataDir: dir, controlPlane: mesh.PublicURL(), bootstrapTokenPath: joinToken, listen: "/ip4/127.0.0.1/tcp/0", logLevel: "info", machineName: name}
		if _, err := enrollOnly(ctx, c); err != nil {
			t.Fatalf("enroll %s: %v", name, err)
		}
		writeSettings(t, dir, settings{Listen: c.listen, LogLevel: "info", Clients: "none", Foreground: true, ConfigFile: configFile, MeshURL: mesh.PublicURL()})
		startNode(t, dir).waitLog(t, "agentiscaled running", 30*time.Second)
		return dir
	}
	a := node("node-a", "")
	node("node-b", nodeConfig)

	// The client starts `agentiscaled mcp` for A and speaks to it on stdio.
	cmd := exec.Command(os.Args[0], "mcp", "-data-dir", a)
	cmd.Env = append(os.Environ(), "AGENTISCALED_TEST_MCP=1")
	cmd.Stderr = os.Stderr
	session, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "test"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	text := func(res *mcp.CallToolResult) string {
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}

	// Find: B's service may take a moment to reach the mesh.
	var found []struct {
		PeerID   string `json:"peer_id"`
		ToolName string `json:"tool_name"`
	}
	var peerB, last string
	for peerB == "" {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "find_remote_tools", Arguments: map[string]any{"tool_name": "echo_arg"}})
		if err != nil {
			t.Fatalf("find_remote_tools: %v", err)
		}
		if !res.IsError {
			last = text(res)
			if json.Unmarshal([]byte(last), &found) == nil {
				for _, tool := range found {
					if tool.ToolName == "mcp://tools/echo_arg" {
						peerB = tool.PeerID
					}
				}
			}
		}
		if peerB == "" {
			if ctx.Err() != nil {
				t.Fatalf("find_remote_tools never found mcp://tools/echo_arg: %q", last)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// Describe: the tool's input schema.
	desc, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "describe_remote_tool", Arguments: map[string]any{"peer_id": peerB, "tool_name": "mcp://tools/echo_arg"}})
	if err != nil || desc.IsError || !strings.Contains(text(desc), "message") {
		t.Fatalf("describe_remote_tool: %+v %v", desc, err)
	}

	// Call.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "call_remote_tool", Arguments: map[string]any{"peer_id": peerB, "tool_name": "mcp://tools/echo_arg", "arguments": map[string]any{"message": "over stdio"}}})
	if err != nil || res.IsError || !strings.Contains(text(res), "echo:over stdio") {
		t.Fatalf("call_remote_tool: %+v %v", res, err)
	}
}
