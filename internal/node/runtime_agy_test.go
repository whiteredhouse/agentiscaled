package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeAgy writes an agy script that records its arguments, its
// environment, its stdin and the network folder's MCP configuration next to
// itself, reports one step as it starts and ends, and prints result.
func fakeAgy(t *testing.T, dir, result string) (command, record string) {
	t.Helper()
	record = filepath.Join(dir, "rec")
	command = filepath.Join(dir, "agy")
	script := `#!/bin/sh
if [ "$1" = --version ]; then echo "agy 1.5.19"; exit 0; fi
printf '%s\0' "$@" > ` + record + `.args
env > ` + record + `.env
pwd > ` + record + `.pwd
cat > ` + record + `.stdin
prev=
for a in "$@"; do
  if [ "$prev" = --add-dir ]; then cat "$a/.agents/mcp_config.json" > ` + record + `.mcp; cat "$a/ticket" > ` + record + `.ticket; fi
  if [ "$prev" = --settings ]; then cat "$a" > ` + record + `.settings; fi
  prev=$a
done
echo '{"event":"init","conversation_id":"6f0b3a52-41a7-4c63-9a0e-3c1f5d2b7e11"}'
echo '{"event":"step_update","step_update":{"step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"read_file","tool_info":{"name":"read_file","parameters":{"Path":"/work/main.go"}}}}'
echo '{"event":"step_update","step_update":{"step_index":2,"state":"DONE","step_type":"tool","tool_name":"read_file","tool_info":{"name":"read_file","parameters":{"Path":"/work/main.go"},"output":"1 lines"}}}'
cat <<'EOF'
` + result + `
EOF
exit ${FAKE_AGY_EXIT:-0}
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return command, record
}

func agyArgs(t *testing.T, record string) []string {
	return strings.Split(strings.TrimSuffix(readRecord(t, record+".args"), "\x00"), "\x00")
}

// argAfter is the argument after flag in args, if any.
func argAfter(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

func TestAgyRuntimeRunsATurn(t *testing.T) {
	dir := t.TempDir()
	command, record := fakeAgy(t, dir, `{"event":"result","result":{"conversation_id":"6f0b3a52-41a7-4c63-9a0e-3c1f5d2b7e11","status":"SUCCESS","response":"","structured_output":{"status":"done","text":" No findings "}}}`)
	var steps []string
	out, err := (&agyRuntime{command: command, bridge: "/bin/agentiscaled"}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "Review main.go",
		Progress: func(step string) { steps = append(steps, step) }})
	if err != nil || out.Text != "No findings" || out.Question || out.Session != "6f0b3a52-41a7-4c63-9a0e-3c1f5d2b7e11" {
		t.Fatalf("run: %+v, %v", out, err)
	}
	if !slices.Equal(steps, []string{"Reading main.go"}) {
		t.Errorf("steps: %q", steps)
	}
	args := agyArgs(t, record)
	if p := readRecord(t, record+".stdin"); !strings.Contains(p, "Review main.go") || p != (&agyRuntime{}).prompt(taskRequest{Sender: testSender, Text: "Review main.go"}) {
		t.Errorf("prompt on stdin %q", p)
	}
	if slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, "Review main.go") }) {
		t.Errorf("the prompt is in agy's arguments: %q", args)
	}
	for flag, want := range map[string]string{"--output-format": "stream-json", "--json-schema": turnSchema, "--print-timeout": "0"} {
		if got, _ := argAfter(args, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	for _, flag := range []string{"-p", "--conversation", "--add-dir", "--settings", "--dangerously-skip-permissions"} {
		if slices.Contains(args, flag) {
			t.Errorf("a first turn without the network has %s", flag)
		}
	}
	if pwd := strings.TrimSpace(readRecord(t, record+".pwd")); pwd != dir {
		if real, _ := filepath.EvalSymlinks(dir); pwd != real {
			t.Errorf("ran in %s, want %s", pwd, dir)
		}
	}
}

// The caller's answer resumes the turn's conversation.
func TestAgyRuntimeResumesTheConversation(t *testing.T) {
	dir := t.TempDir()
	command, record := fakeAgy(t, dir, `{"event":"result","result":{"status":"SUCCESS","structured_output":{"status":"needs_input","text":"Which branch?"}}}`)
	session := "0c7e4f8a-3d2b-4f6e-9a1c-5b8d7e6f4a3b"
	out, err := (&agyRuntime{command: command}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "main", Session: session})
	if err != nil || !out.Question || out.Text != "Which branch?" || out.Session != session {
		t.Fatalf("run: %+v, %v", out, err)
	}
	if got, _ := argAfter(agyArgs(t, record), "--conversation"); got != session {
		t.Errorf("--conversation %q, want %q", got, session)
	}
	if _, err := (&agyRuntime{command: command}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x", Session: "--add-dir"}); err == nil {
		t.Error("a session that is not a UUID was passed to agy")
	}
}

// A turn that agy ended with no answer, because it denied a step nobody
// could approve, fails; so does one that did not succeed.
func TestAgyRuntimeFailures(t *testing.T) {
	for name, tc := range map[string]struct{ result, want string }{
		"denied":  {`{"event":"result","result":{"status":"SUCCESS","response":"","denied_actions":[{"action":"mcp","display_name":"McpTool"},{"action":"mcp"}]}}`, "approval nobody can give on this node (mcp)"},
		"empty":   {`{"event":"result","result":{"status":"SUCCESS","response":"  "}}`, errNoAnswer.Error()},
		"failed":  {`{"event":"result","result":{"status":"ERROR","response":"","error":"invalid model selection","finished_reason":"error"}}`, "agy ended with ERROR: invalid model selection"},
		"timeout": {`{"event":"result","result":{"status":"ERROR","response":"","error":"print timeout after 1s with the turn still in progress","finished_reason":"print_timeout"}}`, "agy ended with ERROR (print_timeout): print timeout after 1s"},
		"oddname": {`{"event":"result","result":{"status":"bad status!","response":""}}`, "agy ended with an error"},
		"none":    {`{"event":"step_update"}`, "agy: no result"},
		"exit 1":  {`{"event":"step_update"} exit1`, "agy: exit status 1"},
		"exit 3":  {`{"event":"result","result":{"status":"ERROR","error":"print timeout","finished_reason":"print_timeout"}} exit3`, "agy ended with ERROR (print_timeout): print timeout"},
		"late":    {`{"event":"result","result":{"status":"SUCCESS","structured_output":{"status":"done","text":"ok"}}} exit3`, "agy exited with exit status 3 after its result"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			result, code, _ := strings.Cut(tc.result, " exit")
			t.Setenv("FAKE_AGY_EXIT", code)
			command, _ := fakeAgy(t, dir, result)
			_, err := (&agyRuntime{command: command}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v, want %q", err, tc.want)
			}
		})
	}
}

// A plain response is the result as it stands; a JSON one in turnSchema is
// read as an answer.
func TestAgyRuntimeReadsAPlainResponse(t *testing.T) {
	for response, want := range map[string]turnResult{
		"Done.":                                  {Text: "Done.", Session: "s"},
		`{"status":"needs_input","text":"Who?"}`: {Text: "Who?", Question: true, Session: "s"},
	} {
		b, _ := json.Marshal(map[string]any{"event": "result", "status": "SUCCESS", "response": response})
		got, err := agyResult(b, "s", nil, "")
		if err != nil || got != want {
			t.Errorf("response %q: %+v, %v", response, got, err)
		}
	}
}

// A definition's instructions come in front of a new task only.
func TestAgyRuntimeDefinition(t *testing.T) {
	r := &agyRuntime{def: &agentDefinition{Instructions: "You review Go code."}}
	if p := r.prompt(taskRequest{Text: "Review main.go"}); !strings.HasPrefix(p, "<role>\nYou review Go code.\n</role>\n\n") {
		t.Errorf("new task prompt %q", p)
	}
	if p := r.prompt(taskRequest{Text: "main", Session: "s"}); strings.Contains(p, "<role>") {
		t.Errorf("answer prompt %q has the role again", p)
	}
}

func TestNewAgyRuntimeRefusesWhatItCannotEnforce(t *testing.T) {
	dir := t.TempDir()
	command, _ := fakeAgy(t, dir, "")
	all := agentPolicy{DirInstructions: true, DirSkills: true, OwnerConfig: true}
	if _, err := newAgyRuntime(command, "", all); err != nil {
		t.Fatalf("owner configuration: %v", err)
	}
	for name, p := range map[string]agentPolicy{
		"own config":      {DirInstructions: true, DirSkills: true},
		"no instructions": {DirSkills: true, OwnerConfig: true},
		"no skills":       {DirInstructions: true, OwnerConfig: true},
	} {
		if _, err := newAgyRuntime(command, "", p); err == nil {
			t.Errorf("%s: agy accepted a policy it cannot enforce", name)
		}
	}
}

func TestAgyProgress(t *testing.T) {
	tool := func(name, params string) string {
		return `{"event":"step_update","step_update":{"step_index":7,"state":"ACTIVE","step_type":"tool","tool_name":"` + name + `","tool_info":{"name":"` + name + `","parameters":` + params + `}}}`
	}
	for event, want := range map[string]string{
		tool("read_file", `{"Path":"/home/u/secret dir/main.go"}`):                                                            "Reading main.go",
		tool("write_to_file", `{"TargetFile":"/x/notes.md"}`):                                                                 "Editing notes.md",
		tool("multi_replace_file_content", `{"TargetFile":"/x/a.go"}`):                                                        "Editing a.go",
		tool("run_command", `{"CommandLine":"TOKEN=abc go test ./..."}`):                                                      "Running go",
		tool("run_command", `{}`):                                                                                             "Running a command",
		tool("call_mcp_tool", `{"ServerName":"agentiscale-network","ToolName":"send_task","Arguments":{"message":"secret"}}`): "Using agentiscale-network.send_task",
		tool("view_file", `{"AbsolutePath":"/Users/u/.gemini/antigravity-cli/mcp/agentiscale-network/list_agents.json"}`):     "",
		tool("grep_search", `{}`):      "Searching",
		tool("read_url_content", `{}`): "Fetching a page",
		`{"event":"step_update","step_update":{"step_index":3,"state":"DONE","step_type":"agent_response","text_delta":"secret"}}`: "",
		`{"event":"result","result":{"status":"SUCCESS"}}`:                                                                         "",
		`not json`: "",
	} {
		step, got := agyProgress([]byte(event))
		if got != want || got != "" && step != 7 {
			t.Errorf("agyProgress(%s) = %d, %q, want %q", event, step, got, want)
		}
	}
}

// With the network, agy starts the bridge from a folder of the node's that
// holds a one-time ticket, never the run's bearer, and may call the
// network's tools without asking.
func TestAgyRuntimeNetwork(t *testing.T) {
	dir := t.TempDir()
	command, record := fakeAgy(t, dir, `{"event":"result","result":{"status":"SUCCESS","structured_output":{"status":"done","text":"ok"}}}`)
	home := t.TempDir()
	t.Setenv("HOME", home)
	settingsDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := `{"trustedWorkspaces":["/w"],"permissions":{"allow":["command(go test)"],"deny":["mcp(other/*)"]}}`
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(owner), 0o600); err != nil {
		t.Fatal(err)
	}
	network, _ := fakeNodeNetworkServer(t)
	g, err := network.grantFor("caller", "t-agy")
	if err != nil {
		t.Fatal(err)
	}
	defer g.begin(context.Background(), "turn-token", time.Now().Add(time.Minute))()
	access := &networkAccess{URL: network.url, Token: g.bearer, ticket: func() (string, func(), error) { return network.ticket(g) }}
	t.Setenv(networkTokenEnv, "leaked")
	if _, err := (&agyRuntime{command: command, bridge: "/opt/agentiscaled"}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x", Network: access}); err != nil {
		t.Fatal(err)
	}
	args := agyArgs(t, record)
	added, ok := argAfter(args, "--add-dir")
	if !ok || strings.HasPrefix(added, dir) {
		t.Fatalf("--add-dir %q, want a folder outside the agent's directory", added)
	}
	if _, err := os.Stat(added); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the network folder %s outlived the turn: %v", added, err)
	}
	// The run's settings take the place of the owner's permissions, so
	// they carry those as well as the network's rule.
	want := `{"permissions":{"allow":["command(go test)"`
	for _, tool := range agyNetworkTools {
		want += `,"mcp(agentiscale-network/` + tool + `)"`
	}
	want += `],"deny":["mcp(other/*)"]}}`
	settingsFile, _ := argAfter(args, "--settings")
	if !filepath.IsAbs(settingsFile) || strings.HasPrefix(settingsFile, added) || strings.HasPrefix(settingsFile, dir) {
		t.Errorf("--settings %q, want a file outside the run's workspace", settingsFile)
	}
	if s := readRecord(t, record+".settings"); s != want {
		t.Errorf("settings %s, want %s", s, want)
	}
	if slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, "command(go test)") }) {
		t.Error("the owner's permissions are in agy's arguments")
	}
	if _, err := os.Stat(settingsFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run's settings outlived the turn: %v", err)
	}
	mcpConfig := readRecord(t, record+".mcp")
	var config struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(mcpConfig), &config); err != nil {
		t.Fatal(err)
	}
	server := config.Servers[networkServerName]
	if server.Command != "/opt/agentiscaled" || len(server.Args) != 5 || server.Args[0] != networkBridgeCommand || server.Args[2] != access.URL ||
		server.Args[3] != "-ticket-file" || filepath.Dir(server.Args[4]) != added {
		t.Fatalf("bridge %+v", server)
	}
	ticket := readRecord(t, record+".ticket")
	if len(ticket) != 64 || strings.Contains(mcpConfig, ticket) || slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, ticket) }) {
		t.Errorf("ticket %q: in a file of its own, not in the configuration or any argument", ticket)
	}
	if strings.Contains(mcpConfig, access.Token) {
		t.Error("the run's bearer was written to the network folder")
	}
	if env := readRecord(t, record+".env"); strings.Contains(env, "leaked") || strings.Contains(env, access.Token) {
		t.Error("a network credential reached agy's environment")
	}
	// The fake agy never started the bridge: the turn's end revoked the
	// ticket.
	if _, err := redeemTicket(context.Background(), http.DefaultClient, ticketURL(access.URL), ticket); err == nil {
		t.Error("an unspent ticket outlived its turn")
	}

	// A server of the network's name in the directory's or the owner's agy
	// configuration would stand in for it: the turn does not run.
	shadow := `{"mcpServers":{"agentiscale-network":{"command":"evil"}}}`
	write(t, filepath.Join(dir, ".agents", "mcp_config.json"), shadow)
	if _, err := (&agyRuntime{command: command, bridge: "/opt/agentiscaled"}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x", Network: access}); err == nil || !strings.Contains(err.Error(), "would take the place of the network's") {
		t.Errorf("shadowed network: %v", err)
	}
	os.Remove(filepath.Join(dir, ".agents", "mcp_config.json"))
	write(t, filepath.Join(home, ".gemini", "config", "mcp_config.json"), shadow)
	if _, err := (&agyRuntime{command: command, bridge: "/opt/agentiscaled"}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x", Network: access}); err == nil || !strings.Contains(err.Error(), "would take the place of the network's") {
		t.Errorf("shadowed network in the owner's configuration: %v", err)
	}
	write(t, filepath.Join(home, ".gemini", "config", "mcp_config.json"), "")

	// Settings it cannot read would drop the owner's permissions: the turn
	// does not run.
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte("{// not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&agyRuntime{command: command, bridge: "/opt/agentiscaled"}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x", Network: access}); err == nil || !strings.Contains(err.Error(), "agy's settings") {
		t.Errorf("unreadable settings: %v", err)
	}
}

// The bridge spends its ticket for the run's bearer and serves the
// network's tools on stdio; nobody can spend the ticket again, and none is
// redeemed once its task has ended.
func TestNetworkBridge(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	network, calls := fakeNodeNetworkServer(t)
	g, err := network.grantFor("caller", "t-bridge")
	if err != nil {
		t.Fatal(err)
	}
	defer g.begin(ctx, "turn-token", time.Now().Add(time.Minute))()
	ticket, _, err := network.ticket(g)
	if err != nil {
		t.Fatal(err)
	}
	ticketFile := filepath.Join(t.TempDir(), "ticket")
	write(t, ticketFile, ticket)
	toBridge, bridgeIn := io.Pipe()
	bridgeOut, fromBridge := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- RunNetworkBridge(ctx, []string{"-url", network.url, "-ticket-file", ticketFile}, toBridge, fromBridge)
		fromBridge.Close()
	}()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "agy", Version: "1"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: bridgeOut, Writer: bridgeIn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if init := session.InitializeResult(); init.ServerInfo.Name != networkServerName || init.Instructions != networkInstructions {
		t.Errorf("bridge serves %q with instructions %q", init.ServerInfo.Name, init.Instructions)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if _, err := os.Stat(ticketFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the bridge left its ticket file: %v", err)
	}
	// agy may call each of the network's tools without asking.
	want := slices.Clone(agyNetworkTools)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("bridge tools %q, want %q (agyNetworkTools)", names, want)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents"})
	if err != nil || res.IsError {
		t.Fatalf("list_agents through the bridge: %+v, %v", res, err)
	}
	if got := calls(); !strings.Contains(got, "Bearer turn-token") {
		t.Errorf("the node got %q", got)
	}
	if _, err := redeemTicket(ctx, http.DefaultClient, ticketURL(network.url), ticket); err == nil {
		t.Error("a spent ticket was redeemed again")
	}
	session.Close()
	if err := <-done; err != nil {
		t.Errorf("bridge: %v", err)
	}

	late, _, err := network.ticket(g)
	if err != nil {
		t.Fatal(err)
	}
	network.forget("t-bridge")
	if _, err := redeemTicket(ctx, http.DefaultClient, ticketURL(network.url), late); err == nil {
		t.Error("a ticket was redeemed after its task ended")
	}
}

func TestNetworkBridgeTakesOnlyALoopbackEndpoint(t *testing.T) {
	for _, u := range []string{"https://127.0.0.1:1/mcp", "http://example.com:80/mcp", "http://127.0.0.1:1/other", "http://10.0.0.1:1/mcp", "nonsense"} {
		if err := loopbackEndpoint(u); err == nil {
			t.Errorf("%s was taken", u)
		}
	}
	if err := loopbackEndpoint("http://127.0.0.1:4242/mcp"); err != nil {
		t.Error(err)
	}
	if err := RunNetworkBridge(context.Background(), []string{"-url", "http://127.0.0.1:1/mcp"}, strings.NewReader(""), io.Discard); err == nil {
		t.Error("the bridge ran without a ticket")
	}
}

// The bridge follows no redirect, which would carry its ticket elsewhere.
func TestNetworkBridgeFollowsNoRedirect(t *testing.T) {
	elsewhere := make(chan string, 1)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		elsewhere <- r.Form.Get("ticket")
	}))
	defer other.Close()
	node := httptest.NewServer(http.RedirectHandler(other.URL+"/ticket", http.StatusTemporaryRedirect))
	defer node.Close()
	ticketFile := filepath.Join(t.TempDir(), "ticket")
	write(t, ticketFile, "secret-ticket")
	if err := RunNetworkBridge(context.Background(), []string{"-url", node.URL + "/mcp", "-ticket-file", ticketFile}, strings.NewReader(""), io.Discard); err == nil {
		t.Error("the bridge took a redirect as the bearer")
	}
	select {
	case got := <-elsewhere:
		t.Errorf("the ticket %q followed the redirect", got)
	default:
	}
}

// A ticket is gone once revoked or expired, or once the network of a
// one-turn grant (open) is released.
func TestNetworkTicketLifetime(t *testing.T) {
	network, _ := fakeNodeNetworkServer(t)
	g, err := network.grantFor("caller", "t-life")
	if err != nil {
		t.Fatal(err)
	}
	defer g.begin(context.Background(), "turn-token", time.Now().Add(time.Minute))()
	redeem := func(ticket string) error {
		_, err := redeemTicket(context.Background(), http.DefaultClient, ticketURL(network.url), ticket)
		return err
	}

	revoked, revoke, _ := network.ticket(g)
	revoke()
	if redeem(revoked) == nil {
		t.Error("a revoked ticket was redeemed")
	}

	expired, _, _ := network.ticket(g)
	network.mu.Lock()
	for k, v := range network.tickets {
		v.expires = time.Now().Add(-time.Second)
		network.tickets[k] = v
	}
	network.mu.Unlock()
	if redeem(expired) == nil {
		t.Error("an expired ticket was redeemed")
	}

	// A redeemed ticket's bearer reaches the grant until revoke, which ends
	// the turn: a bridge that outlives it is refused.
	live, revokeLive, _ := network.ticket(g)
	bearer, err := redeemTicket(context.Background(), http.DefaultClient, ticketURL(network.url), live)
	if err != nil || bearer == "" || bearer == g.bearer {
		t.Fatalf("redeemed %q, %v; want a bearer of its own", bearer, err)
	}
	reach := func() int {
		req, _ := http.NewRequest(http.MethodPost, network.url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := reach(); code != http.StatusOK {
		t.Errorf("the redeemed bearer got %d", code)
	}
	revokeLive()
	if code := reach(); code != http.StatusUnauthorized {
		t.Errorf("the redeemed bearer got %d after its turn", code)
	}

	access, release := network.open("one-turn-token", "caller", network.sentBy("t-open"), time.Now().Add(time.Minute))
	ticket, _, _ := access.ticket()
	release()
	if redeem(ticket) == nil {
		t.Error("a ticket was redeemed after its network was released")
	}
}

// A tool call that a turn's bearer sent runs nothing once that bearer is
// revoked, even when its request was let in before and its body comes only
// once a later turn holds the grant; the task's own bearer works on.
func TestNetworkTicketBearerCallEndsWithItsTurn(t *testing.T) {
	network, calls := fakeNodeNetworkServer(t)
	g, err := network.grantFor("caller", "t-race")
	if err != nil {
		t.Fatal(err)
	}
	endFirst := g.begin(context.Background(), "turn-1-token", time.Now().Add(time.Minute))
	ticket, revoke, _ := network.ticket(g)
	alias, err := redeemTicket(context.Background(), http.DefaultClient, ticketURL(network.url), ticket)
	if err != nil {
		t.Fatal(err)
	}
	post := func(bearer, session string, body io.Reader) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPost, network.url, body)
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
			req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		}
		return http.DefaultClient.Do(req)
	}
	open := func(bearer string) string {
		resp, err := post(bearer, "", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		session := resp.Header.Get("Mcp-Session-Id")
		if resp, err = post(bearer, session, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return session
	}
	const listAgents = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_agents","arguments":{}}}`
	in := make(chan struct{}, 16)
	network.admitted = func() {
		select {
		case in <- struct{}{}:
		default:
		}
	}
	session := open(alias)
	for len(in) > 0 {
		<-in
	}

	// The call is let in during the first turn and stalls in its body.
	body, write := io.Pipe()
	answer := make(chan string, 1)
	go func() {
		resp, err := post(alias, session, body)
		if err != nil {
			answer <- err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		answer <- string(b)
	}()
	<-in
	// Its turn ends, the bearer with it, and the next turn begins.
	revoke()
	endFirst()
	defer g.begin(context.Background(), "turn-2-token", time.Now().Add(time.Minute))()
	write.Write([]byte(listAgents))
	write.Close()
	if got := <-answer; !strings.Contains(got, "no network access now") {
		t.Errorf("the stalled call answered %q", got)
	}
	if strings.Contains(calls(), "turn-2-token") {
		t.Errorf("the stalled call ran with the next turn's token: %q", calls())
	}

	// The task's own bearer keeps its session over the turns.
	own := open(g.bearer)
	resp, err := post(g.bearer, own, strings.NewReader(listAgents))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "no network access now") || !strings.Contains(calls(), "turn-2-token") {
		t.Errorf("the task's bearer in its next turn: %s (node got %q)", b, calls())
	}
}

func TestTicketRedeemNeedsPost(t *testing.T) {
	network, _ := fakeNodeNetworkServer(t)
	resp, err := http.Get(ticketURL(network.url) + "?" + url.Values{"ticket": {"x"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %s", resp.Status)
	}
}
