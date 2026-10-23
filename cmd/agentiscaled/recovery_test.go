package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestMain lets a test start the node as its own process, the way the
// background service does, with its output in a log file.
func TestMain(m *testing.M) {
	if os.Getenv("AGENTISCALED_TEST_MAIN") == "1" {
		watchPoll, watchCheckAfter, watchCheckEvery = 50*time.Millisecond, 300*time.Millisecond, 500*time.Millisecond
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// standInMesh answers a node's pulls from its network's mesh the way a mesh
// in each state would.
type standInMesh struct {
	mu   sync.Mutex
	mode string // fine, gone (no mesh at the address), down, refuse (the credential), rekeyed
	key  ed25519.PrivateKey
	hits int
	srv  *httptest.Server
}

func newStandInMesh(t *testing.T, mode string) *standInMesh {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := &standInMesh{mode: mode, key: key}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *standInMesh) set(mode string) {
	m.mu.Lock()
	m.mode = mode
	m.mu.Unlock()
}

func (m *standInMesh) requests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits
}

func (m *standInMesh) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.hits++
	mode := m.mode
	m.mu.Unlock()
	reply := func(msg proto.Message) {
		b, _ := proto.Marshal(msg)
		_, _ = w.Write(b)
	}
	switch {
	case mode == "gone":
		// The address answers, but no mesh is behind it.
		http.Error(w, "not found", http.StatusNotFound)
	case mode == "down":
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	case r.URL.Path == "/keys":
		signer := m.key
		if mode == "rekeyed" {
			_, signer, _ = ed25519.GenerateKey(rand.Reader)
		}
		resp := &api.KeysResponse{PublicKeys: [][]byte{signer.Public().(ed25519.PublicKey)}, SignTime: timestamppb.Now()}
		if err := api.SignKeysResponse(resp, []ed25519.PrivateKey{signer}, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		reply(resp)
	case r.URL.Path == "/info":
		reply(&api.ControlPlaneInfoResponse{})
	case mode == "refuse":
		http.Error(w, "node credential required", http.StatusUnauthorized)
	case r.URL.Path == "/policies":
		reply(&api.PolicyConfigGetResponse{})
	default:
		http.NotFound(w, r)
	}
}

// standInControl answers /v1/node-status with a fixed status and code, after
// checking the node's signature.
type standInControl struct {
	t      *testing.T
	mu     sync.Mutex
	status int
	code   string
	srv    *httptest.Server
}

func newStandInControl(t *testing.T, status int, code string) *standInControl {
	c := &standInControl{t: t, status: status, code: code}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node-status" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			NetworkID   string `json:"network_id"`
			PeerID      string `json:"peer_id"`
			TimestampMs int64  `json:"timestamp_ms"`
			Signature   string `json:"signature"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		pid, err := peer.Decode(req.PeerID)
		sig, _ := base64.RawURLEncoding.DecodeString(req.Signature)
		if err == nil {
			pub, _ := pid.ExtractPublicKey()
			if ok, _ := pub.Verify(nodeStatusPayload(req.NetworkID, req.PeerID, req.TimestampMs), sig); !ok {
				err = io.ErrUnexpectedEOF
			}
		}
		if err != nil {
			t.Errorf("node-status request not signed by the node: %+v", req)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c.mu.Lock()
		status, code := c.status, c.code
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		switch status {
		case http.StatusOK:
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "member"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "code": code})
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *standInControl) set(status int, code string) {
	c.mu.Lock()
	c.status, c.code = status, code
	c.mu.Unlock()
}

// shortTempDir is a data directory whose socket path fits the Unix limit.
func shortTempDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "ais")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// seedNode gives the data directory a node enrolled on mesh, signed by its
// key, and the settings `up` would save; it returns the peer ID.
func seedNode(t *testing.T, dir, meshURL string, key ed25519.PrivateKey) string {
	t.Helper()
	store, err := meshnode.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	priv := meshnode.GetOrGenerateKey(store)
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(24 * time.Hour)
	biscuit, err := meshnode.MintBootstrapBiscuitToken(key, pid, api.RoleNode, exp, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		store.SaveControlPlaneURL(meshURL),
		store.SaveMeshConfig(key.Public().(ed25519.PublicKey), nil),
		store.SaveIdentity(biscuit),
		store.SaveIdentityExpiration(exp.Unix()),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return pid.String()
}

// nodeProcess is `agentiscaled run` in its own process, logging to a file
// as under launchd or systemd.
type nodeProcess struct {
	cmd *exec.Cmd
	log string
}

func startNode(t *testing.T, dir string) *nodeProcess {
	t.Helper()
	logPath := filepath.Join(dir, logFile)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command(os.Args[0], "run", "-data-dir", dir)
	cmd.Env = append(os.Environ(), "AGENTISCALED_TEST_MAIN=1")
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &nodeProcess{cmd: cmd, log: logPath}
	t.Cleanup(p.stop)
	return p
}

func (p *nodeProcess) stop() {
	if p.cmd.ProcessState != nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
}

func (p *nodeProcess) output() string {
	b, _ := os.ReadFile(p.log)
	return string(b)
}

// waitLog waits until the log has a line containing want, and returns it.
func (p *nodeProcess) waitLog(t *testing.T, want string, wait time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		for _, line := range strings.Split(p.output(), "\n") {
			if strings.Contains(line, want) {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("log has no line with %q:\n%s", want, p.output())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func writeSettings(t *testing.T, dir string, s settings) {
	t.Helper()
	if err := saveSettings(dir, s); err != nil {
		t.Fatal(err)
	}
}

func statusOf(t *testing.T, dir string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runStatus(context.Background(), []string{"-data-dir", dir}, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// For each way a network can stop admitting a node, the node says so in one
// line, in its log and in `status`, names the fix, and stops retrying: the
// log gets no further line and the mesh no further request, also after the
// service starts it again.
func TestNodeStopsWhenItsNetworkNoLongerAdmitsIt(t *testing.T) {
	for _, tc := range []struct {
		name, mesh string
		status     int
		code, want string
	}{
		{"network gone", "gone", http.StatusNotFound, "unknown_network", "no longer exists"},
		{"node removed", "fine", http.StatusGone, "node_removed", "was removed from network"},
		{"credential refused", "refuse", http.StatusOK, "", "no longer accepts its credential"},
		{"signing key rejected", "rekeyed", http.StatusOK, "", "signed with a key this node no longer trusts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mesh := newStandInMesh(t, tc.mesh)
			control := newStandInControl(t, tc.status, tc.code)
			dir := shortTempDir(t)
			network := uuid.NewString()
			pid := seedNode(t, dir, mesh.srv.URL, mesh.key)
			writeSettings(t, dir, settings{Control: control.srv.URL, Listen: "/ip4/127.0.0.1/tcp/0", LogLevel: "info", PeerID: pid, NetworkID: network, MeshURL: mesh.srv.URL})

			node := startNode(t, dir)
			line := node.waitLog(t, "This node stopped", 30*time.Second)
			if !strings.Contains(line, tc.want) || !strings.Contains(line, network) || !strings.Contains(line, "agentiscaled reset -data-dir "+dir) {
				t.Fatalf("stop line %q, want it to say %q about network %s and name the reset", line, tc.want, network)
			}
			status := statusOf(t, dir)
			if !strings.Contains(status, "Mesh connection: "+strings.TrimSpace(line[strings.Index(line, "This node stopped"):])) {
				t.Errorf("status does not show the stop line:\n%s", status)
			}

			// No retries after the stop: watch for many check intervals.
			logged, asked := node.output(), mesh.requests()
			time.Sleep(2 * time.Second)
			if after := node.output(); after != logged {
				t.Errorf("the log went on after the stop:\n%s", strings.TrimPrefix(after, logged))
			}
			if mesh.requests() != asked {
				t.Errorf("the node asked its mesh %d more times after the stop", mesh.requests()-asked)
			}
			if strings.Count(logged, "This node stopped") != 1 {
				t.Errorf("the stop is logged %d times, want once:\n%s", strings.Count(logged, "This node stopped"), logged)
			}

			// Started again, as the service does at login, it says why once
			// and does not retry.
			node.stop()
			before := node.output()
			again := startNode(t, dir)
			again.waitLog(t, "This node stopped", 10*time.Second)
			time.Sleep(time.Second)
			if added := strings.TrimPrefix(again.output(), before); strings.Count(added, "\n") != 1 {
				t.Errorf("after a restart the log got more than the stop line:\n%s", added)
			}
			if mesh.requests() != asked {
				t.Errorf("after a restart the node asked its mesh again")
			}
		})
	}
}

// An outage of the mesh or the control service is not a lost network: the
// node keeps retrying and never suggests a reset, also once they are back.
func TestNodeKeepsRetryingThroughAnOutage(t *testing.T) {
	mesh := newStandInMesh(t, "gone")
	control := newStandInControl(t, http.StatusServiceUnavailable, "")
	dir := shortTempDir(t)
	network := uuid.NewString()
	pid := seedNode(t, dir, mesh.srv.URL, mesh.key)
	writeSettings(t, dir, settings{Control: control.srv.URL, Listen: "/ip4/127.0.0.1/tcp/0", LogLevel: "info", PeerID: pid, NetworkID: network, MeshURL: mesh.srv.URL})

	node := startNode(t, dir)
	node.waitLog(t, "agentiscaled running", 30*time.Second)
	for _, phase := range []struct{ mesh, control string }{
		// The mesh's address answers 404 while the control service is down:
		// only the control service can say the network is gone.
		{"gone", "down"},
		{"down", "down"},
		{"down", "member"},
		// Back again.
		{"fine", "member"},
	} {
		mesh.set(phase.mesh)
		if phase.control == "member" {
			control.set(http.StatusOK, "")
		} else {
			control.set(http.StatusServiceUnavailable, "")
		}
		asked := mesh.requests()
		time.Sleep(1500 * time.Millisecond)
		if mesh.requests() == asked {
			t.Errorf("mesh %s, control %s: the node stopped asking its mesh", phase.mesh, phase.control)
		}
		if out := node.output(); strings.Contains(out, "This node stopped") || strings.Contains(out, "reset") {
			t.Fatalf("mesh %s, control %s: the node gave up:\n%s", phase.mesh, phase.control, out)
		}
		if status := statusOf(t, dir); strings.Contains(status, "reset") {
			t.Fatalf("mesh %s, control %s: status suggests a reset:\n%s", phase.mesh, phase.control, status)
		}
	}
	if !strings.Contains(node.output(), "retrying") {
		t.Errorf("the log does not say the node is retrying:\n%s", node.output())
	}
}

// `reset` replaces only the node's identity and enrollment: every stored
// setting, agent policy included, the agent's own Codex home and sign-in,
// the API token and every client configuration file stay as they were, and
// an agent signed in on its own takes a task afterwards without signing in
// again.
func TestResetKeepsEverythingButTheIdentity(t *testing.T) {
	home := shortTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CODEX_HOME", "")
	dir := filepath.Join(home, "node")
	agentDir := filepath.Join(home, "work")
	for _, d := range []string{dir, agentDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A Codex that works only with the agent's own sign-in.
	codex := filepath.Join(home, "bin", "codex")
	write(t, codex, "#!/bin/sh\n[ \"$1\" = --version ] && { echo codex-test; exit 0; }\n[ -f \"$CODEX_HOME/auth.json\" ] || { echo 'not signed in' >&2; exit 1; }\nwhile [ $# -gt 0 ]; do [ \"$1\" = --output-last-message ] && out=$2; shift; done\ncat > /dev/null\necho 'Done after reset' > \"$out\"\n")
	if err := os.Chmod(codex, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "codex", "auth.json"), `{"agent":"own sign-in"}`)
	write(t, filepath.Join(dir, "api-token"), "TOKEN\n")

	_, oldKey, _ := ed25519.GenerateKey(rand.Reader)
	oldPeer := seedNode(t, dir, "https://old.mesh.test", oldKey)
	before := settings{
		Control: "https://control.test", BindAddr: "", Listen: "/ip4/127.0.0.1/tcp/0", LogLevel: "debug",
		Agent: "codex", AgentDir: agentDir, AgentCommand: codex, AgentName: "helper", AgentDescription: "Answers questions",
		AgentIgnoreDirInstructions: true, AgentIgnoreDirSkills: true, AgentOwnConfig: true,
		AgentEnv: []string{"SOME_VAR=1"}, MachineName: "laptop", Path: "/usr/bin:/bin",
		PeerID: oldPeer, NetworkID: uuid.NewString(), MeshURL: "https://old.mesh.test",
	}
	writeSettings(t, dir, before)
	if err := saveHalt(dir, newHalt(haltNetworkGone, before.NetworkID, dir)); err != nil {
		t.Fatal(err)
	}
	clientFiles := []string{
		filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"),
		filepath.Join(home, ".codex", "config.toml"),
	}
	write(t, clientFiles[0], `{"mcpServers":{"agentiscale":{"command":"agentiscaled","args":["mcp","-data-dir","`+dir+`"]}}}`)
	write(t, clientFiles[1], "[mcp_servers.agentiscale]\ncommand = \"agentiscaled\"\nargs = [\"mcp\", \"-data-dir\", \""+dir+"\"]\n")
	kept := append([]string{filepath.Join(dir, "codex", "auth.json"), filepath.Join(dir, "api-token")}, clientFiles...)
	snapshot := map[string][]byte{}
	for _, f := range kept {
		snapshot[f], _ = os.ReadFile(f)
	}

	// The mesh's part of enrolling, which needs a router, is stood in for:
	// the new key gets a credential.
	newNetwork := uuid.NewString()
	_, newKey, _ := ed25519.GenerateKey(rand.Reader)
	var signedInWith string
	enrollNew = func(ctx context.Context, c config) (string, error) {
		signedInWith = c.signInToken
		seedNode(t, c.dataDir, "https://"+newNetwork+".mesh.test", newKey)
		return newNetwork, nil
	}
	t.Cleanup(func() { enrollNew = enrollOnly })
	auth := newFakeAuth0(t, "")
	var out bytes.Buffer
	if err := runReset(context.Background(), []string{"-data-dir", dir, "-auth-issuer", auth.config().issuer, "-auth-client-id", "CLIENT", "-auth-audience", "https://control.test"}, &out); err != nil {
		t.Fatalf("reset: %v\n%s", err, out.String())
	}
	if signedInWith != "ACCESS" {
		t.Errorf("enrolled with sign-in %q, want the owner's", signedInWith)
	}

	after, err := loadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.PeerID == oldPeer || after.NetworkID != newNetwork || after.MeshURL != "https://"+newNetwork+".mesh.test" {
		t.Errorf("identity after reset: %s in %s at %s, want a new peer in %s", after.PeerID, after.NetworkID, after.MeshURL, newNetwork)
	}
	want := before
	want.PeerID, want.NetworkID, want.MeshURL = after.PeerID, after.NetworkID, after.MeshURL
	if !reflect.DeepEqual(after, want) {
		t.Errorf("settings changed beyond the identity:\n got %+v\nwant %+v", after, want)
	}
	for _, f := range kept {
		if b, _ := os.ReadFile(f); !bytes.Equal(b, snapshot[f]) {
			t.Errorf("%s changed: %q, was %q", f, b, snapshot[f])
		}
	}
	if h, _ := loadHalt(dir); h != nil {
		t.Errorf("the stop is still recorded after reset: %+v", h)
	}
	moved, _ := filepath.Glob(filepath.Join(dir, meshnode.StoreFile+".replaced-*"))
	if len(moved) != 1 {
		t.Fatalf("previous identity moved to %v, want one file", moved)
	}
	if status := statusOf(t, dir); strings.Contains(status, "stopped") || !strings.Contains(status, after.PeerID) {
		t.Errorf("status after reset:\n%s", status)
	}

	// The agent, signed in on its own, takes a task with no new sign-in.
	c := after.config(dir)
	agent, err := newAgent(&c)
	if err != nil {
		t.Fatalf("agent after reset: %v", err)
	}
	srv := httptest.NewServer(agent.handler(c.agentName, "secret"))
	t.Cleanup(srv.Close)
	h := &agentHarness{svc: agent, url: srv.URL, dir: agentDir, client: a2aClient(t, srv.URL, meshHeaders{"secret", testSender})}
	task := h.await(t, h.send(t, "", "What changed?", nil).ID, a2a.TaskStateCompleted)
	if got := describeTask(task).Result; !strings.Contains(got, "Done after reset") {
		t.Errorf("task answer %q", got)
	}
}
