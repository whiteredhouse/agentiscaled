package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestClaudeCallsTheNetwork runs the real Claude Code CLI, given by
// AGENTISCALE_TEST_CLAUDE, with a turn's network (taskNetwork) against a
// fake model that calls list_agents, then send_task. Claude Code must reach
// the network's MCP server with the turn's token and call its tools with no
// permission prompt, restricted or with the owner's configuration.
//
//	AGENTISCALE_TEST_CLAUDE=$(command -v claude) go test ./cmd/agentiscaled -run TestClaudeCallsTheNetwork -v
func TestClaudeCallsTheNetwork(t *testing.T) {
	command := os.Getenv("AGENTISCALE_TEST_CLAUDE")
	if command == "" {
		t.Skip("set AGENTISCALE_TEST_CLAUDE to a Claude Code CLI to run")
	}
	for name, policy := range map[string]agentPolicy{
		"isolated":     {},
		"owner config": {OwnerConfig: true},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
			const token = "task-token-0123456789"
			access, calls := fakeNodeNetwork(t, token)
			model := &claudeModel{}
			ms := httptest.NewServer(model)
			defer ms.Close()
			// The isolated run takes no variable from the node's
			// environment but these settings; the owner's, the
			// environment first.
			env := map[string]string{"ANTHROPIC_BASE_URL": ms.URL, "ANTHROPIC_API_KEY": "sk-ant-test", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"}
			for k, v := range env {
				t.Setenv(k, v)
			}
			for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			settings, _ := json.Marshal(map[string]any{"env": env})
			// Built as newClaudeRuntime would, without its sign-in check.
			help, err := exec.Command(command, "--help").Output()
			if err != nil {
				t.Fatal(err)
			}
			rt := &claudeRuntime{command: command, policy: policy, noPermissionPrompts: !strings.Contains(string(help), "--permission-prompts"),
				extra: []string{"--settings", string(settings)}}
			if !policy.OwnerConfig {
				rt.config = filepath.Join(t.TempDir(), "claude")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			out, err := rt.Run(ctx, t.TempDir(), taskRequest{Sender: testSender, Text: "Ask the helper", Network: access})
			if err != nil {
				t.Fatal(err)
			}
			if out.Text != "done" {
				t.Fatalf("answer %q", out.Text)
			}
			results := model.seen()
			t.Logf("tool results %q", results)
			if len(results) < 2 {
				t.Fatalf("tool results %q", results)
			}
			last := results[len(results)-2:]
			if !strings.Contains(last[0], "helper") || strings.Contains(last[0], "caller") {
				t.Errorf("list_agents answered %q", last[0])
			}
			if !strings.Contains(last[1], "503") {
				t.Errorf("send_task answered %q", last[1])
			}
			checkNodeCalls(t, calls(), token)
		})
	}
}

// claudeModel answers the Messages API: it loads the network's tools with
// ToolSearch when Claude Code defers them, calls list_agents, then
// send_task, and then answers "done" in the turn's schema.
type claudeModel struct {
	mu      sync.Mutex
	results []string
}

func (m *claudeModel) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.results
}

func (m *claudeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[]}`)
		return
	}
	var req struct {
		Stream bool `json:"stream"`
		Tools  []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var tools []string
	for _, tool := range req.Tools {
		tools = append(tools, tool.Name)
	}
	var results []string
	for _, msg := range req.Messages {
		var blocks []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(msg.Content, &blocks)
		for _, b := range blocks {
			if b.Type == "tool_result" {
				results = append(results, string(b.Content))
			}
		}
	}
	server := "mcp__" + networkServerName + "__"
	type step struct {
		name  string
		input map[string]any
	}
	var steps []step
	if slices.Contains(tools, "ToolSearch") {
		steps = append(steps, step{"ToolSearch", map[string]any{"query": "select:" + server + "list_agents," + server + "send_task", "max_results": 5}})
	}
	steps = append(steps, step{server + "list_agents", map[string]any{}}, step{server + "send_task", map[string]any{"service": "helper", "text": "hi"}})

	block := map[string]any{"type": "text", "text": "ok"}
	stop := "end_turn"
	switch {
	case !slices.Contains(tools, "StructuredOutput"):
		// A side request of Claude Code's, such as a title.
	case len(results) < len(steps):
		s := steps[len(results)]
		block = map[string]any{"type": "tool_use", "id": fmt.Sprintf("tu%d", len(results)), "name": s.name, "input": s.input}
		stop = "tool_use"
	default:
		m.mu.Lock()
		m.results = results
		m.mu.Unlock()
		block = map[string]any{"type": "tool_use", "id": "tu-end", "name": "StructuredOutput", "input": map[string]any{"status": "done", "text": "done"}}
		stop = "tool_use"
	}
	message := map[string]any{"id": "m1", "type": "message", "role": "assistant", "model": "test", "content": []any{},
		"stop_reason": nil, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}
	if !req.Stream {
		message["content"], message["stop_reason"] = []any{block}, stop
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(message)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(kind string, data map[string]any) {
		data["type"] = kind
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
	}
	event("message_start", map[string]any{"message": message})
	if block["type"] == "text" {
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": block["text"]}})
	} else {
		input, _ := json.Marshal(block["input"])
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}})
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
	}
	event("content_block_stop", map[string]any{"index": 0})
	event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": 1}})
	event("message_stop", map[string]any{})
}
