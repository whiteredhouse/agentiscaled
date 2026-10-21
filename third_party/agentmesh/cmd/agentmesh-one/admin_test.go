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

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newFakeAdminAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/bootstrap-tokens", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer adm-tok" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPost:
			// Decode into the shared wire type, as the control plane does, so a
			// client that drifts from it fails here.
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"ttl_hours"`) || !strings.Contains(string(body), `"max_usages"`) {
				http.Error(w, "expected proto field names (snake_case)", http.StatusBadRequest)
				return
			}
			req := &api.BootstrapTokenCreateRequest{}
			if err := protojson.Unmarshal(body, req); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if !req.GetAutonomousRecovery() || req.GetMaxUsages() != 1 || req.GetTtlHours() != 24 || req.GetDescription() != "note" {
				http.Error(w, fmt.Sprintf("unexpected request %+v", req), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			out, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&api.BootstrapTokenCreateResponse{
				Id:         "abcdef123456",
				Token:      "mesh-bt-fresh",
				Role:       req.GetRole(),
				ExpireTime: timestamppb.New(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)),
			})
			_, _ = w.Write(out)
		case http.MethodGet:
			out, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&api.BootstrapTokenListResponse{Tokens: []*api.BootstrapToken{{
				Id:          "abcdef123456",
				Role:        api.RoleNode,
				MaxUsages:   3,
				UsagesCount: 1,
				Description: "seeded",
				ExpireTime:  timestamppb.New(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)),
			}}})
			_, _ = w.Write(out)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/admin/bootstrap-tokens/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer adm-tok" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodDelete || r.URL.Path != "/admin/bootstrap-tokens/abcdef123456" {
			http.Error(w, "wrong revoke request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/admin/revoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer adm-tok" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req api.TokenRevokeRequest
		if err := protojson.Unmarshal(body, &req); err != nil || req.PeerId != "12D3KooTestPeer" || !strings.Contains(string(body), `"peer_id"`) {
			http.Error(w, "wrong peer", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAdminClient(t *testing.T) {
	ts := newFakeAdminAPI(t)
	c := &adminClient{client: ts.Client(), server: ts.URL, token: "adm-tok"}

	created, err := c.createToken(api.RoleNode, 24, 1, "note", true)
	if err != nil {
		t.Fatalf("createToken failed: %v", err)
	}
	if created.GetToken() != "mesh-bt-fresh" || created.GetRole() != api.RoleNode {
		t.Errorf("unexpected created token: %+v", created)
	}

	list, err := c.listTokens()
	if err != nil {
		t.Fatalf("listTokens failed: %v", err)
	}
	if len(list) != 1 || list[0].GetDescription() != "seeded" || list[0].GetUsagesCount() != 1 {
		t.Errorf("unexpected token list: %+v", list)
	}

	if err := c.banPeer("12D3KooTestPeer"); err != nil {
		t.Fatalf("banPeer failed: %v", err)
	}

	// Revocation accepts the id prefix `token list` prints and sends the
	// full id; an unknown prefix never reaches the DELETE route.
	if id, err := c.revokeToken("abcdef"); err != nil || id != "abcdef123456" {
		t.Fatalf("revokeToken by prefix = (%q, %v)", id, err)
	}
	if _, err := c.revokeToken("zzz"); err == nil || !strings.Contains(err.Error(), "no bootstrap token") {
		t.Fatalf("revokeToken unknown prefix: err = %v", err)
	}

	bad := &adminClient{client: ts.Client(), server: ts.URL, token: "wrong"}
	if _, err := bad.listTokens(); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 error with wrong token, got %v", err)
	}
}

func TestResolveAdminToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "admin-token"), []byte("mesh_adm_file\n"), 0o600); err != nil {
		t.Fatalf("failed to write token file: %v", err)
	}
	explicit := filepath.Join(t.TempDir(), "my-admin-token")
	if err := os.WriteFile(explicit, []byte(" mesh_adm_explicit\n"), 0o600); err != nil {
		t.Fatalf("failed to write explicit token file: %v", err)
	}

	t.Setenv("AGENTMESH_ADMIN_TOKEN", "")
	if got, err := resolveAdminToken("", dir); err != nil || got != "mesh_adm_file" {
		t.Errorf("data-dir fallback = %q, %v; want mesh_adm_file", got, err)
	}

	t.Setenv("AGENTMESH_ADMIN_TOKEN", "mesh_adm_env")
	if got, err := resolveAdminToken("", dir); err != nil || got != "mesh_adm_env" {
		t.Errorf("env precedence = %q, %v; want mesh_adm_env", got, err)
	}
	if got, err := resolveAdminToken(explicit, dir); err != nil || got != "mesh_adm_explicit" {
		t.Errorf("--admin-token-path precedence = %q, %v; want mesh_adm_explicit (trimmed)", got, err)
	}

	// A path that is set but unusable must not fall through to env or data-dir.
	if _, err := resolveAdminToken(filepath.Join(t.TempDir(), "missing"), dir); err == nil {
		t.Error("expected an error for a missing --admin-token-path")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	_ = os.WriteFile(empty, []byte("\n"), 0o600)
	if _, err := resolveAdminToken(empty, dir); err == nil {
		t.Error("expected an error for an empty --admin-token-path")
	}

	t.Setenv("AGENTMESH_ADMIN_TOKEN", "")
	if _, err := resolveAdminToken("", t.TempDir()); err == nil {
		t.Error("expected an error when no admin token source exists")
	}
}
