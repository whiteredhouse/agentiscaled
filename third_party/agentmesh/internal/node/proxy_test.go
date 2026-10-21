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
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/identity"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

func TestDatapathIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Setup: Create two in-memory nodes (Node A and Node B)
	privA, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	privB, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)

	dirA := t.TempDir()
	dirB := t.TempDir()

	storeA, err := NewStore(dirA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := storeA.Close(); err != nil {
			t.Logf("failed to close storeA: %v", err)
		}
	}()

	storeB, err := NewStore(dirB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := storeB.Close(); err != nil {
			t.Logf("failed to close storeB: %v", err)
		}
	}()

	// Pre-populate stores with dummy keys to avoid enrollment failure if required
	// For this test, we assume we can run without full enrollment if we bypass AuthHandler

	nodeA, err := NewAgentMeshNode(Options{
		PrivKey:           privA,
		RouterAddrs:       nil,
		Store:             storeA,
		DiscoveryInterval: "1s",
		ListenAddrs:       []string{"/ip4/127.0.0.1/tcp/0"},
		EnableRelay:       false,
		NodeConfig:        &NodeConfigComplete{},
		KeyGracePeriod:    24 * time.Hour,
		AllowLoopback:     true,
		MonitorBootstrap:  2 * time.Minute,
		MonitorInterval:   1 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeA.BiscuitTimeout = 500 * time.Millisecond
	if err := nodeA.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := nodeA.Host.Close(); err != nil {
			t.Logf("failed to close nodeA host: %v", err)
		}
	}()

	nodeB, err := NewAgentMeshNode(Options{
		PrivKey:           privB,
		RouterAddrs:       nil,
		Store:             storeB,
		DiscoveryInterval: "1s",
		ListenAddrs:       []string{"/ip4/127.0.0.1/tcp/0"},
		EnableRelay:       false,
		NodeConfig:        &NodeConfigComplete{},
		KeyGracePeriod:    24 * time.Hour,
		AllowLoopback:     true,
		MonitorBootstrap:  2 * time.Minute,
		MonitorInterval:   1 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeB.BiscuitTimeout = 500 * time.Millisecond
	if err := nodeB.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := nodeB.Host.Close(); err != nil {
			t.Logf("failed to close nodeB host: %v", err)
		}
	}()

	// Both nodes enrolled under one root: B (the caller proxy) authenticates
	// to A, and A must show B a verifiable identity before B forwards to it.
	enrollUnderRoot(t, nodeA, nodeB)

	// Connect Node B to Node A directly
	err = nodeB.Host.Connect(ctx, peer.AddrInfo{ID: nodeA.Host.ID(), Addrs: nodeA.Host.Addrs()})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Target Service: On Node A, start a dummy HTTP server
	expectedHeaderValue := "test-value"
	expectedBody := `{"status":"success"}`

	dummyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only assert the test header on the actual proxied request; MCP aggregation
		// probes use other paths (/sse, /message) and don't carry it.
		if r.URL.Path == "/api/v1/test" && r.Header.Get("X-Test-Header") != expectedHeaderValue {
			t.Errorf("Expected header X-Test-Header to be %s, got %s", expectedHeaderValue, r.Header.Get("X-Test-Header"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(expectedBody))
	}))
	defer dummyServer.Close()

	// 3. Registration: Node A registers this dummy server in its ServiceRegistry
	serviceName := "dummy-tool"
	serviceInfo := &api.ServiceInfo{
		Type: api.ServiceType_SERVICE_TYPE_MCP,
		Name: serviceName,
	}

	// We register it in the DHT and also setup the local handler.
	// We ignore the error because DHT Provide might fail if routing table is empty in this isolated test.
	_ = nodeA.RegisterService(ctx, &api.RegisterServiceRequest{
		Service: serviceInfo,
		Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: dummyServer.URL},
	})

	// Manually add to services map to ensure it's registered for the test lookup
	// because RegisterService might have failed due to DHT not being ready in this isolated test.
	targetURL, _ := url.Parse(dummyServer.URL)
	nodeA.services.insertService(&testService{
		info:    serviceInfo,
		handler: httputil.NewSingleHostReverseProxy(targetURL),
	})

	// Start the Sidecar server for Node B to get BoundHTTPAddr populated
	// We don't need full sidecar for Node B in this test if we call createEgressProxy directly,
	// but let's populate BoundHTTPAddr to avoid nil panics if used.
	nodeB.BoundHTTPAddr = "127.0.0.1:0" // Dummy

	// 4. Execution: Node B makes a request via its local Egress Proxy

	// We need to create the egress proxy for Node B.
	// The user request says: "Implement a reverse proxy on the local `agentmesh-node` HTTP server that intercepts requests to `/mesh/`"
	// In sidecar.go we have `createEgressProxy(node)`. We can use it here.

	proxyHandler := createEgressProxy(nodeB)

	// We start a test server for Node B's proxy to simulate the local agent calling it.
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	// Construct the URL targeting Node A's service
	// http://localhost:<port>/mesh/{peer_id}/{service_type}/{service_name}/{upstream_path}
	url := fmt.Sprintf("%s/mesh/%s/mcp/%s/api/v1/test", proxyServer.URL, nodeA.Host.ID().String(), serviceName)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-Header", expectedHeaderValue)

	client := &http.Client{}

	pidStr := nodeA.Host.ID().String()
	_, err = peer.Decode(pidStr)
	if err != nil {
		t.Fatalf("Failed to decode generated peer ID %s: %v", pidStr, err)
	}

	// Wait for DHT/routing to settle or just retry a few times if needed.
	// Since we connected directly, it should work immediately if the protocol is registered.

	var resp *http.Response
	for i := 0; i < 3; i++ {
		resp, err = client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			break
		}
		t.Logf("Attempt %d failed: %v, status: %v", i+1, err, resp)
		time.Sleep(1 * time.Second)
	}

	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("failed to close response body: %v", err)
		}
	}()

	// 5. Assertions
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status OK, got %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if string(bodyBytes) != expectedBody {
		t.Fatalf("Expected body %s, got %s", expectedBody, string(bodyBytes))
	}
}

func TestDatapathIntegration_Unauthenticated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	privA, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	privB, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)

	dirA := t.TempDir()
	dirB := t.TempDir()

	storeA, err := NewStore(dirA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storeA.Close() }()

	storeB, err := NewStore(dirB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storeB.Close() }()

	nodeA, err := NewAgentMeshNode(Options{
		PrivKey:           privA,
		RouterAddrs:       nil,
		Store:             storeA,
		DiscoveryInterval: "1s",
		ListenAddrs:       []string{"/ip4/127.0.0.1/tcp/0"},
		EnableRelay:       false,
		NodeConfig:        &NodeConfigComplete{},
		KeyGracePeriod:    24 * time.Hour,
		AllowLoopback:     true,
		MonitorBootstrap:  2 * time.Minute,
		MonitorInterval:   1 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeA.BiscuitTimeout = 500 * time.Millisecond
	if err := nodeA.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := nodeA.Host.Close(); err != nil {
			t.Logf("failed to close nodeA host: %v", err)
		}
	}()

	nodeB, err := NewAgentMeshNode(Options{
		PrivKey:           privB,
		RouterAddrs:       nil,
		Store:             storeB,
		DiscoveryInterval: "1s",
		ListenAddrs:       []string{"/ip4/127.0.0.1/tcp/0"},
		EnableRelay:       false,
		NodeConfig:        &NodeConfigComplete{},
		KeyGracePeriod:    24 * time.Hour,
		AllowLoopback:     true,
		MonitorBootstrap:  2 * time.Minute,
		MonitorInterval:   1 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeB.BiscuitTimeout = 500 * time.Millisecond
	if err := nodeB.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := nodeB.Host.Close(); err != nil {
			t.Logf("failed to close nodeB host: %v", err)
		}
	}()

	// NOTE: We do NOT setup an identity for Node B (caller).
	// This means the Egress Proxy will not inject an X-Mesh-Biscuit header,
	// and the request should be rejected by Node A's ingress server.

	// Connect Node B to Node A directly
	err = nodeB.Host.Connect(ctx, peer.AddrInfo{ID: nodeA.Host.ID(), Addrs: nodeA.Host.Addrs()})
	if err != nil {
		t.Fatal(err)
	}

	dummyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer dummyServer.Close()

	serviceName := "dummy-tool"
	serviceInfo := &api.ServiceInfo{
		Type: api.ServiceType_SERVICE_TYPE_MCP,
		Name: serviceName,
	}

	_ = nodeA.RegisterService(ctx, &api.RegisterServiceRequest{
		Service: serviceInfo,
		Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: dummyServer.URL},
	})

	targetURL, _ := url.Parse(dummyServer.URL)
	nodeA.services.insertService(&testService{
		info:    serviceInfo,
		handler: httputil.NewSingleHostReverseProxy(targetURL),
	})

	nodeB.BoundHTTPAddr = "127.0.0.1:0"

	proxyHandler := createEgressProxy(nodeB)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	url := fmt.Sprintf("%s/mesh/%s/mcp/%s/api/v1/test", proxyServer.URL, nodeA.Host.ID().String(), serviceName)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{}

	var resp *http.Response
	for i := 0; i < 3; i++ {
		resp, err = client.Do(req)
		if err == nil {
			break
		}
		t.Logf("Attempt %d failed: %v", i+1, err)
		time.Sleep(1 * time.Second)
	}

	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// ASSERTION: Should be service unavailable because the local node has no identity to send
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Expected status ServiceUnavailable (503), got %d", resp.StatusCode)
	}
}

func TestStdioDatapathIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Setup: Create two in-memory nodes (Node A and Node B)
	privA, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	privB, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)

	dirA := t.TempDir()
	dirB := t.TempDir()

	storeA, err := NewStore(dirA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storeA.Close() }()

	storeB, err := NewStore(dirB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storeB.Close() }()

	nodeA, err := NewAgentMeshNode(Options{
		PrivKey:           privA,
		RouterAddrs:       nil,
		Store:             storeA,
		DiscoveryInterval: "1s",
		ListenAddrs:       []string{"/ip4/127.0.0.1/tcp/0"},
		EnableRelay:       false,
		NodeConfig:        &NodeConfigComplete{},
		KeyGracePeriod:    24 * time.Hour,
		AllowLoopback:     true,
		MonitorBootstrap:  2 * time.Minute,
		MonitorInterval:   1 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeA.BiscuitTimeout = 500 * time.Millisecond
	if err := nodeA.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nodeA.Host.Close() }()

	nodeB, err := NewAgentMeshNode(Options{
		PrivKey:           privB,
		RouterAddrs:       nil,
		Store:             storeB,
		DiscoveryInterval: "1s",
		ListenAddrs:       []string{"/ip4/127.0.0.1/tcp/0"},
		EnableRelay:       false,
		NodeConfig:        &NodeConfigComplete{},
		KeyGracePeriod:    24 * time.Hour,
		AllowLoopback:     true,
		MonitorBootstrap:  2 * time.Minute,
		MonitorInterval:   1 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeB.BiscuitTimeout = 500 * time.Millisecond
	if err := nodeB.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nodeB.Host.Close() }()

	// Both nodes enrolled under one root (see TestDatapathIntegration).
	enrollUnderRoot(t, nodeA, nodeB)

	// Connect Node B to Node A directly
	err = nodeB.Host.Connect(ctx, peer.AddrInfo{ID: nodeA.Host.ID(), Addrs: nodeA.Host.Addrs()})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Target Service: Register a stdio service on Node A using 'cat'
	serviceName := "stdio-tool"
	serviceInfo := &api.ServiceInfo{
		Type: api.ServiceType_SERVICE_TYPE_MCP,
		Name: serviceName,
	}

	req := &api.RegisterServiceRequest{
		Service: serviceInfo,
		Backend: &api.RegisterServiceRequest_Command{
			Command: &api.CommandBackend{
				Command: []string{"cat"},
			},
		},
	}

	// Manually add to services map to ensure it's registered for the test lookup
	// and avoid double start by not calling RegisterService.
	handler, cmd, err := createStdioBridgeHandler(req.Backend.(*api.RegisterServiceRequest_Command).Command)
	if err != nil {
		t.Fatal(err)
	}
	nodeA.services.insertService(&testService{
		info:    serviceInfo,
		handler: handler,
	})

	defer func() {
		_ = nodeA.UnregisterService(ctx, serviceName)
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	// Start the Sidecar server for Node B to get BoundHTTPAddr populated
	nodeB.BoundHTTPAddr = "127.0.0.1:0" // Dummy

	// 3. Execution: Node B makes requests via its local Egress Proxy
	proxyHandler := createEgressProxy(nodeB)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	// Construct URLs
	// http://localhost:<port>/mesh/{peer_id}/{service_type}/{service_name}/{upstream_path}
	postURL := fmt.Sprintf("%s/mesh/%s/mcp/%s/", proxyServer.URL, nodeA.Host.ID().String(), serviceName)

	client := &http.Client{}

	// The bridge has no GET side any more: the legacy SSE stream broadcast
	// every backend line to every caller.
	getResp, err := client.Get(postURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = getResp.Body.Close()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on a command-backed service: got %d, want 405", getResp.StatusCode)
	}

	// The backend is `cat`, so the request comes straight back as the reply;
	// the bridge rewrote the id on the way in and must restore it on the way
	// out. Retried while DHT/routing settles.
	testMessage := `{"jsonrpc":"2.0","method":"ping","id":1}`
	var postResp *http.Response
	for i := 0; i < 3; i++ {
		postResp, err = client.Post(postURL, "application/json", bytes.NewBufferString(testMessage))
		if err == nil && postResp.StatusCode == http.StatusOK {
			break
		}
		t.Logf("POST attempt %d failed: %v, status: %v", i+1, err, postResp)
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = postResp.Body.Close() }()
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected POST status OK, got %d", postResp.StatusCode)
	}

	var echoed map[string]any
	if err := json.NewDecoder(postResp.Body).Decode(&echoed); err != nil {
		t.Fatal(err)
	}
	if echoed["method"] != "ping" || echoed["jsonrpc"] != "2.0" {
		t.Fatalf("echoed message = %v, want the ping request", echoed)
	}
	if id, _ := echoed["id"].(float64); id != 1 {
		t.Fatalf("echoed id = %v, want the caller's 1 (bridge id not restored)", echoed["id"])
	}

	cancel()
}

func TestDatapathHeadersAndRoutingTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	privA, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	privB, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)

	dirA := t.TempDir()
	dirB := t.TempDir()

	storeA, err := NewStore(dirA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storeA.Close() }()

	storeB, err := NewStore(dirB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storeB.Close() }()

	nodeA, err := NewAgentMeshNode(Options{
		PrivKey:           privA,
		RouterAddrs:       nil,
		Store:             storeA,
		DiscoveryInterval: "1s",
		ListenAddrs:       []string{"/ip4/127.0.0.1/tcp/0"},
		EnableRelay:       false,
		NodeConfig:        &NodeConfigComplete{},
		KeyGracePeriod:    24 * time.Hour,
		AllowLoopback:     true,
		MonitorBootstrap:  2 * time.Minute,
		MonitorInterval:   1 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeA.BiscuitTimeout = 500 * time.Millisecond
	if err := nodeA.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := nodeA.Host.Close(); err != nil {
			t.Logf("failed to close nodeA host: %v", err)
		}
	}()

	nodeB, err := NewAgentMeshNode(Options{
		PrivKey:           privB,
		RouterAddrs:       nil,
		Store:             storeB,
		DiscoveryInterval: "1s",
		ListenAddrs:       []string{"/ip4/127.0.0.1/tcp/0"},
		EnableRelay:       false,
		NodeConfig:        &NodeConfigComplete{},
		KeyGracePeriod:    24 * time.Hour,
		AllowLoopback:     true,
		MonitorBootstrap:  2 * time.Minute,
		MonitorInterval:   1 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeB.BiscuitTimeout = 500 * time.Millisecond
	if err := nodeB.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := nodeB.Host.Close(); err != nil {
			t.Logf("failed to close nodeB host: %v", err)
		}
	}()

	enrollUnderRoot(t, nodeA, nodeB)

	err = nodeB.Host.Connect(ctx, peer.AddrInfo{ID: nodeA.Host.ID(), Addrs: nodeA.Host.Addrs()})
	if err != nil {
		t.Fatal(err)
	}

	// Capture observed requests on a channel. Network I/O is not a
	// happens-before edge, so a plain variable written in the handler
	// and read in the test is a data race under -race.
	type capturedRequest struct {
		headers http.Header
		path    string
	}
	received := make(chan capturedRequest, 8)

	dummyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// MCP aggregation probes POST initialize; the datapath cases are GET.
		if r.Method == http.MethodGet {
			select {
			case received <- capturedRequest{headers: r.Header.Clone(), path: r.URL.Path}:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer dummyServer.Close()

	serviceName := "dummy-tool"
	serviceInfo := &api.ServiceInfo{
		Type: api.ServiceType_SERVICE_TYPE_MCP,
		Name: serviceName,
	}

	_ = nodeA.RegisterService(ctx, &api.RegisterServiceRequest{
		Service: serviceInfo,
		Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: dummyServer.URL},
	})

	handler, err := newReverseProxyHandler(dummyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	nodeA.services.insertService(&testService{
		info:    serviceInfo,
		handler: handler,
	})

	nodeB.BoundHTTPAddr = "127.0.0.1:0"

	proxyHandler := createEgressProxy(nodeB)
	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	client := &http.Client{}

	tests := []struct {
		name                   string
		pathSuffix             string // e.g. "/api/v1/test" or ""
		requestHeaders         map[string]string
		wantReceivedHeaders    map[string]string // header key -> expected value. If value is "", header must be absent.
		wantReceivedPathSuffix string
	}{
		{
			name:       "Authorization passes through untouched to the destination",
			pathSuffix: "/api/v1/test",
			requestHeaders: map[string]string{
				"Authorization":              "Bearer upstream-token",
				api.HeaderMeshAuthentication: "Bearer local-token",
				"X-Test-Request":             "yes",
			},
			wantReceivedHeaders: map[string]string{
				"Authorization":               "Bearer upstream-token",
				api.HeaderMeshAuthentication:  "",                       // Must be stripped, local-only
				api.HeaderMeshBiscuit:         "",                       // Must be stripped
				api.HeaderMeshNoTrailingSlash: "",                       // Must be stripped
				api.HeaderPeerID:              nodeB.Host.ID().String(), // Verified caller stamped by ingress
				"X-Test-Request":              "yes",
			},
			wantReceivedPathSuffix: "/api/v1/test",
		},
		{
			name:       "Inbound X-Peer-Id is overwritten with the verified caller",
			pathSuffix: "/api/v1/test",
			requestHeaders: map[string]string{
				api.HeaderMeshAuthentication: "Bearer local-token",
				api.HeaderPeerID:             "spoofed-peer-id",
			},
			wantReceivedHeaders: map[string]string{
				api.HeaderPeerID: nodeB.Host.ID().String(),
			},
			wantReceivedPathSuffix: "/api/v1/test",
		},
		{
			name:       "Local gate header stripped, no Authorization to pass through",
			pathSuffix: "/api/v1/test",
			requestHeaders: map[string]string{
				api.HeaderMeshAuthentication: "Bearer local-token",
				"X-Test-Request":             "yes",
			},
			wantReceivedHeaders: map[string]string{
				"Authorization":               "", // Nothing to pass through
				api.HeaderMeshAuthentication:  "", // Must be stripped to avoid leaking local sidecar token
				api.HeaderMeshBiscuit:         "",
				api.HeaderMeshNoTrailingSlash: "",
				"X-Test-Request":              "yes",
			},
			wantReceivedPathSuffix: "/api/v1/test",
		},
		{
			name:       "Trailing slash handling with empty path",
			pathSuffix: "", // Requesting the root of the service: .../dummy-tool
			requestHeaders: map[string]string{
				api.HeaderMeshAuthentication: "Bearer local-token",
			},
			wantReceivedHeaders: map[string]string{
				api.HeaderMeshNoTrailingSlash: "", // Must be stripped
			},
			wantReceivedPathSuffix: "/", // Should be mapped to "/" without trailing slash issues
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := fmt.Sprintf("%s/mesh/%s/mcp/%s%s", proxyServer.URL, nodeA.Host.ID().String(), serviceName, tt.pathSuffix)
			req, err := http.NewRequest("GET", url, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.requestHeaders {
				req.Header.Set(k, v)
			}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("Expected 200 OK, got %d", resp.StatusCode)
			}

			var got capturedRequest
			select {
			case got = <-received:
			case <-ctx.Done():
				t.Fatal("timed out waiting for backend to observe the proxied request")
			}

			if got.path != tt.wantReceivedPathSuffix {
				t.Errorf("Expected received path to be %q, got %q", tt.wantReceivedPathSuffix, got.path)
			}

			for k, expectedVal := range tt.wantReceivedHeaders {
				actualVal := got.headers.Get(k)
				if expectedVal == "" {
					if _, present := got.headers[k]; present {
						t.Errorf("Expected header %q to be absent, but got %q", k, actualVal)
					}
				} else {
					if actualVal != expectedVal {
						t.Errorf("Expected header %q to be %q, got %q", k, expectedVal, actualVal)
					}
				}
			}
		})
	}
}

// The operator's egress floor holds at the /mesh/ chokepoint itself, so an agent
// that skips the facade and dials /mesh/<peer>/... raw is still gated: the
// provider's biscuit must attest every pair of the floor before anything is
// forwarded, whatever the caller did or did not ask for (see #385).
func TestEgressProxyEnforcesFloor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	nodeA, cleanupA := startBareNode(t, ctx) // provider
	defer cleanupA()
	nodeB, cleanupB := startBareNode(t, ctx) // egress side, holds the floor
	defer cleanupB()

	rootPub, rootPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen root key: %v", err)
	}

	// nodeB authenticates to nodeA as an unrestricted caller.
	if err := buildAndSaveBiscuit(nodeB, rootPriv); err != nil {
		t.Fatalf("buildAndSaveBiscuit: %v", err)
	}
	nodeA.keysMu.Lock()
	nodeA.trustedKeys = append(nodeA.trustedKeys, TrustedKey{Key: rootPub, ReceivedAt: time.Now()})
	nodeA.keysMu.Unlock()

	// nodeA attests jurisdiction=eu and nothing else.
	nodeAIdentity, err := identity.MintBootstrapBiscuitToken(rootPriv, nodeA.Host.ID(), api.RoleNode, time.Now().Add(time.Hour), publishEverything, map[string]string{"jurisdiction": "eu"})
	if err != nil {
		t.Fatalf("mint nodeA identity: %v", err)
	}
	nodeA.SetIdentityCache(nodeAIdentity)
	nodeB.keysMu.Lock()
	nodeB.trustedKeys = append(nodeB.trustedKeys, TrustedKey{Key: rootPub, ReceivedAt: time.Now()})
	nodeB.keysMu.Unlock()

	if err := nodeB.Host.Connect(ctx, peer.AddrInfo{ID: nodeA.Host.ID(), Addrs: nodeA.Host.Addrs()}); err != nil {
		t.Fatal(err)
	}

	const expectedBody = `{"status":"floored"}`
	dummyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(expectedBody))
	}))
	defer dummyServer.Close()

	serviceInfo := &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "dummy-tool"}
	targetURL, _ := url.Parse(dummyServer.URL)
	nodeA.services.insertService(&testService{
		info:    serviceInfo,
		handler: httputil.NewSingleHostReverseProxy(targetURL),
	})

	proxyServer := httptest.NewServer(createEgressProxy(nodeB))
	defer proxyServer.Close()
	// The caller stays silent: no X-Mesh-Required-Labels anywhere.
	rawURL := fmt.Sprintf("%s/mesh/%s/mcp/dummy-tool/api/v1/test", proxyServer.URL, nodeA.Host.ID())

	get := func() *http.Response {
		t.Helper()
		resp, err := http.Get(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// nodeA attests the whole floor: egress proceeds.
	nodeB.nodeConfig = &NodeConfigComplete{EgressRequireLabels: map[string]string{"jurisdiction": "eu"}}
	var resp *http.Response
	for i := 0; i < 3; i++ {
		resp = get()
		if resp.StatusCode == http.StatusOK {
			break
		}
		_ = resp.Body.Close()
		time.Sleep(time.Second)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != expectedBody {
		t.Fatalf("egress inside the floor must pass: status %d, body %q", resp.StatusCode, body)
	}

	// A non-canonical spelling of the same peer (CID form) is canonicalized at
	// the gate boundary, so the verdict and the dial name the same peer.
	cidURL := fmt.Sprintf("%s/mesh/%s/mcp/dummy-tool/api/v1/test", proxyServer.URL, peer.ToCid(nodeA.Host.ID()))
	resp, err = http.Get(cidURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != expectedBody {
		t.Fatalf("a CID-spelled peer inside the floor must pass: status %d, body %q", resp.StatusCode, body)
	}

	// One pair short of the floor: refused at the chokepoint, nothing forwarded.
	nodeB.nodeConfig = &NodeConfigComplete{EgressRequireLabels: map[string]string{"jurisdiction": "eu", "compliance": "gdpr"}}
	resp = get()
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("egress outside the floor must be 403 at the chokepoint, got %d", resp.StatusCode)
	}
}
