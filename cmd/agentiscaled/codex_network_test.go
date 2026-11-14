package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// codexNetworkNamespace is the network's MCP server as Codex names its
// tools: mcp__<namespace>__<tool>.
const codexNetworkNamespace = "agentiscale_network"

// TestCodexCallsTheNetwork runs the real Codex CLI, given by
// AGENTISCALE_TEST_CODEX, with a turn's network (taskNetwork) against a fake
// model that calls list_agents, runs a command that looks for the token,
// then calls send_task. Codex must reach the network's MCP server with the
// token from its environment, and call its tools with no approval prompt,
// while the commands it runs do not get the token.
//
//	AGENTISCALE_TEST_CODEX=$(command -v codex) go test ./cmd/agentiscaled -run TestCodexCallsTheNetwork -v
func TestCodexCallsTheNetwork(t *testing.T) {
	command := os.Getenv("AGENTISCALE_TEST_CODEX")
	if command == "" {
		t.Skip("set AGENTISCALE_TEST_CODEX to a Codex CLI to run")
	}
	for name, policy := range map[string]agentPolicy{
		"isolated":     {},
		"owner config": {OwnerConfig: true},
	} {
		t.Run(name, func(t *testing.T) {
			home := codexTempDir(t)
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
			dataDir := codexTempDir(t)
			if policy.OwnerConfig {
				t.Setenv("CODEX_API_KEY", "owner-codex-key")
				write(t, filepath.Join(home, ".codex", "config.toml"), "")
			} else {
				write(t, filepath.Join(dataDir, "codex", "auth.json"), `{"OPENAI_API_KEY": "agent-key"}`)
			}
			rt, err := newCodexRuntime(command, dataDir, policy)
			if err != nil {
				t.Fatal(err)
			}
			codex := rt.(*codexRuntime)

			const token = "task-token-0123456789"
			access, calls := fakeNodeNetwork(t, token)

			model := &networkModel{probe: "echo token:$" + networkTokenEnv + ":end"}
			ms := httptest.NewServer(model)
			defer ms.Close()
			codex.extra = []string{
				"-c", `model_provider="agentiscale-test"`,
				"-c", fmt.Sprintf(`model_providers.agentiscale-test={name="agentiscale-test", base_url="%s/v1", wire_api="responses", requires_openai_auth=true}`, ms.URL),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			out, err := codex.Run(ctx, t.TempDir(), taskRequest{Sender: testSender, Text: "Ask the helper", Network: access})
			if err != nil {
				t.Fatal(err)
			}
			if out.Text != "done" {
				t.Fatalf("answer %q", out.Text)
			}
			offered, outputs := model.seen()
			t.Logf("tool outputs %q", outputs)
			if !offered {
				t.Fatal("Codex did not offer the network's tools")
			}
			if len(outputs) < 3 {
				t.Fatalf("tool outputs %q", outputs)
			}
			// The call reached the network with the turn's token.
			if !strings.Contains(outputs[0], "helper") || strings.Contains(outputs[0], `\"caller\"`) {
				t.Errorf("list_agents answered %q", outputs[0])
			}
			// Both calls reached the node with the turn's token; send_task,
			// which is not read-only, without an approval prompt.
			checkNodeCalls(t, calls(), token)
			if strings.Contains(outputs[2], "approval") {
				t.Errorf("send_task: %s", outputs[2])
			}
			// The command ran, without the token.
			if !strings.Contains(outputs[1], "token:") || strings.Contains(outputs[1], token) {
				t.Errorf("the command's output %q", outputs[1])
			}
		})
	}
}

// fakeNodeNetwork opens a turn's network (taskNetwork) for token, in front
// of a stand-in for the node's local agent API, which knows the agents
// caller (the turn's own) and helper, and refuses tasks. calls lists the
// requests the stand-in got, with the token they presented.
func fakeNodeNetwork(t *testing.T, token string) (access *networkAccess, calls func() string) {
	t.Helper()
	var presented []string
	var mu sync.Mutex
	agentSocket := filepath.Join(shortTempDir(t), "agent.sock")
	ln, err := net.Listen("unix", agentSocket)
	if err != nil {
		t.Fatal(err)
	}
	api := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		presented = append(presented, r.Method+" "+r.URL.Path+" "+r.Header.Get("X-Mesh-Authentication"))
		mu.Unlock()
		if r.URL.Path != "/info" {
			http.Error(w, "the helper is busy", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(localAgentInfo{PeerID: testSender, Services: []string{"caller", "helper"}})
	})}
	go api.Serve(ln)
	t.Cleanup(func() { api.Close() })
	network := &taskNetwork{agentSocket: agentSocket, grants: map[[32]byte]*networkGrant{}, sent: map[a2a.TaskID]*sentTasks{}}
	srv := httptest.NewServer(network)
	network.url = srv.URL + "/mcp"
	access, closeGrant := network.open(token, "caller", network.sentBy("t-1"), time.Now().Add(10*time.Minute))
	t.Cleanup(func() {
		closeGrant()
		network.wait()
		srv.Close()
	})
	return access, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(presented, "\n")
	}
}

// checkNodeCalls checks that list_agents and send_task reached the node
// with the turn's token.
func checkNodeCalls(t *testing.T, calls, token string) {
	t.Helper()
	for _, want := range []string{"GET /info Bearer " + token, "POST /a2a/" + testSender + "/helper Bearer " + token} {
		if !strings.Contains(calls, want) {
			t.Errorf("the node got %q, not %q", calls, want)
		}
	}
}

// networkModel answers the Responses API: the first turn calls the
// network's list_agents tool from a script of Codex's exec tool, which is
// how Codex offers MCP tools, the next runs probe, the next calls
// send_task, the last answers "done".
type networkModel struct {
	probe string

	mu      sync.Mutex
	offered bool
	outputs []string
}

func (m *networkModel) seen() (bool, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.offered, m.outputs
}

func (m *networkModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[],"models":[]}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Input []map[string]any `json:"input"`
	}
	_ = json.Unmarshal(body, &req)
	var outputs []string
	for _, item := range req.Input {
		if t, _ := item["type"].(string); t == "custom_tool_call_output" || t == "function_call_output" {
			b, _ := json.Marshal(item["output"])
			outputs = append(outputs, string(b))
		}
	}
	m.mu.Lock()
	m.offered = m.offered || strings.Contains(string(body), "mcp__"+codexNetworkNamespace)
	m.outputs = outputs
	m.mu.Unlock()

	var item map[string]any
	switch len(outputs) {
	case 0:
		item = map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "c1",
			"input": "const r = await tools.mcp__" + codexNetworkNamespace + "__list_agents({});\ntext(JSON.stringify(r));"}
	case 1:
		cmd, _ := json.Marshal(m.probe)
		item = map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "c2",
			"input": fmt.Sprintf("const r = await tools.exec_command({cmd: %s, login: false});\ntext(JSON.stringify(r));", cmd)}
	case 2:
		item = map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "c3",
			"input": "const r = await tools.mcp__" + codexNetworkNamespace + "__send_task({service: \"helper\", text: \"hi\"});\ntext(JSON.stringify(r));"}
	default:
		item = map[string]any{"type": "message", "role": "assistant", "id": "m1",
			"content": []map[string]any{{"type": "output_text", "text": "done"}}}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(kind string, data map[string]any) {
		data["type"] = kind
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
	}
	event("response.created", map[string]any{"response": map[string]any{"id": "r1"}})
	event("response.output_item.done", map[string]any{"item": item})
	event("response.completed", map[string]any{"response": map[string]any{"id": "r1",
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
}
