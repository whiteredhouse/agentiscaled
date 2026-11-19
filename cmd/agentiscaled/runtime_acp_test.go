package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeACPAgent is an agent that speaks ACP on stdio, for the tests of the
// acp runtime. Its flags say what it declares; each prompt asks for a
// permission, reports a step and a plan, and then:
//   - with ASK-ME in it, asks a question
//   - with SLOW or runtime check's long task in it, waits for session/cancel
//   - else answers with what it saw: its environment, MCP servers, the
//     permission's outcome and whether the prompt asked for JSON.
func fakeACPAgent(args []string) {
	fs := flag.NewFlagSet("fake-acp", flag.ExitOnError)
	policies := fs.String("policies", "", "declared policies, comma-separated")
	status := fs.Bool("status", false, "end turns with _meta.agentiscale.status")
	load := fs.Bool("load", false, "declare loadSession")
	http := fs.Bool("http", false, "declare mcpCapabilities.http")
	noMeta := fs.Bool("no-meta", false, "declare no _meta.agentiscale")
	contract := fs.Int("contract", 1, "declared contract")
	authRequired := fs.Bool("auth-required", false, "refuse session/new as not signed in")
	ignoreCancel := fs.Bool("ignore-cancel", false, "never end a cancelled turn")
	fs.Parse(args)

	var mu sync.Mutex
	out := json.NewEncoder(os.Stdout)
	send := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		out.Encode(v)
	}
	update := func(session string, u map[string]any) {
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": u}})
	}
	chunk := func(session, text string) {
		update(session, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}})
	}
	var servers []string
	responses := map[int]chan json.RawMessage{}
	cancelled := make(chan struct{}, 1)
	nextID := 1000

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 16<<20)
	for in.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil {
			continue
		}
		reply := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result}) }
		switch msg.Method {
		case "":
			var id int
			json.Unmarshal(msg.ID, &id)
			mu.Lock()
			ch := responses[id]
			mu.Unlock()
			if ch != nil {
				ch <- msg.Result
			}
		case "initialize":
			res := map[string]any{
				"protocolVersion":   1,
				"agentInfo":         map[string]string{"name": "fake-acp", "version": "X.Y.Z"},
				"agentCapabilities": map[string]any{"loadSession": *load, "mcpCapabilities": map[string]bool{"http": *http}},
			}
			if !*noMeta {
				var p []string
				if *policies != "" {
					p = strings.Split(*policies, ",")
				}
				res["_meta"] = map[string]any{"agentiscale": map[string]any{"contract": *contract, "policies": p, "status": *status, "login": []string{"fake-acp", "login"}}}
			}
			reply(res)
		case "session/new", "session/load":
			if *authRequired {
				send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32000, "message": "Authentication required"}})
				continue
			}
			var p struct {
				SessionID  string `json:"sessionId"`
				MCPServers []struct {
					Name    string `json:"name"`
					URL     string `json:"url"`
					Headers []struct{ Name, Value string }
				} `json:"mcpServers"`
			}
			json.Unmarshal(msg.Params, &p)
			servers = nil
			for _, s := range p.MCPServers {
				auth := ""
				if len(s.Headers) > 0 {
					auth = s.Headers[0].Name + ":" + s.Headers[0].Value
				}
				servers = append(servers, s.Name+"@"+s.URL+"@"+auth)
			}
			if msg.Method == "session/new" {
				reply(map[string]string{"sessionId": "sess-1"})
				continue
			}
			// session/load replays the session's history first.
			chunk(p.SessionID, "OLD QUESTION")
			update(p.SessionID, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "old", "title": "old", "kind": "read", "locations": []map[string]string{{"path": "/old/replayed.go"}}})
			reply(nil)
		case "session/cancel":
			select {
			case cancelled <- struct{}{}:
			default:
			}
		case "session/prompt":
			var p struct {
				SessionID string `json:"sessionId"`
				Prompt    []struct{ Text string }
			}
			json.Unmarshal(msg.Params, &p)
			id := msg.ID
			go func() {
				prompt := p.Prompt[0].Text
				end := func(stop, st string) {
					res := map[string]any{"stopReason": stop}
					if *status && st != "" {
						res["_meta"] = map[string]any{"agentiscale": map[string]string{"status": st}}
					}
					send(map[string]any{"jsonrpc": "2.0", "id": id, "result": res})
				}
				answer := func(st, text string) {
					if *status {
						chunk(p.SessionID, text)
					} else {
						b, _ := json.Marshal(map[string]string{"status": st, "text": text})
						chunk(p.SessionID, string(b))
					}
					end("end_turn", st)
				}
				// A permission request, which the node must reject.
				mu.Lock()
				nextID++
				pid := nextID
				ch := make(chan json.RawMessage, 1)
				responses[pid] = ch
				mu.Unlock()
				send(map[string]any{"jsonrpc": "2.0", "id": pid, "method": "session/request_permission", "params": map[string]any{
					"sessionId": p.SessionID,
					"toolCall":  map[string]any{"toolCallId": "t1", "title": "mcp__agentiscale-network__send_task"},
					"options": []map[string]string{
						{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
						{"optionId": "no", "name": "Reject", "kind": "reject_once"},
					},
				}})
				var outcome struct {
					Outcome struct{ Outcome, OptionID string }
				}
				json.Unmarshal(<-ch, &outcome)
				update(p.SessionID, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t2", "title": "cat /secret/token-abc", "kind": "read", "locations": []map[string]string{{"path": "/secret/dir/main.go"}}})
				update(p.SessionID, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t3", "title": "curl https://x?q=secret", "kind": "execute", "rawInput": map[string]string{"command": "curl https://x?q=secret"}})
				update(p.SessionID, map[string]any{"sessionUpdate": "plan", "entries": []map[string]string{{"content": "secret plan", "status": "completed"}, {"content": "b", "status": "in_progress"}}})
				switch {
				case strings.Contains(prompt, "ASK-ME"):
					answer("needs_input", "Which color?")
				case strings.Contains(prompt, "SLOW"), strings.Contains(prompt, checkLongTask):
					<-cancelled
					if *ignoreCancel {
						select {}
					}
					end("cancelled", "")
				default:
					var env []string
					for _, kv := range os.Environ() {
						if strings.HasPrefix(kv, "AGENTISCALE_") && !strings.HasPrefix(kv, "AGENTISCALE_FAKE") {
							env = append(env, kv)
						}
					}
					slices.Sort(env)
					answer("done", fmt.Sprintf("env=%s mcp=%s perm=%s:%s json=%v role=%v answer=%v",
						strings.Join(env, ","), strings.Join(servers, ","), outcome.Outcome.Outcome, outcome.Outcome.OptionID,
						strings.Contains(prompt, `"needs_input"`), strings.Contains(prompt, "<role>"), strings.Contains(prompt, "<answer>")))
				}
			}()
		default:
			if len(msg.ID) > 0 {
				send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
			}
		}
	}
}

// fakeACPCommand is a command line that runs fakeACPAgent with flags,
// through a script so that the variable that selects it survives an
// environment the runtime narrows.
func fakeACPCommand(t *testing.T, flags ...string) string {
	t.Helper()
	bin := t.TempDir()
	script := filepath.Join(bin, "fake-acp")
	body := fmt.Sprintf("#!/bin/sh\nAGENTISCALE_FAKE_ACP=1 exec %s \"$@\"\n", os.Args[0])
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return strings.Join(append([]string{script}, flags...), " ")
}

func newFakeACP(t *testing.T, policy agentPolicy, flags ...string) *acpRuntime {
	t.Helper()
	rt, err := newACPRuntime(fakeACPCommand(t, flags...), t.TempDir(), policy)
	if err != nil {
		t.Fatal(err)
	}
	return rt.(*acpRuntime)
}

var ownerPolicy = agentPolicy{DirInstructions: true, DirSkills: true, OwnerConfig: true}

func TestACPRuntimeTurn(t *testing.T) {
	for _, status := range []bool{true, false} {
		t.Run(fmt.Sprintf("status=%v", status), func(t *testing.T) {
			flags := []string{"-http", "-load"}
			if status {
				flags = append(flags, "-status")
			}
			r := newFakeACP(t, ownerPolicy, flags...)
			r.setDefinition(&agentDefinition{Instructions: "You review code."})
			var steps []string
			res, err := r.Run(context.Background(), t.TempDir(), taskRequest{
				Sender: "peer", Text: "do it",
				Progress: func(s string) { steps = append(steps, s) },
				Network:  &networkAccess{URL: "http://127.0.0.1:1/mcp", Token: "tok"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Question || res.Session != "sess-1" {
				t.Fatalf("result %+v", res)
			}
			for _, want := range []string{
				"env=AGENTISCALE_CONTRACT=1,AGENTISCALE_ENV=1 ",
				"mcp=agentiscale-network@http://127.0.0.1:1/mcp@Authorization:Bearer tok ",
				"perm=selected:no ",
				fmt.Sprintf("json=%v ", !status),
				"role=true",
				"answer=false",
			} {
				if !strings.Contains(res.Text, want) {
					t.Errorf("result %q lacks %q", res.Text, want)
				}
			}
			// Steps are fixed summaries, never the agent's titles,
			// commands or plan text.
			if want := []string{"Reading main.go", "Running a command", "Working on step 2 of 2"}; !slices.Equal(steps, want) {
				t.Errorf("steps %q, want %q", steps, want)
			}
		})
	}
}

func TestACPRuntimeNoNetworkWithoutHTTP(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status")
	res, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x", Network: &networkAccess{URL: "http://127.0.0.1:1/mcp", Token: "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "mcp= ") {
		t.Errorf("an agent without mcpCapabilities.http got the network: %q", res.Text)
	}
}

func TestACPRuntimeQuestionAndAnswer(t *testing.T) {
	for _, status := range []bool{true, false} {
		t.Run(fmt.Sprintf("status=%v", status), func(t *testing.T) {
			flags := []string{"-load"}
			if status {
				flags = append(flags, "-status")
			}
			r := newFakeACP(t, ownerPolicy, flags...)
			r.setDefinition(&agentDefinition{Instructions: "role"})
			dir := t.TempDir()
			q, err := r.Run(context.Background(), dir, taskRequest{Text: "ASK-ME"})
			if err != nil {
				t.Fatal(err)
			}
			if !q.Question || q.Text != "Which color?" || q.Session != "sess-1" {
				t.Fatalf("question %+v", q)
			}
			var steps []string
			a, err := r.Run(context.Background(), dir, taskRequest{Text: "blue", Session: q.Session, Progress: func(s string) { steps = append(steps, s) }})
			if err != nil {
				t.Fatal(err)
			}
			// The replayed history is not the turn's: no OLD QUESTION in
			// the result, no step of the replayed tool call.
			if a.Question || strings.Contains(a.Text, "OLD QUESTION") || !strings.Contains(a.Text, "answer=true") || !strings.Contains(a.Text, "role=false") {
				t.Errorf("answer %+v", a)
			}
			if slices.Contains(steps, "Reading replayed.go") {
				t.Errorf("steps include the replayed history: %q", steps)
			}
		})
	}
}

func TestACPRuntimeQuestionWithoutLoadSession(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status")
	if _, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "ASK-ME"}); err == nil || !strings.Contains(err.Error(), "cannot load a session") {
		t.Errorf("question from an agent without loadSession: %v", err)
	}
	if _, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x", Session: "sess-1"}); err == nil {
		t.Error("an answer to an agent without loadSession should fail")
	}
}

func TestACPRuntimePolicies(t *testing.T) {
	data := t.TempDir()
	restricted := agentPolicy{}
	if _, err := newACPRuntime(fakeACPCommand(t, "-policies", "config-dir,dir-instructions"), data, restricted); err == nil || !strings.Contains(err.Error(), "-dir-skills=false") || strings.Contains(err.Error(), "-owner-config") {
		t.Errorf("an undeclared policy: %v", err)
	}
	if _, err := newACPRuntime(fakeACPCommand(t, "-no-meta"), data, agentPolicy{DirSkills: true, OwnerConfig: true}); err == nil || !strings.Contains(err.Error(), "-dir-instructions=false") {
		t.Errorf("a plain ACP agent with a policy: %v", err)
	}
	rt, err := newACPRuntime(fakeACPCommand(t, "-policies", "config-dir,dir-instructions,dir-skills", "-status"), data, restricted)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "owner-secret")
	res, err := rt.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(data, "acp", "fake-acp")
	want := "env=AGENTISCALE_CONFIG_DIR=" + config + ",AGENTISCALE_CONTRACT=1,AGENTISCALE_DIR_INSTRUCTIONS=0,AGENTISCALE_DIR_SKILLS=0,AGENTISCALE_ENV=1 "
	if !strings.Contains(res.Text, want) {
		t.Errorf("result %q lacks %q", res.Text, want)
	}
	if fi, err := os.Stat(config); err != nil || !fi.IsDir() {
		t.Errorf("config dir: %v", err)
	}
}

func TestACPRuntimeRefusesNewerContract(t *testing.T) {
	if _, err := newACPRuntime(fakeACPCommand(t, "-contract", "2"), t.TempDir(), ownerPolicy); err == nil || !strings.Contains(err.Error(), "update agentiscaled") {
		t.Errorf("newer contract: %v", err)
	}
}

func TestACPRuntimeNotSignedIn(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-auth-required")
	_, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "not signed in: run\n  fake-acp login") {
		t.Errorf("not signed in: %v", err)
	}
}

func TestACPRuntimeCancel(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignore=%v", ignore), func(t *testing.T) {
			flags := []string{"-status"}
			if ignore {
				flags = append(flags, "-ignore-cancel")
			}
			r := newFakeACP(t, ownerPolicy, flags...)
			var stop string
			r.stopped = func(s string) { stop = s }
			// The turn is cancelled once it runs: at its first step.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var start time.Time
			var once sync.Once
			_, err := r.Run(ctx, t.TempDir(), taskRequest{Text: "SLOW", Progress: func(string) {
				once.Do(func() { start = time.Now(); cancel() })
			}})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled turn: %v", err)
			}
			took := time.Since(start)
			if ignore {
				if stop != "" || took < acpCancelGrace {
					t.Errorf("an agent that ignores cancel: stop %q after %s", stop, took)
				}
			} else if stop != "cancelled" || took > 3*time.Second {
				t.Errorf("stop %q after %s", stop, took)
			}
		})
	}
}

func TestRuntimeCheck(t *testing.T) {
	old := checkCancelAfter
	checkCancelAfter = 2 * time.Second
	t.Cleanup(func() { checkCancelAfter = old })
	var out bytes.Buffer
	err := runRuntimeCheck(context.Background(), []string{"-command", fakeACPCommand(t, "-status", "-http", "-policies", "config-dir")}, &out)
	if err != nil {
		t.Fatalf("check: %v\n%s", err, out.String())
	}
	// Without loadSession the question is skipped.
	for _, want := range []string{"PASS  initialize", "PASS  conventions", "PASS  network", "PASS  turn", "SKIP  question", "PASS  cancel"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestACPCommandLine(t *testing.T) {
	line := fakeACPCommand(t, "-status")
	resolved, err := resolveACPCommand(line)
	if err != nil || resolved != line {
		t.Errorf("resolve %q: %q, %v", line, resolved, err)
	}
	if _, err := resolveACPCommand("no-such-agent-xyz acp"); err == nil {
		t.Error("a missing program should not resolve")
	}
	if got := runtimeDefaultName("acp", "/opt/bin/My-Agent --acp"); got != "my-agent" {
		t.Errorf("default name %q", got)
	}
	if got := runtimeDefaultName("codex", "/x/y"); got != "codex" {
		t.Errorf("codex default name %q", got)
	}
}
