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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/credprovider"
	"github.com/google/agentmesh/internal/identity"
)

type exchangerFunc func(ctx context.Context, principal string, rules []*api.TaskAuthorizationRule) (string, time.Time, error)

func (f exchangerFunc) Exchange(ctx context.Context, principal string, rules []*api.TaskAuthorizationRule) (string, time.Time, error) {
	return f(ctx, principal, rules)
}

func TestEgressServicePreserveHostAndForwardContext(t *testing.T) {
	h := newSTSNodeHarness(t)
	n := h.node
	tar := &api.TaskAuthorizationRule{
		Name: "tasks/inspect-chain",
		Rules: []*api.TaskRule{{
			AllowedServices: []string{"egress://bigquery.googleapis.com"},
		}},
	}
	taskBiscuit, err := identity.AttenuateBiscuit(n.GetIdentity(), tar)
	if err != nil {
		t.Fatal(err)
	}

	var gotHost, gotAuth, gotPrincipal, gotRoles, gotTask string
	operatorChain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotAuth = r.Header.Get("Authorization")
		gotPrincipal = r.Header.Get(api.HeaderMeshPrincipal)
		gotRoles = r.Header.Get(api.HeaderMeshRoles)
		gotTask = r.Header.Get(api.HeaderMeshTask)
		w.WriteHeader(http.StatusOK)
	}))
	defer operatorChain.Close()

	dest := &api.EgressDestination{
		Name:           "bigquery.googleapis.com",
		TargetUrl:      operatorChain.URL,
		ServedBy:       []string{api.RoleNode},
		PreserveHost:   true,
		ForwardContext: true,
	}
	svc, err := newEgressServiceForNode(n, dest, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.SetExchanger(credprovider.NewStaticSecretExchanger(t.TempDir(), ""))
	svc.SetExchanger(exchangerFunc(func(_ context.Context, principal string, rules []*api.TaskAuthorizationRule) (string, time.Time, error) {
		return "brokered-cloud-token-for-" + principal, time.Now().Add(time.Minute), nil
	}))
	if err := svc.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/bigquery/v2/projects/p/datasets", nil)
	// Include spoofed headers that must be stripped and replaced by verified Biscuit context.
	req.Header.Set("Authorization", "Bearer caller-secret-must-be-stripped")
	req.Header.Set(api.HeaderMeshPrincipal, "spoofed-principal")
	req = req.WithContext(WithCallerBiscuit(req.Context(), taskBiscuit))

	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotHost != "bigquery.googleapis.com" {
		t.Fatalf("expected preserved Host bigquery.googleapis.com, got %q", gotHost)
	}
	if gotAuth != "Bearer brokered-cloud-token-for-alice@example.com" {
		t.Fatalf("expected brokered Authorization, got %q", gotAuth)
	}
	if gotPrincipal != "alice@example.com" {
		t.Fatalf("expected X-Mesh-Principal alice@example.com, got %q", gotPrincipal)
	}
	if !strings.Contains(gotRoles, api.RoleNode) {
		t.Fatalf("expected X-Mesh-Roles to contain %s, got %q", api.RoleNode, gotRoles)
	}
	if gotTask != "tasks/inspect-chain" {
		t.Fatalf("expected X-Mesh-Task tasks/inspect-chain, got %q", gotTask)
	}
}
