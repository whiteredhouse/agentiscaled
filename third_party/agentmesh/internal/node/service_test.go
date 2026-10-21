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
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
)

// testService is a minimal Service implementation for test setup. Tests use
// this with ServiceRegistry.insertDirect to inject a pre-built handler
// without going through Init or DHT advertisement.
type testService struct {
	info    *api.ServiceInfo
	handler http.Handler
}

func (t *testService) Info() *api.ServiceInfo       { return t.info }
func (t *testService) Init(_ context.Context) error { return nil }
func (t *testService) Handler() http.Handler        { return t.handler }
func (t *testService) Teardown() error              { return nil }

func TestBaseService_InitURLBackend_BuildsReverseProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	b := &baseService{
		info:    &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_INFERENCE, Name: "demo"},
		backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: upstream.URL},
	}
	if err := b.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, ok := b.handler.(*httputil.ReverseProxy); !ok {
		t.Fatalf("handler type: got %T, want *httputil.ReverseProxy", b.handler)
	}
	if b.cmd != nil {
		t.Errorf("cmd should be nil for URL backend, got %v", b.cmd)
	}
	if err := b.Teardown(); err != nil {
		t.Errorf("Teardown: %v", err)
	}
}

func TestNewReverseProxyHandler_RewritesRequests(t *testing.T) {
	for _, tc := range []struct {
		name            string
		targetURL       string
		requestURL      string
		noTrailingSlash bool
		wantURL         string
		wantProto       string
	}{
		{
			name:       "joins target path and query",
			targetURL:  "http://backend.example/base?upstream=one",
			requestURL: "https://service.mesh/child/?client=two",
			wantURL:    "http://backend.example/base/child/?upstream=one&client=two",
			wantProto:  "https",
		},
		{
			name:            "marker trims trailing slash",
			targetURL:       "http://backend.example/base?upstream=one",
			requestURL:      "http://service.mesh/child/?client=two",
			noTrailingSlash: true,
			wantURL:         "http://backend.example/base/child?upstream=one&client=two",
			wantProto:       "http",
		},
		{
			name:            "target trailing slash is retained",
			targetURL:       "http://backend.example/base/",
			requestURL:      "http://service.mesh/child/",
			noTrailingSlash: true,
			wantURL:         "http://backend.example/base/child/",
			wantProto:       "http",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := newReverseProxyHandler(tc.targetURL)
			if err != nil {
				t.Fatalf("newReverseProxyHandler: %v", err)
			}
			proxy := handler.(*httputil.ReverseProxy)
			var forwarded *http.Request
			proxy.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				forwarded = req.Clone(req.Context())
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Header:     make(http.Header),
					Body:       http.NoBody,
				}, nil
			})
			req := httptest.NewRequest(http.MethodGet, tc.requestURL, nil)
			req.RemoteAddr = "192.0.2.1:12345"
			if tc.noTrailingSlash {
				req.Header.Set(api.HeaderMeshNoTrailingSlash, "true")
			}
			req.Header.Set("Forwarded", "for=spoofed;host=spoofed;proto=spoofed")
			req.Header.Set("X-Forwarded-For", "spoofed")
			req.Header.Set("X-Forwarded-Host", "spoofed")
			req.Header.Set("X-Forwarded-Proto", "spoofed")
			recorder := httptest.NewRecorder()
			proxy.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
			}
			if forwarded == nil {
				t.Fatal("request did not reach the upstream transport")
			}
			if got := forwarded.URL.String(); got != tc.wantURL {
				t.Errorf("upstream URL = %q, want %q", got, tc.wantURL)
			}
			// The peer chose req.Host; the backend is addressed by its
			// configured URL, and the original stays in X-Forwarded-Host.
			if forwarded.Host != "backend.example" {
				t.Errorf("upstream Host = %q, want %q", forwarded.Host, "backend.example")
			}
			for name, want := range map[string]string{
				api.HeaderMeshNoTrailingSlash: "",
				"Forwarded":                   "",
				"X-Forwarded-For":             "192.0.2.1",
				"X-Forwarded-Host":            req.Host,
				"X-Forwarded-Proto":           tc.wantProto,
			} {
				if got := forwarded.Header.Get(name); got != want {
					t.Errorf("upstream %s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

func TestBaseService_InitURLBackend_InvalidURL(t *testing.T) {
	b := &baseService{
		info:    &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_INFERENCE, Name: "demo"},
		backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: "::not a url::"},
	}
	if err := b.Init(context.Background()); err == nil {
		t.Fatal("Init: expected error for invalid URL, got nil")
	}
}

func TestBaseService_InitCommandBackend_BuildsBridge(t *testing.T) {
	b := &baseService{
		info: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "demo"},
		backend: &api.RegisterServiceRequest_Command{
			Command: &api.CommandBackend{Command: []string{"/bin/cat"}},
		},
	}
	if err := b.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = b.Teardown() }()
	if _, ok := b.handler.(*StdioBridge); !ok {
		t.Fatalf("handler type: got %T, want *StdioBridge", b.handler)
	}
	if b.cmd == nil {
		t.Error("cmd should be non-nil for Command backend")
	}
}

func TestBaseService_TeardownIsNilSafe(t *testing.T) {
	b := &baseService{
		info: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_INFERENCE, Name: "demo"},
	}
	if err := b.Teardown(); err != nil {
		t.Fatalf("Teardown on uninitialised base: %v", err)
	}
}

// A killed backend must also be reaped: kill(pid, 0) keeps succeeding on a
// zombie, and Process.Signal only reports ErrProcessDone once Wait has run.
func TestBaseService_TeardownReapsCommandBackend(t *testing.T) {
	b := &baseService{
		info: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "demo"},
		backend: &api.RegisterServiceRequest_Command{
			Command: &api.CommandBackend{Command: []string{"/bin/cat"}},
		},
	}
	if err := b.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := b.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		err := b.cmd.Process.Signal(syscall.Signal(0))
		if errors.Is(err, os.ErrProcessDone) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend pid %d still not reaped after Teardown (Signal(0) = %v)", b.cmd.Process.Pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := b.Teardown(); err != nil {
		t.Errorf("Teardown on an already-reaped backend: %v", err)
	}
}

func TestNewServiceFromRequest_ReturnsMCPService(t *testing.T) {
	req := &api.RegisterServiceRequest{
		Service: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_MCP, Name: "x"},
		Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: "http://example.com"},
	}
	svc, err := NewServiceFromRequest(req)
	if err != nil {
		t.Fatalf("NewServiceFromRequest: %v", err)
	}
	if _, ok := svc.(*MCPService); !ok {
		t.Fatalf("got %T, want *MCPService", svc)
	}
}

func TestNewServiceFromRequest_ReturnsInferenceService(t *testing.T) {
	req := &api.RegisterServiceRequest{
		Service: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_INFERENCE, Name: "x"},
		Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: "http://example.com"},
	}
	svc, err := NewServiceFromRequest(req)
	if err != nil {
		t.Fatalf("NewServiceFromRequest: %v", err)
	}
	if _, ok := svc.(*InferenceService); !ok {
		t.Fatalf("got %T, want *InferenceService", svc)
	}
}

func TestNewServiceFromRequest_UnspecifiedTypeFails(t *testing.T) {
	req := &api.RegisterServiceRequest{
		Service: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_UNSPECIFIED, Name: "x"},
		Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: "http://example.com"},
	}
	if _, err := NewServiceFromRequest(req); err == nil {
		t.Fatal("expected error for unspecified service type, got nil")
	}
}

func TestBuildRegisterRequest_URL(t *testing.T) {
	req, err := buildRegisterRequest(api.ServiceConfig{
		Type:      "mcp",
		Name:      "demo",
		TargetURL: "http://localhost:9000",
	})
	if err != nil {
		t.Fatalf("buildRegisterRequest: %v", err)
	}
	if req.Service.Type != api.ServiceType_SERVICE_TYPE_MCP {
		t.Errorf("Type = %v, want MCP", req.Service.Type)
	}
	tb, ok := req.Backend.(*api.RegisterServiceRequest_TargetUrl)
	if !ok {
		t.Fatalf("backend type: got %T, want *RegisterServiceRequest_TargetUrl", req.Backend)
	}
	if tb.TargetUrl != "http://localhost:9000" {
		t.Errorf("TargetUrl = %q, want http://localhost:9000", tb.TargetUrl)
	}
}

func TestBuildRegisterRequest_Command(t *testing.T) {
	req, err := buildRegisterRequest(api.ServiceConfig{
		Type:    "inference",
		Name:    "demo",
		Command: []string{"/bin/echo", "hi"},
		Env:     map[string]string{"K": "V"},
	})
	if err != nil {
		t.Fatalf("buildRegisterRequest: %v", err)
	}
	cb, ok := req.Backend.(*api.RegisterServiceRequest_Command)
	if !ok {
		t.Fatalf("backend type: got %T, want *RegisterServiceRequest_Command", req.Backend)
	}
	if got := cb.Command.Command; len(got) != 2 || got[0] != "/bin/echo" || got[1] != "hi" {
		t.Errorf("Command = %v, want [/bin/echo hi]", got)
	}
	if cb.Command.Env["K"] != "V" {
		t.Errorf("Env[K] = %q, want V", cb.Command.Env["K"])
	}
}

func TestBuildRegisterRequest_MissingBackend(t *testing.T) {
	if _, err := buildRegisterRequest(api.ServiceConfig{Type: "mcp", Name: "demo"}); err == nil {
		t.Fatal("expected error for missing backend, got nil")
	}
}

func TestBuildRegisterRequest_InvalidType(t *testing.T) {
	if _, err := buildRegisterRequest(api.ServiceConfig{Type: "bogus", Name: "demo", TargetURL: "http://x"}); err == nil {
		t.Fatal("expected error for invalid type, got nil")
	}
}
