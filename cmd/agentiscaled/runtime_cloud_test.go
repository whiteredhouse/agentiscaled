package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/export/standalone"
	"github.com/google/uuid"
)

func runnerFile(t *testing.T, endpoint string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "runner.json")
	data, err := json.Marshal(map[string]string{"url": endpoint, "token": "turn-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestCloudRunnerCancellation(t *testing.T) {
	start, deleted := make(chan string, 1), make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer turn-secret" {
			t.Error("missing turn credential")
		}
		switch r.Method {
		case http.MethodPost:
			_, _ = io.Copy(io.Discard, r.Body)
			start <- r.URL.Path
			<-r.Context().Done()
		case http.MethodDelete:
			deleted <- r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	rt, err := newCloudRuntime(runnerFile(t, server.URL+"/turns"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := rt.Run(ctx, "", taskRequest{Sender: "peer", Text: "review"})
		done <- err
	}()
	var running string
	select {
	case running = <-start:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not start")
	}
	cancel()
	select {
	case got := <-deleted:
		if running != got || !strings.HasPrefix(got, "/turns/") || uuid.Validate(path.Base(got)) != nil {
			t.Errorf("unexpected turn paths %s, %s", running, got)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("cancellation did not reach the runner")
	}
	if err := <-done; err != context.Canceled {
		t.Errorf("canceled task: %v", err)
	}
}

func TestCloudRunnerKeepsTokenLocal(t *testing.T) {
	for _, endpoint := range []string{"https://runner.example/turns", "http://runner.example/turns", "http://127.0.0.1/turns?a=b"} {
		if _, err := newCloudRuntime(runnerFile(t, endpoint)); err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect") }))
	defer sink.Close()
	redirect := httptest.NewServer(http.RedirectHandler(sink.URL, http.StatusTemporaryRedirect))
	defer redirect.Close()
	file := runnerFile(t, redirect.URL+"/turns")
	rt, err := newCloudRuntime(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Run(t.Context(), "", taskRequest{}); err == nil {
		t.Error("accepted a redirect as a turn result")
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newCloudRuntime(file); err == nil {
		t.Error("read a shared token file")
	}
}

// The real foreground node enrolls once, exits when its parent closes stdin,
// returns as the same peer, and serves a real A2A task to a second mesh node.
// Only the host/worker HTTP turn handler is a stand-in.
func TestCloudGatewayAcrossMesh(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a mesh and two nodes")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	mesh, err := standalone.New(standalone.Options{BindAddress: "127.0.0.1:0", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := mesh.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer mesh.Close()

	callerDir := shortTempDir(t)
	tokenFile := filepath.Join(callerDir, "bootstrap")
	if err := os.WriteFile(tokenFile, []byte(mesh.JoinToken()), 0o600); err != nil {
		t.Fatal(err)
	}
	listen := "/ip4/127.0.0.1/tcp/0"
	if _, err := enrollOnly(ctx, config{dataDir: callerDir, controlPlane: mesh.PublicURL(), bootstrapTokenPath: tokenFile, listen: listen}); err != nil {
		t.Fatal(err)
	}
	enrolled, err := ensureJoined(ctx, &config{dataDir: callerDir, noEnroll: true}, authConfig{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var turn struct {
			Sender     string
			Text       string
			DeadlineMS int64 `json:"deadline_ms"`
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer turn-secret" ||
			!strings.HasPrefix(r.URL.Path, "/turns/") || json.NewDecoder(r.Body).Decode(&turn) != nil ||
			turn.Text != "review" || turn.Sender != enrolled.peerID ||
			turn.DeadlineMS <= time.Now().UnixMilli() || turn.DeadlineMS > time.Now().Add(taskDeadline).UnixMilli() {
			t.Errorf("unexpected request %s %s, fields %v", r.Method, r.URL.Path, turn)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"text": "reviewed", "question": false, "session": ""})
	}))
	defer worker.Close()
	configFile := runnerFile(t, worker.URL+"/turns")
	dir := shortTempDir(t)
	joinFile := filepath.Join(dir, "code")
	if err := os.WriteFile(joinFile, []byte("one-time-code"), 0o600); err != nil {
		t.Fatal(err)
	}
	joined := make(chan string, 2)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		if r.Method != http.MethodPost || r.URL.Path != "/v1/join" || json.NewDecoder(r.Body).Decode(&in) != nil || in["code"] != "one-time-code" || in["machine_name"] != "cloud-reviewer" {
			t.Errorf("unexpected enrollment %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		joined <- in["peer_id"]
		_ = json.NewEncoder(w).Encode(joinAnswer{MeshURL: mesh.PublicURL(), BootstrapToken: mesh.JoinToken(), NetworkID: "test"})
	}))
	defer control.Close()

	start := func() (io.Closer, *nodeProcess) {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], "-bind-addr=", "-listen", "/ip4/127.0.0.1/tcp/0", "-hostname", "cloud-reviewer",
			"-control", control.URL, "-join-code-path", joinFile, "-data-dir", dir, "-agent", "claude",
			"-agent-name", "release-reviewer", "-agent-runner", configFile, "-exit-on-stdin-close")
		cmd.Env = append(os.Environ(), "AGENTISCALED_TEST_MAIN=1")
		pipe, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(t.TempDir(), "log")
		output, err := os.Create(log)
		if err != nil {
			t.Fatal(err)
		}
		defer output.Close()
		cmd.Stderr, cmd.Stdout = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		node := &nodeProcess{cmd: cmd, log: log}
		t.Cleanup(node.stop)
		node.waitLog(t, "agentiscaled running", 15*time.Second)
		return pipe, node
	}
	pipe, gateway := start()
	peer := <-joined
	if err := pipe.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gateway.cmd.Wait(); err != nil {
		t.Fatalf("supervisor disconnected: %v", err)
	}
	if err := os.Remove(joinFile); err != nil {
		t.Fatal(err)
	}
	_, _ = start()
	local := socketClient(filepath.Join(dir, localAgentSocketName))
	defer local.CloseIdleConnections()
	info, err := local.Get("http://node/info")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(info.Body)
	info.Body.Close()
	if err != nil || !bytes.Contains(body, []byte(peer)) || len(joined) != 0 {
		t.Fatalf("restarted gateway changed identity: %s, %v", body, err)
	}

	writeSettings(t, callerDir, settings{Listen: listen, LogLevel: "info", Clients: "none", Foreground: true, MeshURL: mesh.PublicURL()})
	caller := startNode(t, callerDir)
	caller.waitLog(t, "agentiscaled running", 15*time.Second)
	owner := &a2aTasks{http: socketClient(filepath.Join(callerDir, "agentmesh.sock")), base: "http://node",
		local: socketClient(filepath.Join(callerDir, localAgentSocketName))}
	target := taskTarget{PeerID: peer, Service: "release-reviewer"}
	for {
		_, _, err = owner.card(ctx, nil, target)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("gateway service unreachable: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	_, sent, err := owner.send(ctx, nil, sendTaskIn{taskTarget: target, Text: "review"})
	if err != nil {
		t.Fatal(err)
	}
	_, done, err := owner.get(ctx, nil, getTaskIn{taskIn: taskIn{taskTarget: target, TaskID: sent.TaskID}, WaitSeconds: 20})
	if err != nil || done.Terminal != "completed" || done.Result != "reviewed" {
		t.Fatalf("cloud task across nodes: %+v, %v", done, err)
	}
}
