package node

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/whiteredhouse/agentiscaled/internal/caller"
	"github.com/whiteredhouse/agentiscaled/internal/nodeclient"
)

func localAgentFixture(t *testing.T, role *api.PolicyRole, expiry time.Time, cfg *meshnode.NodeConfigComplete) (*meshnode.AgentMeshNode, *fixture, *caller.Client, string) {
	t.Helper()
	n, fixes, tasks, dir := localAgentsFixture(t, role, expiry, cfg, "helper")
	return n, fixes["helper"], tasks, dir
}

// localAgentsFixture is a node that serves an agent under each of names.
func localAgentsFixture(t *testing.T, role *api.PolicyRole, expiry time.Time, cfg *meshnode.NodeConfigComplete, names ...string) (*meshnode.AgentMeshNode, map[string]*fixture, *caller.Client, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := shortTempDir(t)
	mesh := newStandInMesh(t, "fine")
	id := seedNode(t, dir, mesh.srv.URL, mesh.key)
	store, err := meshnode.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	pid, _ := peer.Decode(id)
	token, err := meshnode.MintBootstrapBiscuitToken(mesh.key, pid, api.RoleNode, expiry, []*api.PolicyRole{role}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveIdentity(token); err != nil {
		t.Fatal(err)
	}
	n, err := meshnode.NewAgentMeshNode(meshnode.Options{PrivKey: meshnode.GetOrGenerateKey(store), Store: store, ControlPlanePubKey: mesh.key.Public().(ed25519.PublicKey), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}, AllowLoopback: true, NodeConfig: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Teardown() })
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	sidecar, err := meshnode.StartSidecarServer(n, "", filepath.Join(dir, meshnode.DefaultSocketName), "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sidecar.Close() })
	fixes, targets := map[string]*fixture{}, map[caller.AgentName]string{}
	var agents []*agentService
	for _, name := range names {
		fix := newFixture()
		agent, err := newAgentService(t.TempDir(), fix, dir, caller.AgentName(name))
		if err != nil {
			t.Fatal(err)
		}
		// Each agent says its runtime, as the node's settings name it.
		agent.runtimeName = map[string]string{"writer": "claude"}[name]
		if agent.runtimeName == "" {
			agent.runtimeName = "codex"
		}
		// The writer's definition sets its model; the others run the
		// runtime's default.
		agent.model = map[string]string{"writer": "sonnet"}[name]
		target, _, err := serveAgent(ctx, agent, name)
		if err != nil {
			t.Fatal(err)
		}
		fixes[name], targets[caller.AgentName(name)] = fix, target
		agents = append(agents, agent)
	}
	server, err := serveLocalAgent(ctx, n, cfg, targets, dir, func() []agentActivity {
		var out []agentActivity
		for _, a := range agents {
			out = append(out, a.activity())
		}
		return out
	}, agents)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	tasks := &caller.Client{Mesh: &http.Client{}, BaseURL: "http://remote-must-not-be-used.invalid", Local: nodeclient.SocketClient(filepath.Join(dir, localAgentSocketName))}
	return n, fixes, tasks, dir
}

func localAgentRole() *api.PolicyRole {
	return &api.PolicyRole{Name: api.RoleNode, AllowedServices: []string{"a2a://helper"}, AllowedTargets: []string{"*"}, AllowedPublish: []string{"a2a://helper"}}
}

func TestLocalTaskAdapter(t *testing.T) {
	t.Parallel()
	n, fix, tasks, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	fi, err := os.Stat(filepath.Join(dir, localAgentSocketName))
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions: %v %v", fi, err)
	}
	tasks.Local.Transport = spoofLocalIdentity{next: tasks.Local.Transport}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range []string{"", n.Host.ID().String()} {
		fix.release <- "local result"
		target := caller.Target{PeerID: id, Service: "helper"}
		_, sent, err := tasks.SendTool(ctx, nil, caller.SendInput{Target: target, Text: "work"})
		if err != nil {
			t.Fatal(err)
		}
		<-fix.prompts
		_, got, err := tasks.GetTool(ctx, nil, caller.GetInput{TaskInput: caller.TaskInput{Target: target, TaskID: sent.TaskID}, WaitSeconds: 5})
		if err != nil || got.Result != "local result" {
			t.Fatalf("local task: %+v %v", got, err)
		}
		client, err := tasks.TaskClient(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(sent.TaskID)})
		if err != nil || raw.Metadata[metaSender] != n.Host.ID().String() {
			t.Fatalf("forged header changed task owner: %+v %v", raw, err)
		}
	}

}

// A node with several agents serves each under its own name on the owner's
// socket, and each task reaches the runtime of the agent it names.
func TestLocalTasksReachEachAgent(t *testing.T) {
	t.Parallel()
	role := &api.PolicyRole{Name: api.RoleNode, AllowedServices: []string{"a2a://helper", "a2a://writer"}, AllowedTargets: []string{"*"}, AllowedPublish: []string{"a2a://helper", "a2a://writer"}}
	_, fixes, tasks, _ := localAgentsFixture(t, role, time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{}, "helper", "writer")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := tasks.LocalInfo(ctx)
	if err != nil || !slices.Equal(info.Services, []caller.AgentName{"helper", "writer"}) {
		t.Fatalf("local info: %+v %v", info, err)
	}
	if want := map[caller.AgentName]string{"helper": "codex", "writer": "claude"}; !maps.Equal(info.Runtimes, want) {
		t.Fatalf("local info runtimes %v, want %v", info.Runtimes, want)
	}
	if want := map[caller.AgentName]string{"helper": "", "writer": "sonnet"}; !maps.Equal(info.Models, want) {
		t.Fatalf("local info models %v, want %v", info.Models, want)
	}
	for _, name := range []string{"writer", "helper"} {
		fixes[name].release <- "answer from " + name
		target := caller.Target{Service: caller.AgentName(name)}
		_, sent, err := tasks.SendTool(ctx, nil, caller.SendInput{Target: target, Text: "work"})
		if err != nil {
			t.Fatal(err)
		}
		<-fixes[name].prompts
		_, got, err := tasks.GetTool(ctx, nil, caller.GetInput{TaskInput: caller.TaskInput{Target: target, TaskID: sent.TaskID}, WaitSeconds: 5})
		if err != nil || got.Result != "answer from "+name {
			t.Fatalf("task for %s: %+v %v", name, got, err)
		}
	}
	if _, _, err := tasks.SendTool(ctx, nil, caller.SendInput{Target: caller.Target{Service: "other"}, Text: "work"}); err == nil || !strings.Contains(err.Error(), "does not host") {
		t.Fatalf("task for an agent the node does not host: %v", err)
	}
}

func TestLocalTaskAuthorization(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"no invocation grant", "no publish grant", "expired", "revoked", "invalid signature", "label floor"} {
		t.Run(name, func(t *testing.T) {
			role := localAgentRole()
			expiry := time.Now().Add(time.Hour)
			cfg := &meshnode.NodeConfigComplete{}
			switch name {
			case "no invocation grant":
				role.AllowedServices = nil
			case "no publish grant":
				role.AllowedPublish = nil
			case "expired":
				expiry = time.Now().Add(-time.Hour)
			case "label floor":
				cfg.EgressRequireLabels = map[string]string{"region": "eu"}
			}
			n, fix, tasks, _ := localAgentFixture(t, role, expiry, cfg)
			if name == "invalid signature" {
				raw := append([]byte(nil), n.GetIdentity()...)
				raw[len(raw)-1] ^= 1
				n.SetIdentityCache(raw)
			}
			if name == "revoked" {
				claims, err := n.VerifyLocalBiscuit(n.GetIdentity())
				if err != nil {
					t.Fatal(err)
				}
				n.RevokeBiscuitID(hex.EncodeToString(claims.RevocationIDs[0]), time.Now().Add(time.Hour))
			}
			if _, _, err := tasks.CardTool(context.Background(), nil, caller.Target{Service: "helper"}); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
				t.Fatalf("unauthorized local card was not refused by owner API: %v", err)
			}
			_, _, err := tasks.SendTool(context.Background(), nil, caller.SendInput{Target: caller.Target{Service: "helper"}, Text: "must not run"})
			if err == nil {
				t.Fatal("unauthorized task accepted")
			}
			select {
			case <-fix.prompts:
				t.Fatal("unauthorized runtime execution")
			default:
			}
		})
	}
}

func TestLocalAgentSocketDoesNotReplaceLiveOrRegularPath(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortTempDir(t), localAgentSocketName)
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenAgentSocket(path); err == nil {
		t.Fatal("replaced a regular file")
	}
	content, _ := os.ReadFile(path)
	if string(content) != "keep" {
		t.Fatal("changed regular file")
	}
	os.Remove(path)
	live, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := listenAgentSocket(path); err == nil {
		t.Fatal("replaced a live socket")
	}
}

func TestLocalAgentUnknownTarget(t *testing.T) {
	t.Parallel()
	n, _, tasks, _ := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	for _, path := range []string{"/a2a/" + testSender + "/helper", "/a2a/" + n.Host.ID().String() + "/other"} {
		req, _ := http.NewRequest("POST", "http://agentiscaled"+path, strings.NewReader("{}"))
		response, err := tasks.Local.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown target status %d", response.StatusCode)
		}
	}
}

// The adapter must ignore transport headers supplied by its owner client.
type spoofLocalIdentity struct{ next http.RoundTripper }

func (s spoofLocalIdentity) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(api.HeaderPeerID, testSender)
	r.Header.Set(api.HeaderMeshBiscuit, "forged")
	r.Header.Set("Authorization", "forged")
	r.Header.Set("X-Original-Method", "DELETE")
	r.Header.Set("X-Original-Path", "/forged")
	return s.next.RoundTrip(r)
}

func TestTaskAdapterKeepsRemoteMeshPath(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t, testSender)
	backend := h.svc.handler("codex", "secret")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mesh/"+testSender+"/a2a/codex" {
			t.Errorf("remote route changed: %s", r.URL.Path)
		}
		backend.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	tasks := &caller.Client{Mesh: &http.Client{Transport: meshHeaders{"secret", testSender}}, BaseURL: proxy.URL, Local: nodeclient.SocketClient(filepath.Join(shortTempDir(t), "missing.sock"))}
	h.fix.release <- "remote result"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target := caller.Target{PeerID: testSender, Service: "codex"}
	_, sent, err := tasks.SendTool(ctx, nil, caller.SendInput{Target: target, Text: "remote task"})
	if err != nil {
		t.Fatal(err)
	}
	<-h.fix.prompts
	_, got, err := tasks.GetTool(ctx, nil, caller.GetInput{TaskInput: caller.TaskInput{Target: target, TaskID: sent.TaskID}, WaitSeconds: 5})
	if err != nil || got.Result != "remote result" {
		t.Fatalf("remote result: %+v %v", got, err)
	}
}

func TestLocalAgentSocketReplacesOnlyStaleSocket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortTempDir(t), localAgentSocketName)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	replacement, err := listenAgentSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Close()
	file := filepath.Join(shortTempDir(t), "keep")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, path); err != nil {
		t.Fatal(err)
	}
	if _, err := listenAgentSocket(path); err == nil {
		t.Fatal("replaced a symbolic link")
	}
}

func TestLocalAgentTransportStreamsAndRejectsTCP(t *testing.T) {
	t.Parallel()
	n, _, _, authDir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopped := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(stopped)
		if r.Header.Get(api.HeaderPeerID) != n.Host.ID().String() || r.Header.Get("Authorization") != "Bearer backend-secret" {
			t.Error("backend did not receive verified node credentials")
		}
		if r.Header.Get(api.HeaderMeshBiscuit) != "" || r.Header.Get("Cookie") != "" {
			t.Error("caller secrets reached backend")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer backend.Close()
	target := strings.Replace(backend.URL, "http://", "http://:backend-secret@", 1)
	dir := shortTempDir(t)
	if err := os.Symlink(filepath.Join(authDir, meshnode.DefaultSocketName), filepath.Join(dir, meshnode.DefaultSocketName)); err != nil {
		t.Fatal(err)
	}
	server, err := serveLocalAgent(ctx, n, &meshnode.NodeConfigComplete{}, map[caller.AgentName]string{"helper": target}, dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	// A handler mounted on TCP must not gain the owner's Unix-socket trust.
	tcp := httptest.NewServer(server.Handler)
	defer tcp.Close()
	response, err := tcp.Client().Get(tcp.URL + "/info")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("TCP owner API: %d", response.StatusCode)
	}
	client := nodeclient.SocketClient(filepath.Join(dir, localAgentSocketName))
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://agentiscaled/a2a/"+n.Host.ID().String()+"/helper", strings.NewReader("{}"))
	req.Header.Set(api.HeaderMeshBiscuit, "caller-secret")
	req.Header.Set("Cookie", "caller-secret")
	req.Header.Set(api.HeaderPeerID, testSender)
	response, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(response.Body, data); err != nil {
		t.Fatal(err)
	}
	if string(data) != "data: first\n\n" {
		t.Fatalf("stream: %q", data)
	}
	response.Body.Close()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("closing stream did not cancel backend request")
	}
}

// A hosted runtime with the owner's MCP configuration can address its own
// local agent. The active parent must prevent that call from running again.
func TestLocalAgentRejectsSelfCallWhileRunning(t *testing.T) {
	t.Parallel()
	_, fix, tasks, _ := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target := caller.Target{Service: "helper"}
	_, parent, err := tasks.SendTool(ctx, nil, caller.SendInput{Target: target, Text: "parent task"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fix.prompts:
	case <-ctx.Done():
		t.Fatal("parent did not start")
	}
	_, child, err := tasks.SendTool(ctx, nil, caller.SendInput{Target: target, Text: "call myself"})
	if err != nil {
		t.Fatal(err)
	}
	_, rejected, err := tasks.GetTool(ctx, nil, caller.GetInput{TaskInput: caller.TaskInput{Target: target, TaskID: child.TaskID}, WaitSeconds: 5})
	if err != nil || rejected.Terminal != "rejected" || !strings.Contains(rejected.Message, "already running") {
		t.Fatalf("self-call: %+v %v", rejected, err)
	}
	select {
	case <-fix.prompts:
		t.Fatal("self-call executed a second runtime")
	default:
	}
	fix.release <- "parent completed"
	_, completed, err := tasks.GetTool(ctx, nil, caller.GetInput{TaskInput: caller.TaskInput{Target: target, TaskID: parent.TaskID}, WaitSeconds: 5})
	if err != nil || completed.Terminal != "completed" {
		t.Fatalf("parent: %+v %v", completed, err)
	}
}

func TestLocalAgentCardUsesOwnerSocketWithoutRunningTask(t *testing.T) {
	t.Parallel()
	n, fix, tasks, _ := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	tasks.Local.Transport = spoofLocalIdentity{next: tasks.Local.Transport}
	for _, id := range []string{"", n.Host.ID().String()} {
		result, _, err := tasks.CardTool(context.Background(), nil, caller.Target{PeerID: id, Service: "helper"})
		if err != nil || result == nil || result.IsError {
			t.Fatalf("local agent card for %q: %v, %+v", id, err, result)
		}
		if len(result.Content) != 1 || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, `"name":"helper"`) {
			t.Fatalf("unexpected agent card: %+v", result)
		}
	}
	select {
	case <-fix.prompts:
		t.Fatal("reading an agent card started a task")
	default:
	}
}

func TestLocalAgentCardHonorsHTTPMethodAndPathGrant(t *testing.T) {
	t.Parallel()
	role := localAgentRole()
	role.Http = []*api.HTTPGrant{{Service: "a2a://helper", Methods: []string{"GET"}, Paths: []string{"/.well-known/agent-card.json"}}}
	_, fix, tasks, _ := localAgentFixture(t, role, time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	target := caller.Target{Service: "helper"}
	if _, _, err := tasks.CardTool(context.Background(), nil, target); err != nil {
		t.Fatalf("permitted card read: %v", err)
	}
	if _, _, err := tasks.SendTool(context.Background(), nil, caller.SendInput{Target: target, Text: "must not run"}); err == nil {
		t.Fatal("GET-only credential authorized a task POST")
	}
	select {
	case <-fix.prompts:
		t.Fatal("forbidden POST ran the agent")
	default:
	}
}

func TestLocalAgentRequiresMeshAuthorization(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"allow", "deny", "unavailable", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			n, _, tasks, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
			path := filepath.Join(dir, meshnode.DefaultSocketName)
			// Move any real fixture listener out of the way before the first call.
			if err := os.Rename(path, path+".real"); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			var calls atomic.Int32
			if mode != "unavailable" {
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != "GET" || r.URL.Path != "/ext_authz/.well-known/agent-card.json" || r.Header.Get(api.HeaderMeshTargetService) != "a2a://helper" || r.Header.Get(api.HeaderMeshBiscuit) != base64.StdEncoding.EncodeToString(n.GetIdentity()) {
						t.Error("authorization did not receive trusted identity, target and backend method/path")
					}
					for _, header := range []string{"Cookie", "Authorization", api.HeaderPeerID, "X-Original-Method", "X-Original-Path"} {
						if r.Header.Get(header) != "" {
							t.Errorf("caller header reached authorization: %s", header)
						}
					}
					switch mode {
					case "allow":
						w.WriteHeader(http.StatusOK)
					case "deny":
						http.Error(w, "refused", http.StatusForbidden)
					case "redirect":
						http.Redirect(w, r, "/unexpected", http.StatusTemporaryRedirect)
					}
				})}
				go server.Serve(listener)
				t.Cleanup(func() { server.Close() })
			}
			tasks.Local.Transport = spoofLocalIdentity{next: tasks.Local.Transport}
			_, _, err := tasks.CardTool(context.Background(), nil, caller.Target{Service: "helper"})
			if mode == "allow" {
				if err != nil || calls.Load() != 1 {
					t.Fatalf("authorization was not consulted: calls=%d err=%v", calls.Load(), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
				t.Fatalf("authorization %s did not fail closed: %v", mode, err)
			}
			if mode == "redirect" && calls.Load() != 1 {
				t.Fatalf("followed authorization redirect: %d requests", calls.Load())
			}
		})
	}
}

func TestLocalProviderLabelFloorRequiresAttestedLabels(t *testing.T) {
	t.Parallel()
	n, _, tasks, _ := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{EgressRequireLabels: map[string]string{"region": "uk"}})
	rules, err := api.ParseDatalogRules([]string{`label("region", "uk") <- role("mesh:role:node")`})
	if err != nil {
		t.Fatal(err)
	}
	n.MeshPolicyMu.Lock()
	n.MeshPolicyRules = rules
	n.MeshPolicyMu.Unlock()
	if _, _, err := tasks.CardTool(context.Background(), nil, caller.Target{Service: "helper"}); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("derived label satisfied the attested label floor: %v", err)
	}
}

func TestLocalProviderUsesCurrentPublishPolicy(t *testing.T) {
	t.Parallel()
	role := localAgentRole()
	role.AllowedPublish = nil
	n, _, tasks, _ := localAgentFixture(t, role, time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	rules, err := api.ParseDatalogRules([]string{`granted_publish_exact("a2a", "helper") <- role("mesh:role:node")`})
	if err != nil {
		t.Fatal(err)
	}
	n.MeshPolicyMu.Lock()
	n.MeshPolicyRules = rules
	n.MeshPolicyMu.Unlock()
	if _, _, err := tasks.CardTool(context.Background(), nil, caller.Target{Service: "helper"}); err != nil {
		t.Fatalf("current publish policy was ignored: %v", err)
	}
	n.MeshPolicyMu.Lock()
	n.MeshPolicyRules = nil
	n.MeshPolicyMu.Unlock()
	if _, _, err := tasks.CardTool(context.Background(), nil, caller.Target{Service: "helper"}); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("removed publish policy still authorized the provider: %v", err)
	}
}

func TestNodeAPIRequiresSocketForHostedAgent(t *testing.T) {
	t.Parallel()
	for _, hosted := range []bool{false, true} {
		t.Run(fmt.Sprint(hosted), func(t *testing.T) {
			n, _, _, _ := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
			dir := shortTempDir(t)
			path := filepath.Join(dir, meshnode.DefaultSocketName)
			if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			c := config{dataDir: dir, bindAddr: "127.0.0.1:0"}
			if hosted {
				c.agents = []agentConfig{{Runtime: "codex", Name: "codex"}}
			}
			server, err := startNodeAPI(n, c, "test-owner-token")
			if server != nil {
				defer server.Close()
			}
			if hosted {
				if err == nil || !strings.Contains(err.Error(), "hosted agent requires") {
					t.Fatalf("missing required Unix socket did not fail startup: %v", err)
				}
			} else if err != nil {
				t.Fatalf("ordinary TCP node lost its optional socket fallback: %v", err)
			}
			content, _ := os.ReadFile(path)
			if string(content) != "keep" {
				t.Fatal("startup changed the obstructing file")
			}
		})
	}
}
