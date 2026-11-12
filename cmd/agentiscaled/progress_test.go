package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLineWriterSplitsLinesAcrossWrites(t *testing.T) {
	var lines []string
	w := &lineWriter{line: func(b []byte) { lines = append(lines, string(b)) }}
	for _, chunk := range []string{"one\ntw", "o\n", "\nthr", "ee"} {
		if n, err := w.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("write %q: %d %v", chunk, n, err)
		}
	}
	w.Close()
	if want := []string{"one", "two", "", "three"}; !slices.Equal(lines, want) {
		t.Fatalf("lines %q, want %q", lines, want)
	}
}

// A line over the bound, such as a large tool output, is skipped whole and
// never kept, and the lines after it still come through.
func TestLineWriterSkipsOverlongLines(t *testing.T) {
	var lines []string
	w := &lineWriter{line: func(b []byte) { lines = append(lines, string(b)) }}
	big := strings.Repeat("x", 1<<20)
	w.Write([]byte("before\n"))
	for i := 0; i <= maxRuntimeLine>>20; i++ {
		w.Write([]byte(big))
	}
	if len(w.partial) > maxRuntimeLine {
		t.Fatalf("kept %d bytes of an overlong line", len(w.partial))
	}
	w.Write([]byte("tail\nafter\n"))
	w.Close()
	if want := []string{"before", "after"}; !slices.Equal(lines, want) {
		t.Fatalf("lines %q, want %q", lines, want)
	}
}

func TestProgressLineIsOneShortLine(t *testing.T) {
	if got := progressLine("go test \\\n  ./...\t-run X"); got != "go test \\ ./... -run X" {
		t.Fatalf("one line: %q", got)
	}
	long := progressLine(strings.Repeat("é", 500))
	if n := len([]rune(long)); n != maxProgressLine || !strings.HasSuffix(long, "…") {
		t.Fatalf("long line: %d runes %q", n, long[len(long)-8:])
	}
}

func TestClaudeProgress(t *testing.T) {
	for event, want := range map[string]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./...","description":"Run tests"}}]}}`:                                                                                                                "Bash: go test ./...",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/w/main.go","old_string":"a"}}]}}`:                                                                                                                          "Edit: /w/main.go",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"TODO"}}]}}`:                                                                                                                                                   "Grep: TODO",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Exotic","input":{"n":1}}]}}`:                                                                                                                                                            "Exotic",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TodoWrite","input":{"todos":[{"content":"Read code","status":"completed","activeForm":"Reading code"},{"content":"Fix bug","status":"in_progress","activeForm":"Fixing the bug"}]}}]}}`: "Working on: Fixing the bug",
		`{"type":"assistant","message":{"content":[{"type":"text","text":"\nI'll check the tests first.\nThen fix."}]}}`:                                                                                                                                              "I'll check the tests first.",
		// The turn's structured answer, results, thinking and other events are not steps.
		`{"type":"assistant","message":{"content":[{"type":"text","text":"{\"status\":\"done\",\"text\":\"ok\"}"}]}}`: "",
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"}]}}`:                           "",
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"PASS"}]}}`:                             "",
		`{"type":"result","result":"ok"}`:    "",
		`{"type":"system","subtype":"init"}`: "",
		`not json`:                           "",
	} {
		if got := claudeProgress([]byte(event)); got != want {
			t.Errorf("%s: %q, want %q", event, got, want)
		}
	}
}

func TestCodexProgress(t *testing.T) {
	for event, want := range map[string]string{
		`{"type":"item.started","item":{"id":"1","type":"command_execution","command":"bash -lc 'go test ./...'","status":"in_progress"}}`:          "Running: bash -lc 'go test ./...'",
		`{"type":"item.completed","item":{"id":"2","type":"file_change","changes":[{"path":"a.go","kind":"update"},{"path":"b.go","kind":"add"}]}}`: "Changed a.go, b.go",
		`{"type":"item.started","item":{"id":"3","type":"mcp_tool_call","server":"docs","tool":"search"}}`:                                          "Tool: docs.search",
		`{"type":"item.started","item":{"id":"4","type":"web_search","query":"a2a spec"}}`:                                                          "Searching: a2a spec",
		`{"type":"item.updated","item":{"id":"5","type":"todo_list","items":[{"text":"Read","completed":true},{"text":"Fix","completed":false}]}}`:  "Working on: Fix",
		`{"type":"item.completed","item":{"id":"6","type":"agent_message","text":"Tests pass; now the docs."}}`:                                     "Tests pass; now the docs.",
		// The turn's structured answer, finished commands, reasoning and other events are not steps.
		`{"type":"item.completed","item":{"id":"7","type":"agent_message","text":"{\"status\":\"done\",\"text\":\"ok\"}"}}`: "",
		`{"type":"item.completed","item":{"id":"1","type":"command_execution","command":"ls","exit_code":0}}`:               "",
		`{"type":"item.completed","item":{"id":"8","type":"reasoning","text":"hmm"}}`:                                       "",
		`{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}`:                                      "",
		`{"type":"turn.completed","usage":{}}`:                                                                              "",
	} {
		if got := codexProgress([]byte(event)); got != want {
			t.Errorf("%s: %q, want %q", event, got, want)
		}
	}
}

// Codex reports its steps while it runs, and its session from the same
// stream.
func TestCodexRuntimeReportsSteps(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = --output-last-message ] && out=$2; shift; done\ncat > /dev/null\n" +
		"echo '{\"type\":\"thread.started\",\"thread_id\":\"0199a213-81c0-7800-8aa1-bbab2a035a53\"}'\n" +
		"echo '{\"type\":\"item.started\",\"item\":{\"id\":\"1\",\"type\":\"command_execution\",\"command\":\"go vet\"}}'\n" +
		"echo '{\"type\":\"item.completed\",\"item\":{\"id\":\"2\",\"type\":\"file_change\",\"changes\":[{\"path\":\"main.go\",\"kind\":\"update\"}]}}'\n" +
		"printf '{\"status\":\"done\",\"text\":\"Fixed\"}' > \"$out\"\n"
	if err := os.WriteFile(codex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var steps []string
	out, err := (&codexRuntime{command: codex, home: filepath.Join(dir, "home")}).Run(context.Background(), dir,
		taskRequest{Sender: testSender, Text: "fix", Progress: func(s string) { steps = append(steps, s) }})
	if err != nil || out.Text != "Fixed" || out.Session != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
		t.Fatalf("run: %+v %v", out, err)
	}
	if want := []string{"Running: go vet", "Changed main.go"}; !slices.Equal(steps, want) {
		t.Fatalf("steps %q, want %q", steps, want)
	}
}

// The agent's steps reach the caller as the message of the working
// status, the latest one winning when they come faster than progressEvery,
// and the task's result is unchanged.
func TestTaskProgressReachesCaller(t *testing.T) {
	old := progressEvery
	progressEvery = 300 * time.Millisecond
	t.Cleanup(func() { progressEvery = old })
	h := newAgentHarness(t, testSender)
	task := h.send(t, "", "fix the bug", nil)
	<-h.fix.prompts
	h.fix.steps <- "Bash: go test ./..."
	h.awaitMessage(t, task.ID, "Bash: go test ./...")
	h.fix.steps <- "Edit: a.go"
	h.fix.steps <- "Edit: b.go"
	h.awaitMessage(t, task.ID, "Edit: b.go")
	h.fix.release <- "fixed"
	done := h.await(t, task.ID, a2a.TaskStateCompleted)
	if out := describeTask(done); out.Result != "fixed" || out.Terminal != "completed" || out.Message != "" {
		t.Fatalf("done: %+v", out)
	}
	var seen []string
	for _, m := range done.History {
		if m.Role == a2a.MessageRoleAgent {
			seen = append(seen, messageText(m))
		}
	}
	if want := []string{"Bash: go test ./...", "Edit: b.go"}; !slices.Equal(seen, want) {
		t.Fatalf("progress in history %q, want %q (a step overtaken within progressEvery is not sent)", seen, want)
	}
}

func (h *agentHarness) awaitMessage(t *testing.T, id a2a.TaskID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, err := h.client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		out := describeTask(task)
		if out.State == "working" && out.Message == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s: %+v, want working with message %q", id, out, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// get_task passes each new step to an MCP client that asked for progress,
// and returns the task as before.
func TestGetTaskNotifiesProgress(t *testing.T) {
	old := progressEvery
	progressEvery = 10 * time.Millisecond
	t.Cleanup(func() { progressEvery = old })
	h := newAgentHarness(t, testSender)
	// Stands in for the node's mesh route to the agent.
	proxy := httptest.NewServer(h.svc.handler("codex", "secret"))
	defer proxy.Close()
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	addA2ATools(server, &a2aTasks{http: &http.Client{Transport: meshHeaders{"secret", testSender}}, base: proxy.URL})
	notes := make(chan string, 8)
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) { notes <- r.Params.Message },
	})
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	target := map[string]any{"peer_id": testSender, "service": "codex"}
	sent, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "send_task", Arguments: merge(target, map[string]any{"text": "fix"})})
	if err != nil || sent.IsError {
		t.Fatalf("send: %+v %v", sent, err)
	}
	id := sent.StructuredContent.(map[string]any)["task_id"].(string)
	<-h.fix.prompts
	go func() {
		h.fix.steps <- "Bash: go test ./..."
		time.Sleep(1500 * time.Millisecond)
		h.fix.steps <- "Edit: a.go"
		time.Sleep(1500 * time.Millisecond)
		h.fix.release <- "fixed"
	}()
	params := &mcp.CallToolParams{Name: "get_task", Arguments: merge(target, map[string]any{"task_id": id, "wait_seconds": 10})}
	params.SetProgressToken("p1")
	got, err := session.CallTool(ctx, params)
	if err != nil || got.IsError {
		t.Fatalf("get: %+v %v", got, err)
	}
	if out := got.StructuredContent.(map[string]any); out["terminal"] != "completed" || out["result"] != "fixed" {
		t.Fatalf("get_task: %+v", out)
	}
	var seen []string
	for len(notes) > 0 {
		seen = append(seen, <-notes)
	}
	if want := []string{"Bash: go test ./...", "Edit: a.go"}; !slices.Equal(seen, want) {
		t.Fatalf("progress notifications %q, want %q", seen, want)
	}
}

func merge(a, b map[string]any) map[string]any {
	m := map[string]any{}
	for k, v := range a {
		m[k] = v
	}
	for k, v := range b {
		m[k] = v
	}
	return m
}
