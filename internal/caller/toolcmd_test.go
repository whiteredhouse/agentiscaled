package caller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	meshnode "github.com/google/agentmesh/export/node"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	peerA = "12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN"
	peerB = "12D3KooWHHzSeKaY8xuZVzkLbKFfvNgPPeKhFBGrMbNzbm5akpqu"
)

// fakeNode serves the node's remote-tool MCP tools on a socket in a new
// data directory and records the arguments of each call.
func fakeNode(t *testing.T, rows []RemoteTool) (dir string, calls map[string][]map[string]any) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ais")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	calls = map[string][]map[string]any{}
	server := mcp.NewServer(&mcp.Implementation{Name: "node", Version: "0"}, nil)
	add := func(name string, answer func(map[string]any) *mcp.CallToolResult) {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			calls[name] = append(calls[name], args)
			return answer(args), nil
		})
	}
	text := func(s string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
	}
	add(findRemoteTools, func(args map[string]any) *mcp.CallToolResult {
		var out []RemoteTool
		for _, row := range rows {
			if name, ok := args["tool_name"].(string); ok && !strings.HasSuffix(row.ToolName, "/"+name) {
				continue
			}
			out = append(out, row)
		}
		b, _ := json.Marshal(out)
		return text(string(b))
	})
	add(describeRemoteTool, func(args map[string]any) *mcp.CallToolResult {
		b, _ := json.Marshal(map[string]any{"peer_id": args["peer_id"], "tool_name": args["tool_name"], "input_schema": map[string]any{"type": "object"}})
		return text(string(b))
	})
	add(callRemoteTool, func(args map[string]any) *mcp.CallToolResult {
		if a, _ := args["arguments"].(map[string]any); a["fail"] == true {
			res := text("no such pull request")
			res.IsError = true
			return res
		}
		b, _ := json.Marshal(args["arguments"])
		return text("reviewed " + string(b))
	})
	ln, err := net.Listen("unix", filepath.Join(dir, meshnode.DefaultSocketName))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return dir, calls
}

func runTool(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := RunTool(context.Background(), args, strings.NewReader(stdin), &out, &errOut)
	return out.String(), err
}

func TestToolFind(t *testing.T) {
	dir, calls := fakeNode(t, []RemoteTool{
		{PeerID: peerB, ToolName: "mcp://code-reviewer/review_pr", Description: "Review a\npull request"},
		{PeerID: peerA, Error: "dial timeout"},
		{PeerID: peerA, ToolName: "mcp://code-reviewer/review_pr", Description: "Review a pull request"},
	})
	out, err := runTool(t, "", "find", "-data-dir", dir, "-service", "code-reviewer")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "TOOL") ||
		!strings.Contains(lines[1], peerA) || !strings.Contains(lines[2], peerB) || !strings.Contains(lines[2], "Review a pull request") ||
		!strings.Contains(lines[3], "unavailable: dial timeout") {
		t.Fatalf("find printed:\n%s", out)
	}
	if got := calls[findRemoteTools][0]; got["service_name"] != "code-reviewer" || got["tool_name"] != nil {
		t.Fatalf("find_remote_tools got %v", got)
	}
	if _, err := runTool(t, "", "find", "review_pr", "-data-dir", dir, "-peer", peerA); err != nil {
		t.Fatal(err)
	}
	if got := calls[findRemoteTools][1]; got["tool_name"] != "review_pr" || got["peer_id"] != peerA {
		t.Fatalf("find_remote_tools got %v", got)
	}

	out, err = runTool(t, "", "find", "-data-dir", dir, "-json")
	if err != nil {
		t.Fatal(err)
	}
	var list RemoteToolList
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Tools) != 3 {
		t.Fatalf("find -json printed %s (%v)", out, err)
	}
}

func TestToolDescribeAndCallFindTheOneNode(t *testing.T) {
	dir, calls := fakeNode(t, []RemoteTool{{PeerID: peerA, ToolName: "mcp://code-reviewer/review_pr"}})
	out, err := runTool(t, "", "describe", "code-reviewer/review_pr", "-data-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	var desc map[string]any
	if err := json.Unmarshal([]byte(out), &desc); err != nil || desc["peer_id"] != peerA || desc["tool_name"] != "mcp://code-reviewer/review_pr" {
		t.Fatalf("describe printed %s (%v)", out, err)
	}

	out, err = runTool(t, `{"pr": 42}`, "call", "-data-dir", dir, "-labels", "region=eu", "mcp://code-reviewer/review_pr", "-")
	if err != nil {
		t.Fatal(err)
	}
	if out != "reviewed {\"pr\":42}\n" {
		t.Fatalf("call printed %q", out)
	}
	got := calls[callRemoteTool][0]
	if got["peer_id"] != peerA || got["required_labels"] != "region=eu" {
		t.Fatalf("call_remote_tool got %v", got)
	}

	_, err = runTool(t, "", "call", "-data-dir", dir, "code-reviewer/review_pr", `{"fail": true}`)
	if err == nil || !strings.Contains(err.Error(), "no such pull request") {
		t.Fatalf("a failed call returned %v", err)
	}
}

func TestToolNeedsPeerWhenSeveralNodesOfferIt(t *testing.T) {
	dir, calls := fakeNode(t, []RemoteTool{
		{PeerID: peerA, ToolName: "mcp://code-reviewer/review_pr"},
		{PeerID: peerB, ToolName: "mcp://code-reviewer/review_pr"},
	})
	_, err := runTool(t, "", "call", "-data-dir", dir, "code-reviewer/review_pr")
	if err == nil || !strings.Contains(err.Error(), "pick one with -peer") || len(calls[callRemoteTool]) != 0 {
		t.Fatalf("an ambiguous call returned %v", err)
	}
	if _, err := runTool(t, "", "call", "-data-dir", dir, "-peer", peerB, "code-reviewer/review_pr"); err != nil {
		t.Fatal(err)
	}
	if got := calls[callRemoteTool][0]; got["peer_id"] != peerB || got["arguments"] != nil {
		t.Fatalf("call_remote_tool got %v", got)
	}
	if _, err := runTool(t, "", "describe", "-data-dir", dir, "code-reviewer/other"); err == nil || !strings.Contains(err.Error(), "no node of your network offers") {
		t.Fatalf("describing a missing tool returned %v", err)
	}
}

func TestToolRejectsBadInput(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"list"},
		{"describe"},
		{"call", "review_pr"},
		{"call", "code-reviewer/review_pr", "[1]"},
	} {
		_, err := runTool(t, "", append(args, "-data-dir", t.TempDir())...)
		if err == nil || errors.Is(err, context.Canceled) {
			t.Errorf("tool %v returned %v", args, err)
		}
	}
}
