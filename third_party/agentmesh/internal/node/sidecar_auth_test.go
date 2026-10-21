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
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
)

// TestConstantTimeEqual is a correctness check, not a timing one: a timing
// assertion in a unit test is flaky enough to be worse than nothing. What it
// pins is that swapping the comparison for a constant-time one did not change
// the answer for any of the shapes the plain == used to handle, in particular
// that a prefix of the token is still not the token.
func TestConstantTimeEqual(t *testing.T) {
	const token = "s3cret-sidecar-token"

	tests := []struct {
		name string
		got  string
		want bool
	}{
		{name: "identical", got: token, want: true},
		{name: "empty against a secret", got: ""},
		{name: "prefix", got: token[:len(token)-1]},
		{name: "longer", got: token + "x"},
		{name: "same length, one byte off", got: "s3cret-sidecar-tokeN"},
		{name: "different case", got: "S3CRET-SIDECAR-TOKEN"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := constantTimeEqual(tt.got, token); got != tt.want {
				t.Errorf("constantTimeEqual(%q, %q) = %v, want %v", tt.got, token, got, tt.want)
			}
		})
	}

	// Two empty strings hash alike, so this holds; withAuth never reaches here
	// with an empty configured token, but the function should not pretend.
	if !constantTimeEqual("", "") {
		t.Error("constantTimeEqual(\"\", \"\") = false, want true")
	}
}

// TestSidecarReadyzFollowsTheMeshConnection pins that the sidecar's /readyz
// says what the metrics listener's does: 503 until the node holds an
// authenticated, connected router, then 200, on the socket and on TCP. A
// harness that reads readiness over the socket, as agentmesh-bench does so
// that a member binds no loopback port, learns the same thing a kubelet
// probing the metrics port would.
func TestSidecarReadyzFollowsTheMeshConnection(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	host, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Close() }()
	node := &AgentMeshNode{
		Host:           host,
		Store:          store,
		BiscuitTimeout: 500 * time.Millisecond,
		services:       NewServiceRegistry(&fakeDHT{}, 0),
	}
	node.Directory = &routerDirectory{host: host, routers: node.attachedRouters, addrs: host.Addrs}
	socketPath := filepath.Join(t.TempDir(), "agentmesh.sock")
	srv, err := StartSidecarServer(node, "127.0.0.1:0", socketPath, "test-token", "", "", "")
	if err != nil {
		t.Fatalf("failed to start sidecar server: %v", err)
	}
	defer func() { _ = srv.Close() }()
	socket := waitForSocket(t, socketPath)
	tcp := &http.Client{Timeout: 2 * time.Second}

	readyz := func(t *testing.T, client *http.Client, url string) int {
		t.Helper()
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	for name, url := range map[string]string{"socket": "http://localhost/readyz", "tcp": "http://" + node.BoundHTTPAddr + "/readyz"} {
		client := socket
		if name == "tcp" {
			client = tcp
		}
		if code := readyz(t, client, url); code != http.StatusServiceUnavailable {
			t.Errorf("%s /readyz = %d with no authenticated router, want 503", name, code)
		}
	}

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
	for name, url := range map[string]string{"socket": "http://localhost/readyz", "tcp": "http://" + node.BoundHTTPAddr + "/readyz"} {
		client := socket
		if name == "tcp" {
			client = tcp
		}
		if code := readyz(t, client, url); code != http.StatusOK {
			t.Errorf("%s /readyz = %d once a router is authenticated and connected, want 200", name, code)
		}
	}
}

// TestMetricsGatedOnTCPButNotOnTheSocket pins where the metrics endpoint sits in
// the sidecar's trust model. Its labels carry peer IDs and per-peer request
// counts, and the TCP listener is reachable by any local process, so it is
// gated there. Reaching the socket already proves the caller owns it, which is
// the same bar as reading the token file, and every scrape in this repo goes
// that way.
func TestMetricsGatedOnTCPButNotOnTheSocket(t *testing.T) {
	node := &AgentMeshNode{
		BiscuitTimeout: 500 * time.Millisecond,
		services:       NewServiceRegistry(&fakeDHT{}, 0),
	}
	socketPath := filepath.Join(t.TempDir(), "agentmesh.sock")

	srv, err := StartSidecarServer(node, "127.0.0.1:0", socketPath, "test-token", "", "", "")
	if err != nil {
		t.Fatalf("failed to start sidecar server: %v", err)
	}
	defer func() { _ = srv.Close() }()

	client := waitForSocket(t, socketPath)

	resp, err := client.Get("http://localhost/metrics")
	if err != nil {
		t.Fatalf("socket metrics request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("untokened socket scrape: got status %d, want %d", resp.StatusCode, http.StatusOK)
	}

	tcpClient := &http.Client{Timeout: 2 * time.Second}
	tcpResp, err := tcpClient.Get("http://" + node.BoundHTTPAddr + "/metrics")
	if err != nil {
		t.Fatalf("tcp metrics request failed: %v", err)
	}
	defer func() { _ = tcpResp.Body.Close() }()
	if tcpResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("untokened TCP scrape: got status %d, want %d", tcpResp.StatusCode, http.StatusUnauthorized)
	}
}
