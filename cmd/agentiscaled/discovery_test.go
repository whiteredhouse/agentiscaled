package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/agentmesh/api"
	"github.com/google/uuid"
)

// Use the same authenticated transport as a delegated agent. A location
// filter must query only that backend and never upgrade to the owner's token.
func TestListAgentsByLocationAndCaller(t *testing.T) {
	const token = "delegated-test-token"
	cloudID := uuid.NewString()
	selfPeer, meshPeer := randomPeer(t), randomPeer(t)
	var mu sync.Mutex
	var paths []string
	var denyCloud atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(localAgentInfo{PeerID: selfPeer, Services: []string{"caller", "review"}, Hosted: true})
	})
	mux.HandleFunc("GET "+hostedPath, func(w http.ResponseWriter, _ *http.Request) {
		if denyCloud.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprintf(w, `{"agents":[{"id":%q,"name":"review"}]}`, cloudID)
	})
	mux.HandleFunc("GET /mesh/service/discover", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("type") != "a2a" || q.Get("name") != "" {
			t.Errorf("unexpected mesh query: %v", q)
		}
		one := map[string]string{"srv_name": "review", "peer_id": meshPeer}
		json.NewEncoder(w).Encode([]map[string]string{one, one,
			{"srv_name": "caller", "peer_id": selfPeer},
			{"srv_name": "review", "peer_id": cloudTargetPrefix + cloudID},
		})
	})
	socket := filepath.Join(shortTempDir(t), "agent.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(api.HeaderMeshAuthentication) != "Bearer "+token {
			t.Error("missing delegated authorization for", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		mux.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	ctx := context.Background()
	client := meshTokenClient(ctx, socket, token)
	tasks := &a2aTasks{self: "caller", http: client, base: "http://agentiscaled", local: client}
	check := func(location string, want []agentRow, calls ...string) {
		t.Helper()
		_, out, err := tasks.listAgents(ctx, nil, listAgentsIn{Location: location})
		if err != nil || !slices.Equal(out.Agents, want) || out.Warning != "" {
			t.Fatalf("list(%q) returned %+v, %v", location, out, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(paths, calls) {
			t.Fatalf("list(%q) queried %v, want %v", location, paths, calls)
		}
		paths = nil
	}
	local := agentRow{Service: "review", Location: "local"}
	mesh := agentRow{Service: "review", Location: "mesh", PeerID: meshPeer}
	cloud := agentRow{Service: "review", Location: "cloud", PeerID: cloudTargetPrefix + cloudID}
	check("cloud", []agentRow{cloud}, hostedPath)
	check("mesh", []agentRow{mesh}, "/info", "/mesh/service/discover")
	check("local", []agentRow{local}, "/info")
	check("", []agentRow{cloud, local, mesh}, "/info", hostedPath, "/mesh/service/discover")

	if _, _, err := tasks.listAgents(ctx, nil, listAgentsIn{Location: "hosted"}); err == nil {
		t.Fatal("unknown location accepted")
	}
	denyCloud.Store(true)
	if _, _, err := tasks.listAgents(ctx, nil, listAgentsIn{Location: "cloud"}); err == nil {
		t.Fatal("cloud permission error hidden")
	}
	_, partial, err := tasks.listAgents(ctx, nil, listAgentsIn{})
	if err != nil || !strings.Contains(partial.Warning, "403") || !slices.Equal(partial.Agents, []agentRow{local, mesh}) {
		t.Fatalf("partial results %+v, %v", partial, err)
	}
}
