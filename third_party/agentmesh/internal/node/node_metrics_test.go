// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package node

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
)

func getMetricsBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestNodeMetricsServer(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	// Half-built, as a node is between NewAgentMeshNode and Start: alive, not
	// ready, and nothing read from a host that does not exist yet.
	node := &AgentMeshNode{Store: store, services: NewServiceRegistry(&fakeDHT{}, 0)}
	srv, err := StartMetricsServer(node, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	base := "http://" + srv.Addr
	if base == "http://" {
		t.Fatal("metrics server did not record its address")
	}

	if code, _ := getMetricsBody(t, base+"/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d before ready", code)
	}
	if code, _ := getMetricsBody(t, base+"/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d before ready, want 503", code)
	}
	code, body := getMetricsBody(t, base+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d: %s", code, body)
	}
	if !strings.Contains(body, "agentmesh_node_ready 0") {
		t.Error("/metrics missing agentmesh_node_ready 0 before start")
	}
	if strings.Contains(body, "agentmesh_node_connected_peers") {
		t.Error("/metrics exported host state before the host existed")
	}

	host, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Close() }()
	expiry := time.Now().Add(time.Hour).Unix()
	if err := store.SaveIdentityExpiration(expiry); err != nil {
		t.Fatal(err)
	}
	node.Host = host
	node.Directory = &routerDirectory{host: host, routers: node.attachedRouters, addrs: host.Addrs}
	node.authPeers.Store(peer.ID("peer-a"), time.Now().Add(time.Hour))
	node.authPeers.Store(peer.ID("peer-b"), time.Now().Add(time.Hour))

	// Ready, but no authenticated router yet: the kubelet must not route to it.
	if code, _ := getMetricsBody(t, base+"/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d with no router, want 503", code)
	}
	_, body = getMetricsBody(t, base+"/metrics")
	for _, want := range []string{
		"agentmesh_node_ready 1",
		"agentmesh_node_mesh_connected 0",
		"agentmesh_node_connected_peers 0",
		"agentmesh_node_authenticated_peers 2",
		"agentmesh_node_conns_per_source_ip_limit 100",
		`agentmesh_node_inbound_connections_refused_total{reason="per_ip_limit"} 0`,
		"agentmesh_node_biscuit_expiry_timestamp_seconds " + strconv.FormatFloat(float64(expiry), 'g', -1, 64),
		// The process-wide registry rides along: sidecar counters and libp2p.
		"agentmesh_node_requests_in_flight",
		"libp2p_",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
	if strings.Contains(body, "agentmesh_node_services_registered") {
		t.Error("services gauge exported with an empty registry")
	}

	// An authenticated router that is actually connected flips both.
	routerHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = routerHost.Close() }()
	if err := host.Connect(t.Context(), peer.AddrInfo{ID: routerHost.ID(), Addrs: routerHost.Addrs()}); err != nil {
		t.Fatal(err)
	}
	node.mu.Lock()
	node.authenticatedRouters = map[peer.ID]bool{routerHost.ID(): true}
	node.mu.Unlock()

	if code, _ := getMetricsBody(t, base+"/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d once a router is authenticated", code)
	}
	_, body = getMetricsBody(t, base+"/metrics")
	for _, want := range []string{"agentmesh_node_mesh_connected 1", "agentmesh_node_connected_peers 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

func TestStartMetricsServerRequiresAddress(t *testing.T) {
	if _, err := StartMetricsServer(&AgentMeshNode{}, ""); err == nil {
		t.Fatal("expected an error for an empty address")
	}
}
