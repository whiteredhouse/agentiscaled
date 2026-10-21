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
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// socketClient talks HTTP over a Unix socket; the host in the URL is ignored.
func socketClient(path string) *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		},
	}
}

func waitForSocket(t *testing.T, path string) *http.Client {
	t.Helper()
	client := socketClient(path)
	for i := 0; i < 50; i++ {
		resp, err := client.Get("http://localhost/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return client
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sidecar socket %s never answered", path)
	return nil
}

// TestSidecarSocketAuthorizesWithoutToken pins the socket's trust model: the
// file's owner-only permissions are the credential, while the TCP listener on
// the very same server keeps demanding the token.
func TestSidecarSocketAuthorizesWithoutToken(t *testing.T) {
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

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("failed to stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("socket permissions are %#o, want 0600", perm)
	}
	if node.BoundSocketPath != socketPath {
		t.Errorf("BoundSocketPath = %q, want %q", node.BoundSocketPath, socketPath)
	}

	// 503 means the request cleared the auth gate and was only stopped by the
	// node not being connected to a mesh.
	resp, err := client.Get("http://localhost/mesh/service/discover?type=mcp&name=test")
	if err != nil {
		t.Fatalf("socket request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("untokened socket request: got status %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	tcpClient := &http.Client{Timeout: 2 * time.Second}
	tcpResp, err := tcpClient.Get("http://" + node.BoundHTTPAddr + "/mesh/service/discover?type=mcp&name=test")
	if err != nil {
		t.Fatalf("tcp request failed: %v", err)
	}
	defer func() { _ = tcpResp.Body.Close() }()
	if tcpResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("untokened TCP request: got status %d, want %d", tcpResp.StatusCode, http.StatusUnauthorized)
	}
}

// TestSidecarSocketOnly covers a node with no TCP listener at all, where no API
// token exists to demand.
func TestSidecarSocketOnly(t *testing.T) {
	node := &AgentMeshNode{
		BiscuitTimeout: 500 * time.Millisecond,
		services:       NewServiceRegistry(&fakeDHT{}, 0),
	}
	socketPath := filepath.Join(t.TempDir(), "agentmesh.sock")

	srv, err := StartSidecarServer(node, "", socketPath, "", "", "", "")
	if err != nil {
		t.Fatalf("failed to start socket-only sidecar server: %v", err)
	}
	defer func() { _ = srv.Close() }()

	waitForSocket(t, socketPath)

	if node.BoundHTTPAddr != "" {
		t.Errorf("BoundHTTPAddr = %q, want empty with no TCP listener", node.BoundHTTPAddr)
	}
}

func TestStartSidecarServerRequiresAListener(t *testing.T) {
	node := &AgentMeshNode{services: NewServiceRegistry(&fakeDHT{}, 0)}
	if _, err := StartSidecarServer(node, "", "", "token", "", "", ""); err == nil {
		t.Fatal("expected an error when neither a TCP address nor a socket is configured")
	}
}

// TestSidecarSocketFailureKeepsTCPServing pins that the socket stays optional:
// an unusable path degrades to a warning as long as TCP is still serving, but
// is fatal for a node that has no other way in.
func TestSidecarSocketFailureKeepsTCPServing(t *testing.T) {
	unusable := filepath.Join(t.TempDir(), "missing-parent")
	if err := os.WriteFile(unusable, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("failed to write the blocking file: %v", err)
	}
	socketPath := filepath.Join(unusable, "agentmesh.sock")

	node := &AgentMeshNode{
		BiscuitTimeout: 500 * time.Millisecond,
		services:       NewServiceRegistry(&fakeDHT{}, 0),
	}
	srv, err := StartSidecarServer(node, "127.0.0.1:0", socketPath, "test-token", "", "", "")
	if err != nil {
		t.Fatalf("a bad socket path must not stop a TCP-serving node: %v", err)
	}
	defer func() { _ = srv.Close() }()
	if node.BoundHTTPAddr == "" {
		t.Error("expected the TCP listener to be bound")
	}
	if node.BoundSocketPath != "" {
		t.Errorf("BoundSocketPath = %q, want empty after a failed socket", node.BoundSocketPath)
	}

	socketOnly := &AgentMeshNode{services: NewServiceRegistry(&fakeDHT{}, 0)}
	if _, err := StartSidecarServer(socketOnly, "", socketPath, "", "", "", ""); err == nil {
		t.Error("expected an error when the socket is the only configured listener")
	}
}

func TestListenLocalSocket(t *testing.T) {
	t.Run("replaces a socket left behind by a crashed node", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agentmesh.sock")
		stale, err := net.Listen("unix", path)
		if err != nil {
			t.Fatalf("failed to create the stale socket: %v", err)
		}
		// Closing without unlinking is what a crash leaves behind.
		if unixListener, ok := stale.(*net.UnixListener); ok {
			unixListener.SetUnlinkOnClose(false)
		}
		_ = stale.Close()

		listener, err := listenLocalSocket(path)
		if err != nil {
			t.Fatalf("listenLocalSocket on a stale socket: %v", err)
		}
		_ = listener.Close()
	})

	t.Run("refuses a socket a live node is using", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agentmesh.sock")
		live, err := net.Listen("unix", path)
		if err != nil {
			t.Fatalf("failed to create the live socket: %v", err)
		}
		defer func() { _ = live.Close() }()
		go func() {
			for {
				conn, err := live.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()

		if _, err := listenLocalSocket(path); err == nil {
			t.Fatal("expected an error when another node is listening on the socket")
		}
	})

	t.Run("refuses to remove a file that is not a socket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agentmesh.sock")
		if err := os.WriteFile(path, []byte("precious"), 0600); err != nil {
			t.Fatalf("failed to write the file: %v", err)
		}

		if _, err := listenLocalSocket(path); err == nil {
			t.Fatal("expected an error for an existing non-socket file")
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the existing file must be left alone: %v", err)
		}
	})
}

func TestWithAuth(t *testing.T) {
	token := "test-token"
	handler := withAuth(token, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name           string
		authHeader     string
		expectedStatus int
	}{
		{
			name:           "Valid token",
			authHeader:     "Bearer test-token",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "Missing token",
			authHeader:     "",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "Invalid format",
			authHeader:     "test-token",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "Wrong token",
			authHeader:     "Bearer wrong-token",
			expectedStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/any", nil)
			if tt.authHeader != "" {
				req.Header.Set(api.HeaderMeshAuthentication, tt.authHeader)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rr.Code)
			}
		})
	}

	t.Run("Authorization fallback accepted when allowed", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/any", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}
	})

	t.Run("Consumed gate header is stripped, other headers survive", func(t *testing.T) {
		var gotMeshAuth, gotAuth string
		inspect := withAuth(token, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMeshAuth = r.Header.Get(api.HeaderMeshAuthentication)
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest("GET", "/any", nil)
		req.Header.Set(api.HeaderMeshAuthentication, "Bearer "+token)
		req.Header.Set("Authorization", "Bearer backend-credential")
		rr := httptest.NewRecorder()
		inspect.ServeHTTP(rr, req)

		if gotMeshAuth != "" {
			t.Errorf("consumed %s header must be stripped, got %q", api.HeaderMeshAuthentication, gotMeshAuth)
		}
		if gotAuth != "Bearer backend-credential" {
			t.Errorf("Authorization must survive when not consumed, got %q", gotAuth)
		}
	})

	t.Run("Consumed Authorization fallback is stripped", func(t *testing.T) {
		var gotAuth string
		inspect := withAuth(token, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest("GET", "/any", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		inspect.ServeHTTP(rr, req)

		if gotAuth != "" {
			t.Errorf("consumed Authorization header must be stripped, got %q", gotAuth)
		}
	})

	t.Run("Authorization fallback rejected in strict mode", func(t *testing.T) {
		strictHandler := withAuth(token, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest("GET", "/any", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		strictHandler.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
		}
	})

	t.Run("Empty token configured", func(t *testing.T) {
		handlerEmpty := withAuth("", true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest("GET", "/any", nil)
		req.Header.Set(api.HeaderMeshAuthentication, "Bearer anything")
		rr := httptest.NewRecorder()
		handlerEmpty.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}
	})
}

func TestSidecarServerAuthEnforcement(t *testing.T) {
	node := &AgentMeshNode{
		BiscuitTimeout: 500 * time.Millisecond,
		services:       NewServiceRegistry(&fakeDHT{}, 0),
	}
	// We use a dummy token
	token := "test-token"

	// Start sidecar on an ephemeral port
	sidecarSrv, err := StartSidecarServer(node, "127.0.0.1:0", "", token, "", "", "")
	if err != nil {
		t.Fatalf("Failed to start sidecar server: %v", err)
	}
	defer func() { _ = sidecarSrv.Close() }()

	baseURL := "http://" + node.BoundHTTPAddr
	client := &http.Client{Timeout: 2 * time.Second}
	ready := false
	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		resp, err := client.Get(baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
	}
	if !ready {
		t.Fatalf("Sidecar server failed to become ready")
	}

	tests := []struct {
		name           string
		method         string
		path           string
		expectedStatus int
		needsToken     bool
	}{
		{"healthz is public", "GET", "/healthz", http.StatusOK, false},
		// Public, and honest: this node holds no router, so it is not ready.
		{"readyz is public", "GET", "/readyz", http.StatusServiceUnavailable, false},
		// Liveness says nothing; metrics name peers and count their traffic.
		{"metrics is protected", "GET", "/metrics", http.StatusUnauthorized, false},
		// Services are declared in configuration only: the former runtime
		// mutation endpoints are gone, so these paths are plain egress-proxy
		// paths (503: not connected) with no special handling.
		{"register does not exist", "POST", "/mesh/service/register", http.StatusServiceUnavailable, true},
		{"unregister does not exist", "POST", "/mesh/service/unregister", http.StatusServiceUnavailable, true},
		{"discover is protected", "GET", "/mesh/service/discover?type=mcp&name=test", http.StatusUnauthorized, false},
		{"egress proxy is protected", "GET", "/mesh/", http.StatusUnauthorized, false},
		{"mcp root is protected", "GET", "/mcp", http.StatusUnauthorized, false},

		{"metrics with token", "GET", "/metrics", http.StatusOK, true},
		// /mesh/service/discover without mesh connection will return 503 instead of 400 since node is not connected,
		// but as long as it gets past auth, that's what we want to verify.
		{"discover with token", "GET", "/mesh/service/discover?type=mcp&name=test", http.StatusServiceUnavailable, true},
		{"egress proxy with token", "GET", "/mesh/", http.StatusServiceUnavailable, true},
		{"mcp root with token", "GET", "/mcp", http.StatusServiceUnavailable, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, baseURL+tt.path, nil)
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}
			if tt.needsToken {
				req.Header.Set(api.HeaderMeshAuthentication, "Bearer "+token)
			}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer func() {
				_ = resp.Body.Close()
			}()

			if resp.StatusCode != tt.expectedStatus {
				t.Errorf("expected status %d for %s, got %d", tt.expectedStatus, tt.path, resp.StatusCode)
			}
		})
	}
}

// TestSidecarAuthorizationFallbackScope verifies that plain "Authorization" is
// only accepted as a local-gate credential on endpoints that never forward it
// (register/unregister/discover, mcp), and rejected on the egress proxy, which
// must forward "Authorization" untouched to the destination service.
func TestSidecarAuthorizationFallbackScope(t *testing.T) {
	node := &AgentMeshNode{
		BiscuitTimeout: 500 * time.Millisecond,
		services:       NewServiceRegistry(&fakeDHT{}, 0),
	}
	token := "test-token"

	sidecarSrv, err := StartSidecarServer(node, "127.0.0.1:0", "", token, "", "", "")
	if err != nil {
		t.Fatalf("Failed to start sidecar server: %v", err)
	}
	defer func() { _ = sidecarSrv.Close() }()

	baseURL := "http://" + node.BoundHTTPAddr
	client := &http.Client{Timeout: 2 * time.Second}
	ready := false
	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		resp, err := client.Get(baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
	}
	if !ready {
		t.Fatalf("Sidecar server failed to become ready")
	}

	tests := []struct {
		name           string
		method         string
		path           string
		expectedStatus int // status once past the auth gate (may still be a downstream error)
	}{
		{"discover accepts Authorization fallback", "GET", "/mesh/service/discover?type=mcp&name=test", http.StatusServiceUnavailable},
		{"mcp root accepts Authorization fallback", "GET", "/mcp", http.StatusServiceUnavailable},
		{"egress proxy rejects Authorization fallback", "GET", "/mesh/", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, baseURL+tt.path, nil)
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.expectedStatus {
				t.Errorf("expected status %d for %s, got %d", tt.expectedStatus, tt.path, resp.StatusCode)
			}
		})
	}
}

func TestRegisterService(t *testing.T) {
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	// The router the node announces to.
	router := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")
	serveDirectory(router.h)
	if err := h.Connect(context.Background(), peer.AddrInfo{ID: router.h.ID(), Addrs: router.h.Addrs()}); err != nil {
		t.Fatal(err)
	}
	d := &routerDirectory{host: h, routers: func() []peer.ID { return []peer.ID{router.h.ID()} }, addrs: h.Addrs}

	node := &AgentMeshNode{BiscuitTimeout: 500 * time.Millisecond,
		services:  NewServiceRegistry(d, 0),
		Directory: d,
	}

	// MCPService.Init() opens a live session to the URL; serve a fake MCP backend.
	upstream := httptest.NewServer(newFakeMCPHandler(t, []*mcp.Tool{}))
	defer upstream.Close()

	req := &api.RegisterServiceRequest{
		Service: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "test-service", Description: "test desc"},
		Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: upstream.URL},
	}
	if err := node.RegisterService(context.Background(), req); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}

	if !node.IsServiceRegistered("test-service") {
		t.Errorf("expected service to be registered")
	}

	// Tear down so the live MCP session releases the upstream SSE stream;
	// otherwise upstream.Close() blocks waiting for in-flight handlers.
	if err := node.UnregisterService(context.Background(), "test-service"); err != nil {
		t.Errorf("unregister: %v", err)
	}
}

func TestUnregisterService(t *testing.T) {
	node := &AgentMeshNode{BiscuitTimeout: 500 * time.Millisecond,
		services: NewServiceRegistry(&fakeDHT{}, 0),
	}
	node.services.insertService(&testService{info: &api.ServiceInfo{Name: "test-service"}})

	if err := node.UnregisterService(context.Background(), "test-service"); err != nil {
		t.Fatalf("UnregisterService: %v", err)
	}

	if node.IsServiceRegistered("test-service") {
		t.Errorf("expected service to be unregistered")
	}
}

func TestHandleDiscoverService(t *testing.T) {
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	router := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")
	dir := serveDirectory(router.h)
	if err := h.Connect(context.Background(), peer.AddrInfo{ID: router.h.ID(), Addrs: router.h.Addrs()}); err != nil {
		t.Fatal(err)
	}
	d := &routerDirectory{host: h, routers: func() []peer.ID { return []peer.ID{router.h.ID()} }, addrs: h.Addrs}

	node := &AgentMeshNode{BiscuitTimeout: 500 * time.Millisecond,
		services:      NewServiceRegistry(d, 0),
		Directory:     d,
		Host:          h,
		BoundHTTPAddr: "127.0.0.1:8080",
	}

	// A service another member announced to the directory.
	h2, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h2.Close() }()
	serviceInfo := &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "remote-service"}
	key, err := api.ServiceKey(serviceInfo.Type, serviceInfo.Name)
	if err != nil {
		t.Fatal(err)
	}
	dir.add(key, h2.ID(), h2.Addrs()...)

	req := httptest.NewRequest("GET", "/mesh/service/discover?type=mcp&name=remote-service", nil)
	rr := httptest.NewRecorder()

	handleDiscoverService(node, rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %d, body: %s", rr.Code, rr.Body.String())
	}

	var providers []*api.DiscoveredProvider
	if err := json.NewDecoder(rr.Body).Decode(&providers); err != nil {
		t.Fatal(err)
	}

	if len(providers) != 1 {
		t.Errorf("expected 1 provider, got %d", len(providers))
	} else if providers[0].PeerId != h2.ID().String() {
		t.Errorf("expected provider %s, got %s", h2.ID().String(), providers[0].PeerId)
	}
}

func TestListLocalServices(t *testing.T) {
	node := &AgentMeshNode{BiscuitTimeout: 500 * time.Millisecond,
		services: NewServiceRegistry(&fakeDHT{}, 0),
	}

	service1 := &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "service1"}
	service2 := &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_INFERENCE, Name: "service2"}

	node.services.insertService(&testService{info: service1})
	node.services.insertService(&testService{info: service2})

	services := node.ListLocalServices(api.ServiceType_SERVICE_TYPE_UNSPECIFIED)

	if len(services) != 2 {
		t.Errorf("expected 2 services, got %d", len(services))
	}
}

func TestListLocalServices_TypeFilter(t *testing.T) {
	node := &AgentMeshNode{BiscuitTimeout: 500 * time.Millisecond,
		services: NewServiceRegistry(&fakeDHT{}, 0),
	}
	mcpA := &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "mcp-a"}
	mcpB := &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "mcp-b"}
	inf := &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_INFERENCE, Name: "inf-a"}
	node.services.insertService(&testService{info: mcpA})
	node.services.insertService(&testService{info: mcpB})
	node.services.insertService(&testService{info: inf})

	cases := []struct {
		name      string
		filter    api.ServiceType
		wantCount int
	}{
		{"unspecified returns all", api.ServiceType_SERVICE_TYPE_UNSPECIFIED, 3},
		{"mcp filter", api.ServiceType_SERVICE_TYPE_MCP, 2},
		{"inference filter", api.ServiceType_SERVICE_TYPE_INFERENCE, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := node.ListLocalServices(tc.filter)
			if len(got) != tc.wantCount {
				t.Errorf("expected %d services, got %d", tc.wantCount, len(got))
			}
			for _, s := range got {
				if tc.filter != api.ServiceType_SERVICE_TYPE_UNSPECIFIED && s.Type != tc.filter {
					t.Errorf("filter %v leaked service of type %v: %s", tc.filter, s.Type, s.Name)
				}
			}
		})
	}
}

func TestRegisterService_Validation(t *testing.T) {
	node := &AgentMeshNode{BiscuitTimeout: 500 * time.Millisecond,
		services: NewServiceRegistry(&fakeDHT{}, 0),
	}

	tests := []struct {
		name    string
		reqBody *api.RegisterServiceRequest
	}{
		{
			name: "Missing service",
			reqBody: &api.RegisterServiceRequest{
				Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: "http://localhost:8080"},
			},
		},
		{
			name: "Missing name",
			reqBody: &api.RegisterServiceRequest{
				Service: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP},
				Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: "http://localhost:8080"},
			},
		},
		{
			name: "Unspecified type",
			reqBody: &api.RegisterServiceRequest{
				Service: &api.ServiceInfo{Name: "test-service", Type: api.ServiceType_SERVICE_TYPE_UNSPECIFIED},
				Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: "http://localhost:8080"},
			},
		},
		{
			name: "Missing backend",
			reqBody: &api.RegisterServiceRequest{
				Service: &api.ServiceInfo{Name: "test-service", Type: api.ServiceType_SERVICE_TYPE_MCP},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := node.RegisterService(context.Background(), tt.reqBody); err == nil {
				t.Errorf("expected RegisterService to reject invalid request")
			}
		})
	}
}

func TestStartSidecarServer_TokenMandatory(t *testing.T) {
	node := &AgentMeshNode{BiscuitTimeout: 500 * time.Millisecond}

	// Test case: No token, no TLS
	_, err := StartSidecarServer(node, "127.0.0.1:0", "", "", "", "", "")
	if err == nil {
		t.Fatal("Expected StartSidecarServer to fail without token and TLS, but it succeeded")
	}
	if !strings.Contains(err.Error(), "token is mandatory when not using mTLS") {
		t.Fatalf("Expected error to contain 'token is mandatory when not using mTLS', got: %v", err)
	}

	// Test case: Token provided, should not fail immediately
	sidecarSrv, err := StartSidecarServer(node, "127.0.0.1:0", "", "some-token", "", "", "")
	if err == nil {
		defer func() { _ = sidecarSrv.Close() }()
	} else {
		if strings.Contains(err.Error(), "token is mandatory when not using mTLS") {
			t.Fatalf("Did not expect 'token is mandatory' error when token is provided, got: %v", err)
		}
	}
}

func TestDiscoverService_Pagination(t *testing.T) {
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	router := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")
	dir := serveDirectory(router.h)
	if err := h.Connect(context.Background(), peer.AddrInfo{ID: router.h.ID(), Addrs: router.h.Addrs()}); err != nil {
		t.Fatal(err)
	}
	d := &routerDirectory{host: h, routers: func() []peer.ID { return []peer.ID{router.h.ID()} }, addrs: h.Addrs}

	node := &AgentMeshNode{
		BiscuitTimeout: 500 * time.Millisecond,
		services:       NewServiceRegistry(d, 0),
		Directory:      d,
		Host:           h,
		BoundHTTPAddr:  "127.0.0.1:8080",
	}

	serviceInfo := &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "paginated-service"}
	key, err := api.ServiceKey(serviceInfo.Type, serviceInfo.Name)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		h2, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = h2.Close() }()
		dir.add(key, h2.ID(), h2.Addrs()...)
	}

	// 1. Query page 1 (limit=2, offset=0)
	req := httptest.NewRequest("GET", "/mesh/service/discover?type=mcp&name=paginated-service&limit=2&offset=0", nil)
	rr := httptest.NewRecorder()
	handleDiscoverService(node, rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected status OK, got %d, body: %s", rr.Code, rr.Body.String())
	}
	var page1 []*api.DiscoveredProvider
	if err := json.NewDecoder(rr.Body).Decode(&page1); err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 {
		t.Errorf("expected 2 providers on page 1, got %d", len(page1))
	}

	// 2. Query page 2 (limit=2, offset=2)
	req2 := httptest.NewRequest("GET", "/mesh/service/discover?type=mcp&name=paginated-service&limit=2&offset=2", nil)
	rr2 := httptest.NewRecorder()
	handleDiscoverService(node, rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Errorf("expected status OK, got %d, body: %s", rr2.Code, rr2.Body.String())
	}
	var page2 []*api.DiscoveredProvider
	if err := json.NewDecoder(rr2.Body).Decode(&page2); err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 {
		t.Errorf("expected 1 provider on page 2, got %d", len(page2))
	}

	// 3. Query page 3 (limit=2, offset=4)
	req3 := httptest.NewRequest("GET", "/mesh/service/discover?type=mcp&name=paginated-service&limit=2&offset=4", nil)
	rr3 := httptest.NewRecorder()
	handleDiscoverService(node, rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Errorf("expected status OK, got %d, body: %s", rr3.Code, rr3.Body.String())
	}
	var page3 []*api.DiscoveredProvider
	if err := json.NewDecoder(rr3.Body).Decode(&page3); err != nil {
		t.Fatal(err)
	}
	if len(page3) != 0 {
		t.Errorf("expected 0 providers on page 3, got %d", len(page3))
	}
}
