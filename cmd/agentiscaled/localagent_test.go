package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/libp2p/go-libp2p/core/peer"
)

func localAgentFixture(t *testing.T, role *api.PolicyRole, expiry time.Time, cfg *meshnode.NodeConfigComplete) (*meshnode.AgentMeshNode, *fixture, *a2aTasks, string) {
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
	fix := newFixture()
	agent, err := newAgentService(t.TempDir(), fix, dir)
	if err != nil {
		t.Fatal(err)
	}
	target, err := serveAgent(ctx, agent, "helper")
	if err != nil {
		t.Fatal(err)
	}
	server, err := serveLocalAgent(ctx, n, cfg, "helper", target, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	tasks := &a2aTasks{http: &http.Client{}, base: "http://remote-must-not-be-used.invalid", local: socketClient(filepath.Join(dir, localAgentSocketName))}
	return n, fix, tasks, dir
}

func localAgentRole() *api.PolicyRole {
	return &api.PolicyRole{Name: api.RoleNode, AllowedServices: []string{"a2a://helper"}, AllowedTargets: []string{"*"}, AllowedPublish: []string{"a2a://helper"}}
}

func TestLocalTaskAdapter(t *testing.T) {
	n, fix, tasks, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	fi, err := os.Stat(filepath.Join(dir, localAgentSocketName))
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions: %v %v", fi, err)
	}
	tasks.local.Transport = spoofLocalIdentity{next: tasks.local.Transport}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range []string{"", n.Host.ID().String()} {
		fix.release <- "local result"
		target := taskTarget{PeerID: id, Service: "helper"}
		_, sent, err := tasks.send(ctx, nil, sendTaskIn{taskTarget: target, Text: "work"})
		if err != nil {
			t.Fatal(err)
		}
		<-fix.prompts
		_, got, err := tasks.get(ctx, nil, getTaskIn{taskIn: taskIn{taskTarget: target, TaskID: sent.TaskID}, WaitSeconds: 5})
		if err != nil || got.Result != "local result" {
			t.Fatalf("local task: %+v %v", got, err)
		}
		client, err := tasks.client(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(sent.TaskID)})
		if err != nil || raw.Metadata[metaSender] != n.Host.ID().String() {
			t.Fatalf("forged header changed task owner: %+v %v", raw, err)
		}
	}

}

func TestLocalTaskAuthorization(t *testing.T) {
	for _, name := range []string{"no invocation grant", "no publish grant", "expired", "revoked", "label floor"} {
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
			if name == "revoked" {
				claims, err := n.VerifyLocalBiscuit(n.GetIdentity())
				if err != nil {
					t.Fatal(err)
				}
				n.RevokeBiscuitID(hex.EncodeToString(claims.RevocationIDs[0]), time.Now().Add(time.Hour))
			}
			_, _, err := tasks.send(context.Background(), nil, sendTaskIn{taskTarget: taskTarget{Service: "helper"}, Text: "must not run"})
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
	n, _, tasks, _ := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	for _, path := range []string{"/a2a/" + testSender + "/helper", "/a2a/" + n.Host.ID().String() + "/other"} {
		req, _ := http.NewRequest("POST", "http://agentiscaled"+path, strings.NewReader("{}"))
		response, err := tasks.local.Do(req)
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
	return s.next.RoundTrip(r)
}

func TestTaskAdapterKeepsRemoteMeshPath(t *testing.T) {
	h := newAgentHarness(t, testSender)
	backend := h.svc.handler("codex", "secret")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mesh/"+testSender+"/a2a/codex" {
			t.Errorf("remote route changed: %s", r.URL.Path)
		}
		backend.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	tasks := &a2aTasks{http: &http.Client{Transport: meshHeaders{"secret", testSender}}, base: proxy.URL, local: socketClient(filepath.Join(shortTempDir(t), "missing.sock"))}
	h.fix.release <- "remote result"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target := taskTarget{PeerID: testSender, Service: "codex"}
	_, sent, err := tasks.send(ctx, nil, sendTaskIn{taskTarget: target, Text: "remote task"})
	if err != nil {
		t.Fatal(err)
	}
	<-h.fix.prompts
	_, got, err := tasks.get(ctx, nil, getTaskIn{taskIn: taskIn{taskTarget: target, TaskID: sent.TaskID}, WaitSeconds: 5})
	if err != nil || got.Result != "remote result" {
		t.Fatalf("remote result: %+v %v", got, err)
	}
}

func TestLocalAgentSocketReplacesOnlyStaleSocket(t *testing.T) {
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
	n, _, _, _ := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
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
	server, err := serveLocalAgent(ctx, n, &meshnode.NodeConfigComplete{}, "helper", target, dir)
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
	client := socketClient(filepath.Join(dir, localAgentSocketName))
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
