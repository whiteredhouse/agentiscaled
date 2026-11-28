package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/whiteredhouse/agentiscaled/internal/caller"
)

func serveDiscoveryFixture(t *testing.T, dir, socket string, handler http.HandlerFunc) {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(dir, socket))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
}

func TestNetworkAgentListIncludesLocalAndRemote(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	local, first, second := randomPeer(t), randomPeer(t), randomPeer(t)
	serveDiscoveryFixture(t, dir, localAgentSocketName, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			t.Errorf("unexpected local route %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(localAgentInfo{PeerID: local, Services: []caller.AgentName{"helper"}})
	})
	providers := []*api.DiscoveredProvider{
		{SrvName: "helper", PeerId: local},
		{SrvName: "helper", PeerId: first, SrvDescription: "a helper"},
		{SrvName: "helper", PeerId: first, SrvDescription: "duplicate description"},
		{SrvName: "helper", PeerId: second},
		{SrvName: "../unsafe", PeerId: first},
		{SrvName: "unknown", PeerId: ""},
	}
	for i := range 125 {
		providers = append(providers, &api.DiscoveredProvider{SrvName: fmt.Sprintf("agent-%03d", i), PeerId: first})
	}
	serveDiscoveryFixture(t, dir, meshnode.DefaultSocketName, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/mesh/service/discover" || q.Get("type") != "a2a" || q.Get("name") != "" {
			t.Errorf("unexpected discovery route %s", r.URL)
		}
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 {
			limit = 20
		}
		json.NewEncoder(w).Encode(providers[:min(len(providers), limit)])
	})
	list := func() []caller.AgentRow {
		t.Helper()
		var out bytes.Buffer
		if err := caller.RunNetwork(context.Background(), []string{"agent", "list", "-data-dir", dir, "-json"}, nil, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		var result caller.AgentList
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Agents
	}
	got := list()
	want := []caller.AgentRow{
		{Service: "helper"},
		{Service: "helper", PeerID: first, Description: "a helper"},
		{Service: "helper", PeerID: second},
		{Service: "agent-124", PeerID: first},
	}
	for _, row := range want {
		if !slices.Contains(got, row) {
			t.Errorf("missing %+v", row)
		}
	}
	if len(got) != 128 {
		t.Fatalf("got %d agents, want 128", len(got))
	}
}

// Drained agents stay listed, with their drain, here and on other nodes.
func TestNetworkAgentListShowsDrains(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	local, remote := randomPeer(t), randomPeer(t)
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	serveDiscoveryFixture(t, dir, localAgentSocketName, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(localAgentInfo{PeerID: local, Services: []caller.AgentName{"helper", "open"}, Draining: map[caller.AgentName]drainMark{"helper": {Reason: "re-login", Since: since, By: "node", Drained: true}}})
	})
	serveDiscoveryFixture(t, dir, meshnode.DefaultSocketName, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]*api.DiscoveredProvider{
			{SrvName: "reviewer", PeerId: remote, SrvDescription: "reviews", SrvMetadata: map[string]string{caller.DrainMetadataKey: `{"reason":"reboot","since":"2026-01-02T03:04:05Z","by":"node"}`}},
			{SrvName: "idle", PeerId: remote, SrvMetadata: map[string]string{caller.DrainMetadataKey: `{"since":"2026-01-02T03:04:05Z","by":"node","drained":true}`}},
			{SrvName: "broken", PeerId: remote, SrvMetadata: map[string]string{caller.DrainMetadataKey: "not json"}},
			{SrvName: "other", PeerId: remote, SrvMetadata: map[string]string{"something.else": "x"}},
		})
	})
	var out bytes.Buffer
	if err := caller.RunNetwork(context.Background(), []string{"agent", "list", "-data-dir", dir, "-json"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result caller.AgentList
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	drains := map[string]*caller.Drain{}
	for _, row := range result.Agents {
		drains[row.Service.String()] = row.Draining
	}
	want := map[string]*caller.Drain{
		"helper":   {Reason: "re-login", Since: since, By: "node", Drained: true},
		"open":     nil,
		"reviewer": {Reason: "reboot", Since: since, By: "node"},
		"idle":     {Since: since, By: "node", Drained: true},
		"broken":   {},
		"other":    nil,
	}
	for name, w := range want {
		got, ok := drains[name]
		if !ok || (got == nil) != (w == nil) || (got != nil && *got != *w) {
			t.Errorf("%s: %+v, want %+v", name, got, w)
		}
	}
	out.Reset()
	if err := caller.RunNetwork(context.Background(), []string{"agent", "list", "-data-dir", dir}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	// The STATE column uses agent list's words: draining while work is
	// left, which a node too old to say always is, then drained.
	ago := `\(\d+h\d+m\)`
	for _, line := range []string{
		`(?m)^AGENT +PEER +STATE +RUNTIME +MODEL +DESCRIPTION$`,
		`(?m)^helper +local +drained ` + ago + `: re-login +- +- *$`,
		`(?m)^open +local +available +- +- *$`,
		`(?m)^reviewer +\S+ +draining ` + ago + `: reboot +- +- +reviews$`,
		`(?m)^idle +\S+ +drained ` + ago + ` +- +- *$`,
		`(?m)^broken +\S+ +draining +- +- *$`,
		`(?m)^other +\S+ +available +- +- *$`,
	} {
		if !regexp.MustCompile(line).MatchString(out.String()) {
			t.Errorf("table lacks %s:\n%s", line, out.String())
		}
	}
}

func TestNetworkAgentListShowsRuntime(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	local, remote := randomPeer(t), randomPeer(t)
	serveDiscoveryFixture(t, dir, localAgentSocketName, func(w http.ResponseWriter, r *http.Request) {
		// A raw document, as a node of any version could send it.
		fmt.Fprintf(w, `{"peer_id":%q,"services":["helper","plain"],"runtimes":{"helper":"claude","plain":"Not A Word","Bad/Name":"codex"}}`, local)
	})
	serveDiscoveryFixture(t, dir, meshnode.DefaultSocketName, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]*api.DiscoveredProvider{
			{SrvName: "reviewer", PeerId: remote, SrvMetadata: map[string]string{caller.RuntimeMetadataKey: "codex"}},
			{SrvName: "odd", PeerId: remote, SrvMetadata: map[string]string{caller.RuntimeMetadataKey: "codex\x1b[2J"}},
			{SrvName: "old", PeerId: remote},
		})
	})
	var out bytes.Buffer
	if err := caller.RunNetwork(context.Background(), []string{"agent", "list", "-data-dir", dir, "-json"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result caller.AgentList
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, row := range result.Agents {
		got[row.Service.String()] = row.Runtime
	}
	want := map[string]string{"helper": "claude", "plain": "", "reviewer": "codex", "odd": "", "old": ""}
	if !maps.Equal(got, want) {
		t.Fatalf("runtimes = %v, want %v", got, want)
	}
	out.Reset()
	if err := caller.RunNetwork(context.Background(), []string{"agent", "list", "-data-dir", dir}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[0], "RUNTIME") {
		t.Errorf("header lacks RUNTIME: %q", lines[0])
	}
	for name, runtime := range map[string]string{"helper": "claude", "reviewer": "codex", "old": "-"} {
		found := false
		for _, line := range lines {
			if f := strings.Fields(line); len(f) >= 4 && f[0] == name && f[3] == runtime {
				found = true
			}
		}
		if !found {
			t.Errorf("no row %s with runtime %s:\n%s", name, runtime, out.String())
		}
	}
}

// Each agent shows the model it asks for: the one its definition sets,
// "default" when it runs its runtime's, and "-" when its node does not say
// or says something that is not a model.
func TestNetworkAgentListShowsModel(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	local, remote := randomPeer(t), randomPeer(t)
	serveDiscoveryFixture(t, dir, localAgentSocketName, func(w http.ResponseWriter, r *http.Request) {
		// A raw document, as a node of any version could send it.
		fmt.Fprintf(w, `{"peer_id":%q,"services":["helper","plain","old-local","null-local"],"models":{"helper":"sonnet","plain":"","old-local":7,"null-local":null,"Bad/Name":"opus"}}`, local)
	})
	serveDiscoveryFixture(t, dir, meshnode.DefaultSocketName, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]*api.DiscoveredProvider{
			{SrvName: "reviewer", PeerId: remote, SrvMetadata: map[string]string{caller.ModelMetadataKey: "gpt-5.1-codex"}},
			{SrvName: "bedrock", PeerId: remote, SrvMetadata: map[string]string{caller.ModelMetadataKey: "us.anthropic.claude-opus-4-1-20250805-v1:0"}},
			{SrvName: "default", PeerId: remote, SrvMetadata: map[string]string{caller.ModelMetadataKey: ""}},
			{SrvName: "odd", PeerId: remote, SrvMetadata: map[string]string{caller.ModelMetadataKey: "opus\x1b[2J"}},
			{SrvName: "old", PeerId: remote},
		})
	})
	var out bytes.Buffer
	if err := caller.RunNetwork(context.Background(), []string{"agent", "list", "-data-dir", dir, "-json"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result caller.AgentList
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, row := range result.Agents {
		got[row.Service.String()] = caller.ShownModel(row.Model)
	}
	want := map[string]string{"helper": "sonnet", "plain": "default", "old-local": "-", "null-local": "-", "reviewer": "gpt-5.1-codex",
		"bedrock": "us.anthropic.claude-opus-4-1-20250805-v1:0", "default": "default", "odd": "-", "old": "-"}
	if !maps.Equal(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	// The JSON tells the runtime's default ("") from a node that does not
	// say (no key).
	if !strings.Contains(out.String(), `{"service":"plain","model":""}`) || !strings.Contains(out.String(), `{"service":"old-local"}`) ||
		!strings.Contains(out.String(), `{"service":"null-local"}`) {
		t.Fatalf("json: %s", out.String())
	}
	out.Reset()
	if err := caller.RunNetwork(context.Background(), []string{"agent", "list", "-data-dir", dir}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if f := strings.Fields(lines[0]); len(f) < 5 || f[4] != "MODEL" {
		t.Fatalf("header: %q", lines[0])
	}
	for name, model := range want {
		found := false
		for _, line := range lines {
			if f := strings.Fields(line); len(f) >= 5 && f[0] == name && f[4] == model {
				found = true
			}
		}
		if !found {
			t.Errorf("no row %s with model %s:\n%s", name, model, out.String())
		}
	}
}

func TestNetworkAgentListReadsLargeLocalCatalog(t *testing.T) {
	t.Parallel()
	var names []string
	for i := range 400 {
		names = append(names, fmt.Sprintf("local-agent-%03d", i))
	}
	for _, tc := range []struct {
		name     string
		services []string
		wantErr  string
	}{
		{"many agents", names, ""},
		{"over 1 MiB", []string{strings.Repeat("x", 1<<20)}, "local agent catalog exceeds 1 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mesh := newFakeNetwork(t)
			info := map[string]any{"peer_id": randomPeer(t), "services": tc.services}
			serveDiscoveryFixture(t, mesh.dir, localAgentSocketName, func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(info)
			})
			var out bytes.Buffer
			err := caller.RunNetwork(t.Context(), []string{"agent", "list", "-json", "-data-dir", mesh.dir}, nil, &out, io.Discard)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || out.Len() != 0 {
					t.Fatalf("oversized catalog: stdout %d bytes, error %v", out.Len(), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got caller.AgentList
			if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Agents) != len(names) {
				t.Fatalf("read %d local agents: %v", len(got.Agents), err)
			}
			for i, name := range names {
				if got.Agents[i] != (caller.AgentRow{Service: caller.AgentName(name)}) {
					t.Fatalf("row %d: got %+v, want local agent %s", i, got.Agents[i], name)
				}
			}
		})
	}
}

func TestNetworkAgentCardFollowsTaskPeerSelection(t *testing.T) {
	t.Parallel()
	mesh := newFakeNetwork(t)
	helper := newAgentHarness(t, testSender)
	a, b := randomPeer(t), randomPeer(t)
	mesh.offer(a)
	mesh.offer(b)
	mesh.route(b, helper.url)
	var out bytes.Buffer
	args := []string{"agent", "card", "helper", "-data-dir", mesh.dir}
	if err := caller.RunNetwork(context.Background(), args, nil, &out, io.Discard); err == nil || !strings.Contains(err.Error(), "pick one with -peer") {
		t.Fatalf("ambiguous card: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout after ambiguous lookup: %s", &out)
	}
	if err := caller.RunNetwork(context.Background(), append(args, "-peer", b), nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var card struct{ Name string }
	if err := json.Unmarshal(out.Bytes(), &card); err != nil || card.Name == "" {
		t.Fatalf("card %q: %v", &out, err)
	}
}

func TestNetworkAgentCardReadsLocalAgent(t *testing.T) {
	t.Parallel()
	_, _, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	var out bytes.Buffer
	if err := caller.RunNetwork(t.Context(), []string{"agent", "card", "helper", "-data-dir", dir}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var card struct{ Name string }
	if err := json.Unmarshal(out.Bytes(), &card); err != nil || card.Name != "helper" {
		t.Fatalf("local card %q: %v", &out, err)
	}
}

func TestNetworkAgentListOnCallerOnlyNode(t *testing.T) {
	t.Parallel()
	mesh := newFakeNetwork(t)
	var out bytes.Buffer
	if err := caller.RunNetwork(t.Context(), []string{"agent", "list", "-json", "-data-dir", mesh.dir}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != `{"agents":[]}` {
		t.Fatalf("empty list: %s", got)
	}
}
