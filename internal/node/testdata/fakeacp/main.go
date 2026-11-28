package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/whiteredhouse/agentiscaled/internal/checkprompt"
)

// The runtime check's prompts.
const (
	checkDoneTask     = checkprompt.Done
	checkQuestionTask = checkprompt.Question
	checkAnswer       = checkprompt.Answer
	checkLongTask     = checkprompt.Long
)

// The fake ACP agent for internal/node's acp runtime tests.
func main() { fakeACPAgent(os.Args[1:]) }

// fakeACPAgent is an agent that speaks ACP on stdio, for the tests of the
// acp runtime. Its flags say what it declares; each prompt asks for a
// permission, reports a step and a plan, and then:
//   - with ASK-ME or runtime check's question task in it, asks a question
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
	checkMode := fs.String("check-mode", "", "behavior to exercise during runtime check")
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
	prompts := 0

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
				res["_meta"] = map[string]any{"agentiscale": map[string]any{"contract": *contract, "policies": p, "status": *status, "login": []string{"/opt/My Agent/fake-acp", "login", "--account", "Jo's team"}}}
			}
			reply(res)
		case "session/new", "session/load":
			if *checkMode == "slow-start" {
				time.Sleep(300 * time.Millisecond)
			}
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
					Command string   `json:"command"`
					Args    []string `json:"args"`
					Env     []struct{ Name, Value string }
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
				if s.Command != "" {
					var env []string
					for _, e := range s.Env {
						env = append(env, e.Name+"="+e.Value)
					}
					servers = append(servers, s.Name+"@stdio:"+s.Command+" "+strings.Join(s.Args, " ")+"@"+strings.Join(env, ";"))
					continue
				}
				auth := ""
				if len(s.Headers) > 0 {
					auth = s.Headers[0].Name + ":" + s.Headers[0].Value
				}
				servers = append(servers, s.Name+"@"+s.URL+"@"+auth)
			}
			if msg.Method == "session/new" {
				reply(map[string]string{"sessionId": "sess-1"})
				if *stopReading {
					forever()
				}
				if *closeStdin {
					os.Stdin.Close()
					forever()
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
			prompts++
			promptsSoFar := prompts
			go func() {
				prompt := p.Prompt[0].Text
				if *hang {
					forever()
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
				update(p.SessionID, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t2", "title": "cat /secret/token-abc", "kind": "read", "content": []any{}, "locations": []map[string]string{{"path": "/secret/dir/main.go"}}})
				update(p.SessionID, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t3", "title": "curl https://x?q=secret", "kind": "execute", "rawInput": map[string]string{"command": "curl https://x?q=secret"}})
				update(p.SessionID, map[string]any{"sessionUpdate": "plan", "entries": []map[string]string{{"content": "secret plan", "status": "completed"}, {"content": "b", "status": "in_progress"}}})
				if *big {
					chunk(p.SessionID, strings.Repeat("a", 700<<10))
					chunk(p.SessionID, strings.Repeat("b", 700<<10))
				}
				switch {
				case strings.Contains(prompt, checkDoneTask):
					answer("done", "Ready.")
				case strings.Contains(prompt, "<answer>\n"+checkAnswer+"\n</answer>"):
					switch *checkMode {
					case "empty-answer":
						answer("done", "")
					case "lost-history":
						answer("done", "The answer is blue.")
					default:
						answer("done", "Blue, and the original word was Lantern.")
					}
				case strings.Contains(prompt, "ASK-ME"), strings.Contains(prompt, checkQuestionTask):
					if *checkMode == "exit-after-question" {
						if err := os.WriteFile("asking.pid", []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
							panic(err)
						}
					}
					answer("needs_input", "Which color?")
					if *checkMode == "exit-after-question" {
						os.Exit(0)
					}
				case strings.Contains(prompt, "SLOW"), strings.Contains(prompt, checkLongTask):
					switch *checkMode {
					case "prompt-error":
						send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "prompt failed"}})
						return
					case "quick-result":
						answer("done", "100000")
						return
					}
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
						forever()
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
					answer("done", fmt.Sprintf("prompts=%d env=%s mcp=%s perm=%s:%s json=%v role=%v answer=%v", promptsSoFar,
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

// forever blocks the way a stuck agent does. A bare select{} would not:
// with every goroutine blocked, the runtime ends this small program as
// deadlocked, where the agent it stands in for would keep running.
func forever() {
	time.Sleep(time.Duration(math.MaxInt64))
}
