package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/multiformats/go-multiaddr"
)

// A hosted Claude Code run gets the network as an MCP server whose
// configuration, token included, is in an owner-only file that is gone when
// the run ends, and may use its tools.
func TestClaudeRuntimeNetwork(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "rec")
	command := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + ".args\n" +
		"while [ $# -gt 0 ]; do\n  if [ \"$1\" = --mcp-config ]; then cat \"$2\" > " + record + ".mcp; ls -l \"$2\" > " + record + ".mode; echo \"$2\" > " + record + ".path; fi\n  shift\ndone\n" +
		"cat > /dev/null\nprintf '{\"type\":\"result\",\"is_error\":false,\"result\":\"done\"}\\n'\n"
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	access := &networkAccess{URL: "http://127.0.0.1:1234/mcp", Token: "task-token"}
	for _, policy := range []agentPolicy{{}, {OwnerConfig: true}} {
		rt := &claudeRuntime{command: command, config: filepath.Join(dir, "cfg"), policy: policy}
		if _, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x", Network: access}); err != nil {
			t.Fatal(err)
		}
		args := " " + strings.ReplaceAll(readRecord(t, record+".args"), "\n", " ")
		if !strings.Contains(args, " --allowedTools mcp__agentiscale-network ") || strings.Contains(args, "task-token") {
			t.Errorf("claude arguments %q", args)
		}
		var config struct {
			MCPServers map[string]struct {
				Type, URL string
				Headers   map[string]string
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(readRecord(t, record+".mcp")), &config); err != nil {
			t.Fatal(err)
		}
		server := config.MCPServers[networkServerName]
		if len(config.MCPServers) != 1 || server.Type != "http" || server.URL != access.URL || server.Headers["Authorization"] != "Bearer task-token" {
			t.Errorf("MCP configuration %+v", config)
		}
		if mode := readRecord(t, record+".mode"); !strings.HasPrefix(mode, "-rw-------") {
			t.Errorf("MCP configuration file mode %q", mode)
		}
		if path := strings.TrimSpace(readRecord(t, record+".path")); strings.HasPrefix(path, dir) {
			t.Errorf("MCP configuration %s is in the agent's directory", path)
		} else if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("MCP configuration %s left after the run: %v", path, err)
		}
	}

	// Without the network, no MCP configuration.
	rt := &claudeRuntime{command: command, config: filepath.Join(dir, "cfg")}
	if _, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if args := readRecord(t, record+".args"); strings.Contains(args, "--mcp-config") || strings.Contains(args, "--allowedTools") {
		t.Errorf("claude arguments without the network %q", args)
	}
}

// A hosted Codex run gets the network as an MCP server whose token is only in
// Codex's environment, never on its command line.
func TestCodexRuntimeNetwork(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "rec")
	command := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + ".args\nenv > " + record + ".env\n" +
		"while [ $# -gt 0 ]; do\n  if [ \"$1\" = --output-last-message ]; then echo done > \"$2\"; fi\n  shift\ndone\ncat > /dev/null\n"
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(networkTokenEnv, "stale")
	access := &networkAccess{URL: "http://127.0.0.1:1234/mcp", Token: "task-token"}
	for _, policy := range []agentPolicy{{}, {OwnerConfig: true}} {
		rt := &codexRuntime{command: command, home: filepath.Join(dir, "home"), policy: policy}
		if _, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x", Network: access}); err != nil {
			t.Fatal(err)
		}
		args := strings.ReplaceAll(readRecord(t, record+".args"), "\n", " ")
		for _, want := range []string{`-c mcp_servers.agentiscale-network.url="http://127.0.0.1:1234/mcp"`, `-c mcp_servers.agentiscale-network.bearer_token_env_var="AGENTISCALE_NETWORK_TOKEN"`, `-c shell_environment_policy.inherit="core"`} {
			if !strings.Contains(args, want) {
				t.Errorf("codex arguments %q lack %q", args, want)
			}
		}
		if strings.Contains(args, "task-token") {
			t.Errorf("the token is on the command line: %q", args)
		}
		env := readRecord(t, record+".env")
		if !strings.Contains(env, networkTokenEnv+"=task-token\n") || strings.Contains(env, "stale") {
			t.Errorf("codex environment %q", env)
		}
	}
}

// caller is a hosted agent's runtime that calls another agent through the
// network it gets, as a Claude Code or Codex run would through its MCP
// client.
type caller struct {
	helper *fixture
	tokens chan string
}

func (c *caller) Describe(card *a2a.AgentCard) { card.Description = "caller" }

func (c *caller) Run(ctx context.Context, _ string, req taskRequest) (turnResult, error) {
	if req.Network == nil {
		return turnResult{}, errors.New("no network")
	}
	c.tokens <- req.Network.Token
	session, err := networkSession(ctx, req.Network)
	if err != nil {
		return turnResult{}, err
	}
	defer session.Close()
	call := func(name string, args map[string]any, out any) error {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return err
		}
		if res.IsError {
			return errors.New(res.Content[0].(*mcp.TextContent).Text)
		}
		return json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), out)
	}
	var agents listAgentsOut
	if err := call("list_agents", nil, &agents); err != nil {
		return turnResult{}, err
	}
	if len(agents.Agents) != 1 || agents.Agents[0] != (agentRow{Service: "helper"}) {
		return turnResult{}, errors.New("list_agents: " + jsonString(agents))
	}
	var self taskOut
	if err := call("send_task", map[string]any{"service": "caller", "text": "loop"}, &self); err == nil || !strings.Contains(err.Error(), "cannot send tasks to itself") {
		return turnResult{}, errors.New("sending a task to itself: " + jsonString(self))
	}
	var sent, done taskOut
	if err := call("send_task", map[string]any{"service": "helper", "text": "help with " + req.Text}, &sent); err != nil {
		return turnResult{}, err
	}
	c.helper.release <- "helped"
	if err := call("get_task", map[string]any{"service": "helper", "task_id": sent.TaskID, "wait_seconds": 10}, &done); err != nil {
		return turnResult{}, err
	}
	if done.Terminal != "completed" {
		return turnResult{}, errors.New("helper task: " + jsonString(done))
	}
	return turnResult{Text: "caller got: " + done.Result, Session: "s"}, nil
}

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }

// networkSession connects to a run's network as its runtime would.
func networkSession(ctx context.Context, n *networkAccess) (*mcp.ClientSession, error) {
	client := &http.Client{Transport: bearer{http.DefaultTransport, n.Token}}
	return mcp.NewClient(&mcp.Implementation{Name: "runtime", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: n.URL, HTTPClient: client}, nil)
}

type bearer struct {
	next  http.RoundTripper
	token string
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// A hosted agent calls another agent with its task's sealed token, which
// the node checks, and which nothing accepts once the task has ended. The
// agent it calls works at the delegation bound, so without the network.
func TestHostedAgentCallsAnotherAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := shortTempDir(t)
	mesh := newStandInMesh(t, "fine")
	routerAddr := standInRouter(t, mesh.key)
	pid := seedNode(t, dir, mesh.srv.URL, mesh.key)
	store, err := meshnode.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id, _ := peer.Decode(pid)
	token, err := meshnode.MintBootstrapBiscuitToken(mesh.key, id, api.RoleNode, time.Now().Add(time.Hour), []*api.PolicyRole{{Name: api.RoleNode, AllowedServices: []string{"*"}, AllowedTargets: []string{"*"}, AllowedPublish: []string{"*"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveIdentity(token); err != nil {
		t.Fatal(err)
	}
	node, err := meshnode.NewAgentMeshNode(meshnode.Options{PrivKey: meshnode.GetOrGenerateKey(store), Store: store, ControlPlanePubKey: mesh.key.Public().(ed25519.PublicKey), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}, RouterAddrs: []multiaddr.Multiaddr{routerAddr}, AllowLoopback: true, NodeConfig: &meshnode.NodeConfigComplete{}})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Teardown()
	if err := node.Start(ctx); err != nil {
		t.Fatal(err)
	}
	socket, agentSocket := filepath.Join(dir, meshnode.DefaultSocketName), filepath.Join(dir, localAgentSocketName)
	network, err := serveTaskNetwork(ctx, socket, agentSocket, func(ctx context.Context) ([]*api.DiscoveredProvider, error) {
		return node.DiscoverRemoteServices(ctx, api.ServiceType_SERVICE_TYPE_A2A, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	helperRT := newFixture()
	callerRT := &caller{helper: helperRT, tokens: make(chan string, 1)}
	targets := map[string]string{}
	for name, rt := range map[string]runtime{"helper": helperRT, "caller": callerRT} {
		svc, err := newAgentService(t.TempDir(), rt, dir, name)
		if err != nil {
			t.Fatal(err)
		}
		svc.network = network
		if targets[name], err = serveAgent(ctx, svc, name); err != nil {
			t.Fatal(err)
		}
		if err := node.RegisterService(ctx, &api.RegisterServiceRequest{Service: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_A2A, Name: name}, Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: targets[name]}}); err != nil {
			t.Fatal(err)
		}
	}
	sidecar, err := meshnode.StartSidecarServer(node, "", socket, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer sidecar.Shutdown(context.Background())
	localAPI, err := serveLocalAgent(ctx, node, &meshnode.NodeConfigComplete{}, targets, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer localAPI.Close()
	for !node.IsConnected() {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("router not admitted")
		}
	}

	// The owner asks the caller, which asks the helper.
	owner := &a2aTasks{http: socketClient(socket), base: "http://agentiscaled", local: socketClient(agentSocket)}
	_, sent, err := owner.send(ctx, nil, sendTaskIn{taskTarget: taskTarget{Service: "caller"}, Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-helperRT.prompts:
		if p.Text != "help with x" || p.Network != nil {
			t.Fatalf("the helper got %+v; at the delegation bound it has no network", p)
		}
	case <-ctx.Done():
		_, state, err := owner.get(context.Background(), nil, getTaskIn{taskIn: taskIn{taskTarget: taskTarget{Service: "caller"}, TaskID: sent.TaskID}})
		t.Fatalf("the helper never got the caller's task; the caller's task: %+v %v", state, err)
	}
	_, done, err := owner.get(ctx, nil, getTaskIn{taskIn: taskIn{taskTarget: taskTarget{Service: "caller"}, TaskID: sent.TaskID}, WaitSeconds: 10})
	if err != nil || done.Terminal != "completed" || done.Result != "caller got: helped" {
		t.Fatalf("caller task: %+v %v", done, err)
	}
	taskToken := <-callerRT.tokens

	// The task has ended: its token opens nothing.
	if _, err := networkSession(ctx, &networkAccess{URL: network.url, Token: taskToken}); err == nil {
		t.Error("the network API took the token of an ended task")
	}
	expectRefused := func(what string, c *http.Client, method, target string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, target, strings.NewReader("{}"))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s with the ended task's token: HTTP %d, want 403", what, resp.StatusCode)
		}
	}
	expectRefused("local agent", meshTokenClient(agentSocket, taskToken), http.MethodPost, "http://agentiscaled/a2a/"+pid+"/helper")
	expectRefused("remote agent", meshTokenClient(socket, taskToken), http.MethodGet, "http://agentiscaled/mesh/"+pid+"/a2a/helper/.well-known/agent-card.json")

	// A running task's token allows only A2A calls, and is sealed.
	live, err := network.mint(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	authorize := func(service string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://agentiscaled/ext_authz/", nil)
		req.Header.Set(api.HeaderMeshBiscuit, live)
		req.Header.Set(api.HeaderMeshTargetService, service)
		resp, err := socketClient(socket).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := authorize("a2a://helper"); code != http.StatusOK {
		t.Errorf("an A2A call with a running task's token: HTTP %d", code)
	}
	for _, service := range []string{"mcp://tools", "egress://api.example.com"} {
		if code := authorize(service); code != http.StatusForbidden {
			t.Errorf("a call to %s with a task's token: HTTP %d, want 403", service, code)
		}
	}
	form := url.Values{"grant_type": {api.GrantTypeTokenExchange}, "subject_token": {live}, "scope": {"a2a://helper"}}
	if _, err := network.post(ctx, "/oauth/token", form); err == nil {
		t.Error("a sealed task token was narrowed further")
	}

	// The tasks a hosted agent sent are its task's, over all its turns and
	// their tokens; no other task of the node's is.
	toolCall := func(session *mcp.ClientSession, name string, args map[string]any) (taskOut, error) {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return taskOut{}, err
		}
		if res.IsError {
			return taskOut{}, errors.New(res.Content[0].(*mcp.TextContent).Text)
		}
		var out taskOut
		return out, json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &out)
	}
	turn := func(parent a2a.TaskID) (*mcp.ClientSession, func()) {
		token, err := network.mint(ctx, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		access, closeGrant := network.open(token, "caller", network.sentBy(parent), time.Now().Add(time.Minute), 1)
		session, err := networkSession(ctx, access)
		if err != nil {
			t.Fatal(err)
		}
		return session, func() { session.Close(); closeGrant() }
	}
	first, endFirst := turn("parent")
	helperRT.ask <- "Which file?"
	child, err := toolCall(first, "send_task", map[string]any{"service": "helper", "text": "child"})
	if err != nil {
		t.Fatal(err)
	}
	<-helperRT.prompts
	endFirst()
	next, endNext := turn("parent")
	defer endNext()
	if got, err := toolCall(next, "get_task", map[string]any{"service": "helper", "task_id": child.TaskID, "wait_seconds": 5}); err != nil || got.State != "input_required" {
		t.Fatalf("the task's next turn following its own task: %+v %v", got, err)
	}
	other, endOther := turn("other")
	defer endOther()
	_, owners, err := owner.send(ctx, nil, sendTaskIn{taskTarget: taskTarget{Service: "helper"}, Text: "the owner's"})
	if err != nil {
		t.Fatal(err)
	}
	<-helperRT.prompts
	for _, id := range []string{child.TaskID, owners.TaskID} {
		for _, call := range []struct {
			tool string
			args map[string]any
		}{
			{"get_task", map[string]any{"service": "helper", "task_id": id}},
			{"cancel_task", map[string]any{"service": "helper", "task_id": id}},
			{"send_task", map[string]any{"service": "helper", "task_id": id, "text": "an answer"}},
		} {
			if _, err := toolCall(other, call.tool, call.args); err == nil || !strings.Contains(err.Error(), "not one you sent") {
				t.Errorf("%s on task %s of another: %v", call.tool, id, err)
			}
		}
	}
	helperRT.release <- "done"

	// An answer cannot make a delegated task shallower: the helper's task
	// from the caller resumes without the network, though the node's owner
	// answers it (as the same sender) at depth 0.
	if _, _, err := owner.send(ctx, nil, sendTaskIn{taskTarget: taskTarget{Service: "helper"}, Text: "main.go", TaskID: child.TaskID}); err != nil {
		t.Fatal(err)
	}
	if p := <-helperRT.prompts; p.Text != "main.go" || p.Network != nil {
		t.Fatalf("the delegated task resumed as %+v; it has no network", p)
	}
	helperRT.release <- "reviewed"
	// A task from a person does get the network.
	_, _, err = owner.send(ctx, nil, sendTaskIn{taskTarget: taskTarget{Service: "helper"}, Text: "from the owner"})
	if err != nil {
		t.Fatal(err)
	}
	if p := <-helperRT.prompts; p.Network == nil {
		t.Fatalf("a task from a person ran without the network: %+v", p)
	}
	helperRT.release <- "done"
}
