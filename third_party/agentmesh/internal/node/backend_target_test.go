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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/agentmesh/api"
)

func TestParseBackendTarget(t *testing.T) {
	tests := []struct {
		raw      string
		wantURL  string
		wantAuth string
	}{
		{"http://127.0.0.1:9090", "http://127.0.0.1:9090", ""},
		{"http://:s3cret@127.0.0.1:9090/mcp", "http://127.0.0.1:9090/mcp", "Bearer s3cret"},
		{"http://alice:pw@backend.example/v1", "http://backend.example/v1", "Basic YWxpY2U6cHc="},
		// Percent-encoded userinfo is decoded before it becomes a header.
		{"http://alice:p%40ss@backend.example", "http://backend.example", "Basic YWxpY2U6cEBzcw=="},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := parseBackendTarget(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.url.String() != tt.wantURL {
				t.Errorf("url = %q, want %q", got.url.String(), tt.wantURL)
			}
			if got.auth != tt.wantAuth {
				t.Errorf("auth = %q, want %q", got.auth, tt.wantAuth)
			}
			if strings.Contains(got.url.String(), "@") {
				t.Errorf("userinfo leaked into the dialled URL %q", got.url)
			}
		})
	}
	// A malformed URL must not be echoed: url.Error prints it, credential
	// included.
	_, err := parseBackendTarget("http://:leaked-secret@[::1")
	if err == nil {
		t.Fatal("an unparsable URL must be an error")
	}
	if strings.Contains(err.Error(), "leaked-secret") {
		t.Errorf("parse error echoes the credential: %v", err)
	}
}

// Config files are copied, committed and rendered into ConfigMaps, so a
// credential written into target_url is refused there; target_auth_path
// reads it from a file and composes the in-memory URL instead.
func TestBackendCredentialComesFromAFileNotTheConfig(t *testing.T) {
	if err := rejectInlineBackendCredential("http://:tok@127.0.0.1:9090"); err == nil {
		t.Error("a credential inside target_url must be refused in a config file")
	}
	if err := rejectInlineBackendCredential("http://127.0.0.1:9090"); err != nil {
		t.Errorf("a plain target_url must be accepted: %v", err)
	}

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("phone-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	basicFile := filepath.Join(dir, "basic")
	if err := os.WriteFile(basicFile, []byte("alice:p@ss"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name, path, wantAuth string
	}{
		{"bearer", tokenFile, "Bearer phone-token"},
		{"basic", basicFile, "Basic YWxpY2U6cEBzcw=="},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildRegisterRequest(api.ServiceConfig{
				Type: "mcp", Name: "sensors", TargetURL: "http://127.0.0.1:9090/mcp", TargetAuthPath: tt.path,
			})
			if err != nil {
				t.Fatal(err)
			}
			target, err := parseBackendTarget(req.GetTargetUrl())
			if err != nil {
				t.Fatal(err)
			}
			if target.auth != tt.wantAuth {
				t.Errorf("auth = %q, want %q", target.auth, tt.wantAuth)
			}
			if target.url.String() != "http://127.0.0.1:9090/mcp" {
				t.Errorf("dialled URL = %q", target.url)
			}
		})
	}

	if _, err := buildRegisterRequest(api.ServiceConfig{Type: "mcp", Name: "s", TargetURL: "http://127.0.0.1:1", TargetAuthPath: filepath.Join(dir, "missing")}); err == nil {
		t.Error("a missing credential file must fail the service, not silently run unauthenticated")
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildRegisterRequest(api.ServiceConfig{Type: "mcp", Name: "s", TargetURL: "http://127.0.0.1:1", TargetAuthPath: empty}); err == nil {
		t.Error("an empty credential file must be an error")
	}
}

// The credential in target_url is the node's for the backend: it is sent on
// every proxied request and overrides whatever Authorization the caller
// sent, which was for the node.
func TestReverseProxySendsBackendCredential(t *testing.T) {
	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	h, err := newReverseProxyHandler(strings.Replace(backend.URL, "http://", "http://:phone-token@", 1))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if gotAuth != "Bearer phone-token" {
		t.Errorf("backend saw Authorization %q, want the configured backend credential", gotAuth)
	}

	// Without a configured credential the caller's header passes through.
	plain, err := newReverseProxyHandler(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	plain.ServeHTTP(httptest.NewRecorder(), req)
	if gotAuth != "Bearer caller-token" {
		t.Errorf("without a backend credential the caller's Authorization must pass through, got %q", gotAuth)
	}
}
