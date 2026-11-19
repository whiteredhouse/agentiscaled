package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
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
	rejectAlwaysOnly := fs.Bool("reject-always-only", false, "offer only allow_once and reject_always")
	big := fs.Bool("big", false, "send a result over the size limit, then a small chunk")
	fail := fs.Bool("fail", false, "answer session/prompt with an error, and log a secret to stderr")
	stopReading := fs.Bool("stop-reading", false, "read nothing after session/new")
	hang := fs.Bool("hang", false, "never answer session/prompt")
	closeStdin := fs.Bool("close-stdin", false, "close stdin after session/new, keeping stdout open")
	badStatus := fs.Bool("bad-status", false, "end turns with a status that is not done or needs_input")
	badStop := fs.Bool("bad-stop", false, "end turns with a token-shaped stop reason")
	permAfterCancel := fs.Bool("perm-after-cancel", false, "on cancel, ask for a permission and log its outcome to stderr")
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
				Cwd        string `json:"cwd"`
				MCPServers []struct {
					Name    string `json:"name"`
					URL     string `json:"url"`
					Headers []struct{ Name, Value string }
				} `json:"mcpServers"`
			}
			json.Unmarshal(msg.Params, &p)
			// ACP takes only an absolute working directory.
			if !filepath.IsAbs(p.Cwd) {
				send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32602, "message": "cwd must be absolute"}})
				continue
			}
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
				if *stopReading {
					select {}
				}
				if *closeStdin {
					os.Stdin.Close()
					select {}
				}
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
				if *hang {
					select {}
				}
				if *fail {
					fmt.Fprintln(os.Stderr, "Authorization: Bearer SECRET-TOKEN")
					send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "boom SECRET-TOKEN"}})
					return
				}
				end := func(stop, st string) {
					if *badStop {
						stop = "tok_SECRET123"
					}
					if *badStatus {
						st = "weird SECRET"
					}
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
					"options": func() []map[string]string {
						if *rejectAlwaysOnly {
							return []map[string]string{{"optionId": "yes", "name": "Allow", "kind": "allow_once"}, {"optionId": "never", "name": "Never", "kind": "reject_always"}}
						}
						return []map[string]string{{"optionId": "yes", "name": "Allow", "kind": "allow_once"}, {"optionId": "no", "name": "Reject", "kind": "reject_once"}}
					}(),
				}})
				var outcome struct {
					Outcome struct{ Outcome, OptionID string }
				}
				json.Unmarshal(<-ch, &outcome)
				update(p.SessionID, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t2", "title": "cat /secret/token-abc", "kind": "read", "locations": []map[string]string{{"path": "/secret/dir/main.go"}}})
				update(p.SessionID, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t3", "title": "curl https://x?q=secret", "kind": "execute", "rawInput": map[string]string{"command": "curl https://x?q=secret"}})
				update(p.SessionID, map[string]any{"sessionUpdate": "plan", "entries": []map[string]string{{"content": "secret plan", "status": "completed"}, {"content": "b", "status": "in_progress"}}})
				if *big {
					chunk(p.SessionID, strings.Repeat("a", 700<<10))
					chunk(p.SessionID, strings.Repeat("b", 700<<10))
				}
				switch {
				case strings.Contains(prompt, "ASK-ME"):
					answer("needs_input", "Which color?")
				case strings.Contains(prompt, "SLOW"), strings.Contains(prompt, checkLongTask):
					<-cancelled
					if *permAfterCancel {
						mu.Lock()
						nextID++
						pid := nextID
						ch := make(chan json.RawMessage, 1)
						responses[pid] = ch
						mu.Unlock()
						send(map[string]any{"jsonrpc": "2.0", "id": pid, "method": "session/request_permission", "params": map[string]any{
							"sessionId": p.SessionID, "toolCall": map[string]any{"toolCallId": "t9"},
							"options": []map[string]string{{"optionId": "no", "name": "Reject", "kind": "reject_once"}},
						}})
						fmt.Fprintf(os.Stderr, "perm-after-cancel=%s\n", <-ch)
					}
					if *ignoreCancel {
						select {}
					}
					end("cancelled", "")
				default:
					var env []string
					for _, kv := range os.Environ() {
						// Only the contract's variables: CI sets others.
						name, _, _ := strings.Cut(kv, "=")
						if slices.Contains([]string{"AGENTISCALE_ENV", "AGENTISCALE_CONTRACT", "AGENTISCALE_CONFIG_DIR", "AGENTISCALE_DIR_INSTRUCTIONS", "AGENTISCALE_DIR_SKILLS"}, name) {
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

// newACPFromLine makes the acp runtime from a -command line.
func newACPFromLine(line, dataDir string, policy agentPolicy) (runtime, error) {
	program, args, err := splitACPCommand(line)
	if err != nil {
		return nil, err
	}
	return newACPRuntime(program, args, dataDir, policy)
}

func newFakeACP(t *testing.T, policy agentPolicy, flags ...string) *acpRuntime {
	t.Helper()
	rt, err := newACPFromLine(fakeACPCommand(t, flags...), t.TempDir(), policy)
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
	if _, err := newACPFromLine(fakeACPCommand(t, "-policies", "config-dir,dir-instructions"), data, restricted); err == nil || !strings.Contains(err.Error(), "-dir-skills=false") || strings.Contains(err.Error(), "-owner-config") {
		t.Errorf("an undeclared policy: %v", err)
	}
	if _, err := newACPFromLine(fakeACPCommand(t, "-no-meta"), data, agentPolicy{DirSkills: true, OwnerConfig: true}); err == nil || !strings.Contains(err.Error(), "-dir-instructions=false") {
		t.Errorf("a plain ACP agent with a policy: %v", err)
	}
	rt, err := newACPFromLine(fakeACPCommand(t, "-policies", "config-dir,dir-instructions,dir-skills", "-status"), data, restricted)
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
	if _, err := newACPFromLine(fakeACPCommand(t, "-contract", "2"), t.TempDir(), ownerPolicy); err == nil || !strings.Contains(err.Error(), "update agentiscaled") {
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
	program, args, err := splitACPCommand("  my-agent  acp --x ")
	if err != nil || program != "my-agent" || !slices.Equal(args, []string{"acp", "--x"}) {
		t.Errorf("split: %q %q %v", program, args, err)
	}
	if _, _, err := splitACPCommand(" "); err == nil {
		t.Error("an empty command should not split")
	}
	if got := runtimeDefaultName("acp", "/opt/My Apps/My-Agent"); got != "my-agent" {
		t.Errorf("default name %q", got)
	}
	if got := runtimeDefaultName("codex", "/x/y"); got != "codex" {
		t.Errorf("codex default name %q", got)
	}
	// The program's path may hold spaces once it is resolved: it stays
	// apart from the arguments.
	line := fakeACPCommand(t, "-status")
	script, flags, _ := splitACPCommand(line)
	dir := filepath.Join(t.TempDir(), "my apps")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(script, filepath.Join(dir, "fake-acp")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	a := agentConfig{Runtime: "acp", Command: "fake-acp " + strings.Join(flags, " ")}
	if err := a.splitCommand(); err != nil {
		t.Fatal(err)
	}
	if err := checkAgent(&a, t.TempDir(), false); err == nil {
		t.Error("checkAgent without a directory should fail")
	}
	a.Dir = t.TempDir()
	if err := checkAgent(&a, t.TempDir(), false); err != nil {
		t.Fatalf("checkAgent: %v", err)
	}
	if a.Command != filepath.Join(dir, "fake-acp") || !slices.Equal(a.Args, flags) || a.Name != "fake-acp" {
		t.Errorf("checked agent: %q %q %q", a.Command, a.Args, a.Name)
	}
	if _, err := a.newRuntime(t.TempDir()); err != nil {
		t.Errorf("runtime from a path with a space: %v", err)
	}
}

func TestACPRuntimeRejectsOnlyOnce(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status", "-reject-always-only")
	res, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "perm=cancelled: ") {
		t.Errorf("with only reject_always the request should be cancelled: %q", res.Text)
	}
}

func TestACPRuntimeResultOverLimit(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status", "-big")
	if _, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"}); err == nil || !strings.Contains(err.Error(), "longer than the limit") {
		t.Errorf("a result over the limit: %v", err)
	}
}

func TestACPRuntimeFailureKeepsDiagnosticsLocal(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	r := newFakeACP(t, ownerPolicy, "-status", "-fail")
	_, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"})
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "session/prompt answered with error -32603") {
		t.Errorf("the caller's error: %v", err)
	}
	if !strings.Contains(logged.String(), "SECRET-TOKEN") {
		t.Errorf("the node's log lacks the agent's diagnostics: %q", logged.String())
	}
}

func TestACPRuntimeCancelWhenAgentStopsReading(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status", "-stop-reading")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		// A prompt bigger than a pipe holds, which the agent never reads.
		_, err := r.Run(ctx, t.TempDir(), taskRequest{Text: strings.Repeat("x", 1<<20)})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("turn: %v", err)
		}
	case <-time.After(time.Second + acpCancelGrace + 10*time.Second):
		t.Fatalf("the turn did not end %s after it was cancelled", time.Since(start))
	}
}

func TestRuntimeCheckTimesOut(t *testing.T) {
	old := checkCancelAfter
	checkCancelAfter = 200 * time.Millisecond
	t.Cleanup(func() { checkCancelAfter = old })
	var out bytes.Buffer
	err := runRuntimeCheck(context.Background(), []string{"-timeout", "1s", "-command", fakeACPCommand(t, "-status", "-hang")}, &out)
	if err == nil || !strings.Contains(out.String(), "FAIL  turn: no answer within 1s (-timeout)") {
		t.Errorf("check of an agent that never answers: %v\n%s", err, out.String())
	}
}

func TestACPRuntimeStdinClosed(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status", "-close-stdin")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, t.TempDir(), taskRequest{Text: strings.Repeat("x", 1<<20)})
	if err == nil || ctx.Err() != nil || time.Since(start) > 10*time.Second {
		t.Errorf("a turn whose agent closed stdin: %v after %s", err, time.Since(start))
	}
}

func TestACPRuntimeFixedTurnReasons(t *testing.T) {
	for flag, want := range map[string]string{
		"-bad-status": "not done or needs_input",
		"-bad-stop":   "the agent stopped the turn: an unknown reason",
	} {
		r := newFakeACP(t, ownerPolicy, "-status", flag)
		_, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"})
		if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", flag, err)
		}
	}
}

func TestACPRuntimeRelativeCommand(t *testing.T) {
	script, flags, _ := splitACPCommand(fakeACPCommand(t, "-status"))
	t.Chdir(filepath.Dir(script))
	rt, err := newACPRuntime("./"+filepath.Base(script), flags, t.TempDir(), ownerPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if r := rt.(*acpRuntime); r.command != script {
		t.Errorf("command %q, want %q", r.command, script)
	}
	if _, err := rt.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"}); err != nil {
		t.Errorf("run from another directory: %v", err)
	}
}

func TestACPRuntimeCancelsPendingPermissions(t *testing.T) {
	// The agent asks for a permission after the cancel and then ignores
	// the cancel, so the node stops it.
	r := newFakeACP(t, ownerPolicy, "-status", "-perm-after-cancel", "-ignore-cancel")
	var stderr string
	r.stderr = func(s string) { stderr = s }
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	r.Run(ctx, t.TempDir(), taskRequest{Text: "SLOW", Progress: func(string) { once.Do(cancel) }})
	if !strings.Contains(stderr, `perm-after-cancel={"outcome":{"outcome":"cancelled"}}`) {
		t.Errorf("a permission asked after the cancel: %q", stderr)
	}
}

func TestACPSendDropsCancelledWrites(t *testing.T) {
	var written bytes.Buffer
	var mu sync.Mutex
	s := &acpSession{out: make(chan acpWrite), dead: make(chan struct{}), broken: make(chan struct{}), done: make(chan struct{}),
		stdin: nopCloser{writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return written.Write(p) })}}
	go s.write()
	defer close(s.done)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 64 {
		if err := s.send(ctx, map[string]string{"method": "session/prompt"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("send with a cancelled context: %v", err)
		}
	}
	if err := s.send(context.Background(), map[string]string{"method": "ok"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if got := written.String(); got != "{\"method\":\"ok\"}\n" {
		t.Errorf("written %q", got)
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func TestRuntimeCheckRelativeDir(t *testing.T) {
	old := checkCancelAfter
	checkCancelAfter = 2 * time.Second
	t.Cleanup(func() { checkCancelAfter = old })
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	if err := runRuntimeCheck(context.Background(), []string{"-dir", ".", "-command", fakeACPCommand(t, "-status")}, &out); err != nil {
		t.Errorf("check with -dir .: %v\n%s", err, out.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
