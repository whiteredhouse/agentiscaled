package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
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
func fakeNodeAgentsControl(t *testing.T, peerID string, answer func(w http.ResponseWriter)) (*httptest.Server, *atomic.Int32) {
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
		answer(w)
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

// The node asks the control service, signed with its key, and keeps the
// answer briefly so local callers share one question.
func TestNetworkAgentsSourceAsksControlSignedAndKeepsTheAnswer(t *testing.T) {
	priv, self := testNodeKey(t)
	since := time.Now().Add(-3 * time.Minute).UTC().Truncate(time.Second)
	control, calls := fakeNodeAgentsControl(t, self, func(w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"network_id": testNetwork, "observed_at": time.Now(), "stale_after_seconds": 300,
			"agents": []map[string]any{
				{"peer_id": self, "name": "codex", "description": "Writes code", "reported_at": time.Now(), "report_status": "fresh", "activity": map[string]any{"state": "busy", "since": since, "waiting": 0}},
				{"peer_id": "12D3KooWOther", "name": "reviewer", "reported_at": time.Now().Add(-time.Hour), "report_status": "stale"},
			},
		})
	})
	clock := time.Now()
	source := &networkAgentsSource{control: control.URL + "/", network: testNetwork, priv: priv, now: func() time.Time { return clock }}
	got, err := source.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.PeerID != self || got.StaleAfterSeconds != 300 || len(got.Agents) != 2 {
		t.Fatalf("answer: %+v", got)
	}
	codex := got.Agents[0]
	if !codex.available() || codex.Activity == nil || codex.Activity.State != activityBusy || !codex.Activity.Since.Equal(since) {
		t.Fatalf("codex: %+v", codex)
	}
	if got.Agents[1].available() {
		t.Fatal("stale agent shown available")
	}
	if _, err := source.get(context.Background()); err != nil || calls.Load() != 1 {
		t.Fatalf("second get within the interval asked again (%d calls, %v)", calls.Load(), err)
	}
	clock = clock.Add(networkAgentsTTL)
	if _, err := source.get(context.Background()); err != nil || calls.Load() != 2 {
		t.Fatalf("get after the interval: %d calls, %v", calls.Load(), err)
	}
}

func TestNetworkAgentsSourceExplainsRefusals(t *testing.T) {
	priv, self := testNodeKey(t)
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   int
		says   string
	}{
		{"removed", http.StatusGone, `{"code":"node_removed"}`, http.StatusGone, "removed"},
		{"unknown", http.StatusNotFound, `{"code":"unknown_network"}`, http.StatusNotFound, "does not know this node"},
		{"old control", http.StatusNotFound, `404 page not found`, http.StatusBadGateway, "does not list network agents yet"},
		{"outage", http.StatusServiceUnavailable, `{"error":"agents temporarily unavailable"}`, http.StatusBadGateway, "Service Unavailable"},
		{"other network", http.StatusOK, `{"network_id":"00000000-0000-0000-0000-000000000000","agents":[]}`, http.StatusBadGateway, "another network"},
		{"garbage", http.StatusOK, `not json`, http.StatusBadGateway, "unreadable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			control, _ := fakeNodeAgentsControl(t, self, func(w http.ResponseWriter) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			})
			source := &networkAgentsSource{control: control.URL, network: testNetwork, priv: priv}
			_, err := source.get(context.Background())
			var known *errNetworkAgents
			if !errors.As(err, &known) || known.status != c.want || !strings.Contains(known.message, c.says) {
				t.Fatalf("err = %v", err)
			}
			// A refusal is kept for the interval like an answer.
			if source.last != nil || source.lastErr == nil {
				t.Fatal("refusal not kept as a refusal")
			}
		})
	}
	source := &networkAgentsSource{network: testNetwork, priv: priv}
	if _, err := source.get(context.Background()); err == nil || !strings.Contains(err.Error(), "agentiscaled up") {
		t.Fatalf("no control service: %v", err)
	}
	unreachable := &networkAgentsSource{control: "http://127.0.0.1:1", network: testNetwork, priv: priv}
	var known *errNetworkAgents
	if _, err := unreachable.get(context.Background()); !errors.As(err, &known) || known.status != http.StatusServiceUnavailable {
		t.Fatalf("unreachable: %v", err)
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

// A watch sends the list at once, then again only when it changes, and
// says why while it can't be read.
func TestNetworkAgentsWatchStreamsChanges(t *testing.T) {
	ttl, interval := networkAgentsTTL, networkWatchInterval
	networkAgentsTTL, networkWatchInterval = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { networkAgentsTTL, networkWatchInterval = ttl, interval })
	priv, self := testNodeKey(t)
	var version atomic.Int32
	control, _ := fakeNodeAgentsControl(t, self, func(w http.ResponseWriter) {
		switch version.Load() {
		case 2:
			w.WriteHeader(http.StatusGone)
			return
		}
		state := "idle"
		if version.Load() == 1 {
			state = "busy"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"network_id": testNetwork, "observed_at": time.Now(), "stale_after_seconds": 300,
			"agents": []map[string]any{{"peer_id": self, "name": "codex", "reported_at": "2026-11-20T13:00:00Z", "report_status": "fresh", "activity": map[string]any{"state": state}}},
		})
	})
	source := &networkAgentsSource{control: control.URL, network: testNetwork, priv: priv}
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
	lines := bufio.NewScanner(resp.Body)
	next := func() map[string]any {
		t.Helper()
		if !lines.Scan() {
			t.Fatalf("stream ended: %v", lines.Err())
		}
		var line map[string]any
		if err := json.Unmarshal(lines.Bytes(), &line); err != nil {
			t.Fatalf("line %q: %v", lines.Text(), err)
		}
		return line
	}
	stateOf := func(line map[string]any) string {
		agents := line["agents"].([]any)
		return agents[0].(map[string]any)["activity"].(map[string]any)["state"].(string)
	}
	if first := next(); stateOf(first) != "idle" || first["peer_id"] != self {
		t.Fatalf("first: %v", first)
	}
	// Unchanged reads (only observed_at moves) send nothing.
	time.Sleep(60 * time.Millisecond)
	version.Store(1)
	if second := next(); stateOf(second) != "busy" {
		t.Fatalf("second: %v", second)
	}
	version.Store(2)
	if third := next(); third["error"] != "this node was removed from the network" {
		t.Fatalf("third: %v", third)
	}
}
