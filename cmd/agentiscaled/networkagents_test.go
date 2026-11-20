package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

const testNetwork = "7d1c3a52-5f0e-4c1b-9a3e-2b8f6c4d9e10"

// fakeNodeAgentsControl answers POST /v1/node-agents as the control
// service does, checking the node's signature.
func fakeNodeAgentsControl(t *testing.T, peerID string, answer func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/node-agents" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			NetworkID   string `json:"network_id"`
			PeerID      string `json:"peer_id"`
			TimestampMs int64  `json:"timestamp_ms"`
			Signature   string `json:"signature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("request: %v", err)
		}
		pid, err := peer.Decode(req.PeerID)
		if err != nil || req.PeerID != peerID || req.NetworkID != testNetwork {
			t.Errorf("request names %s in %s", req.PeerID, req.NetworkID)
		}
		pub, _ := pid.ExtractPublicKey()
		sig, _ := base64.RawURLEncoding.DecodeString(req.Signature)
		if ok, err := pub.Verify(nodeAgentsPayload(req.NetworkID, req.PeerID, req.TimestampMs), sig); err != nil || !ok {
			t.Errorf("signature does not verify over the node-agents payload")
		}
		if d := time.Since(time.UnixMilli(req.TimestampMs)); d < 0 || d > time.Minute {
			t.Errorf("timestamp %v off", d)
		}
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testNodeKey(t *testing.T) (crypto.PrivKey, string) {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pid.String()
}

// streamLines answers as the control service's stream does: each line sent
// on lines, until lines closes or the node leaves.
func streamLines(w http.ResponseWriter, r *http.Request, lines <-chan string) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			_, _ = io.WriteString(w, line+"\n")
			w.(http.Flusher).Flush()
		}
	}
}

func agentsLine(t *testing.T, self, state string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"network_id": testNetwork, "observed_at": time.Now(), "stale_after_seconds": 300,
		"agents": []map[string]any{{"peer_id": self, "name": "codex", "machine_name": "mbp", "reported_at": "2026-11-20T13:00:00Z", "report_status": "fresh", "activity": map[string]any{"state": state}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// shortTimings makes a source's stream timings short for tests.
var shortTimings = &streamTimings{linger: 50 * time.Millisecond, retry: 20 * time.Millisecond, refusedRetry: 20 * time.Millisecond}

// The node opens one signed stream, shared by every local reader, answers
// from its latest list, and closes it a little after the last reader left.
func TestNetworkAgentsSourceHoldsOneSignedStreamWhileRead(t *testing.T) {
	priv, self := testNodeKey(t)
	lines := make(chan string, 4)
	closed := make(chan struct{})
	control, calls := fakeNodeAgentsControl(t, self, func(w http.ResponseWriter, r *http.Request) {
		streamLines(w, r, lines)
		close(closed)
	})
	source := &networkAgentsSource{control: control.URL, network: testNetwork, priv: priv, timings: shortTimings}
	lines <- "" // a keepalive
	lines <- agentsLine(t, self, "idle")
	got, err := source.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.PeerID != self || len(got.Agents) != 1 || got.Agents[0].MachineName != "mbp" || got.Agents[0].Activity.State != "idle" {
		t.Fatalf("answer: %+v", got)
	}
	// While the stream is held, reads share it and see each change.
	done := source.subscribe()
	lines <- agentsLine(t, self, "busy")
	eventually(t, func() bool {
		got, err := source.get(context.Background())
		return err == nil && got.Agents[0].Activity.State == "busy"
	})
	if calls.Load() != 1 {
		t.Fatalf("%d streams for one node", calls.Load())
	}
	done()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("stream held after the last reader left")
	}
	if state, _ := source.current(); state.version != 0 {
		t.Fatalf("stale state kept after the stream closed: %+v", state)
	}
}

// The node says why it has no list: the control service refused the
// stream, removed the node while it watched, or could not be reached.
func TestNetworkAgentsSourceExplainsRefusals(t *testing.T) {
	priv, self := testNodeKey(t)
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter, r *http.Request)
		want   string
	}{
		{"removed", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) }, "this node was removed from the network"},
		{"unknown", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"unknown network","code":"unknown_network"}`)
		}, "the network does not know this node"},
		{"older control", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, "does not list network agents yet"},
		{"busy", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }, "busy"},
		{"failing", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, "Internal Server Error"},
		{"removed while watching", func(w http.ResponseWriter, r *http.Request) {
			lines := make(chan string, 1)
			lines <- `{"error":"this node was removed from the network","code":"node_removed"}`
			close(lines)
			streamLines(w, r, lines)
		}, "this node was removed from the network"},
		{"mesh failing", func(w http.ResponseWriter, r *http.Request) {
			lines := make(chan string, 1)
			lines <- `{"error":"agent catalog unavailable"}`
			streamLines(w, r, lines)
		}, "agent catalog unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control, _ := fakeNodeAgentsControl(t, self, tc.answer)
			source := &networkAgentsSource{control: control.URL, network: testNetwork, priv: priv, timings: shortTimings}
			_, err := source.get(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	source := &networkAgentsSource{control: "http://127.0.0.1:1", network: testNetwork, priv: priv, timings: shortTimings}
	if _, err := source.get(context.Background()); err == nil || !strings.Contains(err.Error(), "could not be reached") {
		t.Fatalf("unreachable: %v", err)
	}
	source = &networkAgentsSource{priv: priv, timings: shortTimings}
	if _, err := source.get(context.Background()); err == nil || !strings.Contains(err.Error(), "agentiscaled up") {
		t.Fatalf("not configured: %v", err)
	}
}

// A stream that worked and ended, as when the control service restarts, is
// opened again, and the list stays meanwhile.
func TestNetworkAgentsSourceReopensAnEndedStream(t *testing.T) {
	priv, self := testNodeKey(t)
	var opened atomic.Int32
	control, _ := fakeNodeAgentsControl(t, self, func(w http.ResponseWriter, r *http.Request) {
		lines := make(chan string, 1)
		state := "idle"
		if opened.Add(1) > 1 {
			state = "busy"
			streamLines(w, r, lines) // the second stream stays open
			return
		}
		lines <- agentsLine(t, self, state)
		close(lines)
		streamLines(w, r, lines)
	})
	source := &networkAgentsSource{control: control.URL, network: testNetwork, priv: priv, timings: shortTimings}
	done := source.subscribe()
	defer done()
	if _, err := source.get(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return opened.Load() == 2 })
	if got, err := source.get(context.Background()); err != nil || got.Agents[0].Activity.State != "idle" {
		t.Fatalf("list lost while the stream reopened: %+v %v", got, err)
	}
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// agent list -network asks the running node and shows each agent's state
// and machine, or the node's reason when it can't list them.
func TestAgentListNetwork(t *testing.T) {
	dir := shortTempDir(t)
	list := func(args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		err := runAgent(context.Background(), append([]string{"list", "-network", "-data-dir", dir}, args...), &out)
		return out.String(), err
	}
	if _, err := list(); err == nil || !strings.Contains(err.Error(), "agentiscaled up") {
		t.Fatalf("not joined: %v", err)
	}
	writeSettings(t, dir, settings{Control: "https://control.test", PeerID: "12D3KooWSelf", NetworkID: testNetwork})
	if _, err := list(); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("no node: %v", err)
	}
	if err := runAgent(context.Background(), []string{"list", "-json", "-data-dir", dir}, &bytes.Buffer{}); err == nil {
		t.Fatal("-json without -network accepted")
	}

	listener, err := listenAgentSocket(filepath.Join(dir, localAgentSocketName))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	since, waitingSince, seen := now.Add(-3*time.Minute), now.Add(-42*time.Second), now.Add(-12*time.Minute)
	answer := networkAgents{NetworkID: testNetwork, PeerID: "12D3KooWSelf", ObservedAt: now, StaleAfterSeconds: 300, Agents: []networkAgent{
		{PeerID: "12D3KooWSelf", Name: "codex", ReportedAt: now, ReportStatus: "fresh", Activity: &agentActivity{State: activityBusy, Since: &since}},
		{PeerID: "12D3KooWAnotherMachine1", MachineName: "artur-mbp", Name: "reviewer", ReportedAt: now, ReportStatus: "fresh", Activity: &agentActivity{State: activityIdle, Waiting: 1, WaitingSince: &waitingSince}},
		{PeerID: "12D3KooWAnotherMachine2", Name: "writer", ReportedAt: now, ReportStatus: "fresh"},
		{PeerID: "12D3KooWAnotherMachine2", Name: "gone", ReportedAt: seen, ReportStatus: "stale"},
	}}
	var current atomic.Value
	current.Store(answer)
	var failure atomic.Value
	failure.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /network/agents", func(w http.ResponseWriter, r *http.Request) {
		if message := failure.Load().(string); message != "" {
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
			return
		}
		_ = json.NewEncoder(w).Encode(current.Load().(networkAgents))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })

	out, err := list()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"NAME      STATE",
		"codex     busy (3m)",
		"this machine",
		"reviewer  waiting for an answer (42s)",
		"artur-mbp",
		"writer    idle",
		"gone      offline (last seen 12m ago)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	out, err = list("-json")
	if err != nil {
		t.Fatal(err)
	}
	var decoded networkAgents
	if err := json.Unmarshal([]byte(out), &decoded); err != nil || len(decoded.Agents) != 4 || decoded.Agents[3].ReportStatus != "stale" {
		t.Fatalf("json: %v\n%s", err, out)
	}
	failure.Store("this node was removed from the network")
	if _, err := list(); err == nil || err.Error() != "this node was removed from the network" {
		t.Fatalf("refusal: %v", err)
	}
	answer.Agents = nil
	current.Store(answer)
	failure.Store("")
	if out, err := list(); err != nil || !strings.Contains(out, "No machine offers an agent") {
		t.Fatalf("empty: %q %v", out, err)
	}
}

// A watch sends the list once known, then again only when it changes, and
// says why while it can't be read.
func TestNetworkAgentsWatchStreamsChanges(t *testing.T) {
	priv, self := testNodeKey(t)
	lines := make(chan string, 4)
	control, _ := fakeNodeAgentsControl(t, self, func(w http.ResponseWriter, r *http.Request) {
		streamLines(w, r, lines)
	})
	source := &networkAgentsSource{control: control.URL, network: testNetwork, priv: priv, timings: shortTimings}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveNetworkAgents(w, r, source)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/network/agents?watch=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(resp.Body)
	next := func() map[string]any {
		t.Helper()
		if !scanner.Scan() {
			t.Fatalf("stream ended: %v", scanner.Err())
		}
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("line %q: %v", scanner.Text(), err)
		}
		return line
	}
	stateOf := func(line map[string]any) string {
		agents := line["agents"].([]any)
		return agents[0].(map[string]any)["activity"].(map[string]any)["state"].(string)
	}
	lines <- agentsLine(t, self, "idle")
	if first := next(); stateOf(first) != "idle" || first["peer_id"] != self {
		t.Fatalf("first: %v", first)
	}
	// The same list again (only observed_at moves) sends nothing.
	lines <- agentsLine(t, self, "idle")
	lines <- agentsLine(t, self, "busy")
	if second := next(); stateOf(second) != "busy" {
		t.Fatalf("second: %v", second)
	}
	lines <- `{"error":"this node was removed from the network","code":"node_removed"}`
	if third := next(); third["error"] != "this node was removed from the network" {
		t.Fatalf("third: %v", third)
	}
}
