package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/whiteredhouse/agentiscaled/internal/caller"
	"github.com/whiteredhouse/agentiscaled/internal/nodestate"
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
	dir := shortTempDir(t)
	local, first, second := randomPeer(t), randomPeer(t), randomPeer(t)
	serveDiscoveryFixture(t, dir, localAgentSocketName, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			t.Errorf("unexpected local route %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(localAgentInfo{PeerID: local, Services: []string{"helper"}})
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
	if err := os.WriteFile(nodestate.StopFilePath(dir), []byte("2026-11-23T10:00:00Z\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got = list()
	if len(got) != 127 || slices.Contains(got, caller.AgentRow{Service: "helper"}) {
		t.Errorf("network list after stopping local agents: %d rows", len(got))
	}
}

func TestNetworkAgentListReadsLargeLocalCatalog(t *testing.T) {
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
			info := localAgentInfo{PeerID: randomPeer(t), Services: tc.services}
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
				if got.Agents[i] != (caller.AgentRow{Service: name}) {
					t.Fatalf("row %d: got %+v, want local agent %s", i, got.Agents[i], name)
				}
			}
		})
	}
}

func TestNetworkAgentCardFollowsTaskPeerSelection(t *testing.T) {
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
	mesh := newFakeNetwork(t)
	var out bytes.Buffer
	if err := caller.RunNetwork(t.Context(), []string{"agent", "list", "-json", "-data-dir", mesh.dir}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != `{"agents":[]}` {
		t.Fatalf("empty list: %s", got)
	}
}
