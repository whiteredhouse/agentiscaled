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

package controlplane

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// A node declares its own labels in its enrollment request, and the control
// plane signs them into label() facts that peers then treat as attested. Peers
// gate on those facts (required_labels on call_remote_tool), so without this
// check any identity able to enrol could claim region="us-east-1" and satisfy
// every consumer requiring it. A role's allowed_labels is what makes a declared
// label worth signing.
func TestRegisterRefusesLabelsTheRoleDoesNotGrant(t *testing.T) {
	issuer, mintToken := startCustomMockOIDC(t)
	srv, store, baseURL := setupTestServer(t, issuer)
	defer func() {
		_ = srv.Close()
		_ = store.Close()
	}()

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}

	seedPolicy := func(t *testing.T, allowedLabels []string) {
		t.Helper()
		roles := []*api.PolicyRole{{
			Name:            api.RoleNode,
			AllowedServices: []string{"mcp://*"},
			AllowedLabels:   allowedLabels,
		}}
		bindings := []*api.PolicyBinding{{
			Role:    api.RoleNode,
			Members: []string{api.SystemAuthenticated},
		}}
		if err := store.SaveMeshPolicy(ctx, roles, bindings); err != nil {
			t.Fatalf("SaveMeshPolicy: %v", err)
		}
	}

	// Each attempt uses a fresh peer so it is a first enrollment every time.
	register := func(t *testing.T, labels map[string]string) int {
		t.Helper()
		priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
		if err != nil {
			t.Fatal(err)
		}
		pID, err := peer.IDFromPrivateKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		pubBytes, err := crypto.MarshalPublicKey(priv.GetPublic())
		if err != nil {
			t.Fatal(err)
		}

		ts, sig := registerPoP(t, priv, pID.String())
		body, err := proto.Marshal(&api.EnrollRequest{
			Jwt:                mintToken(map[string]interface{}{"sub": "node-alice"}),
			PeerId:             pID.String(),
			PublicKey:          pubBytes,
			RequestedRole:      api.RoleNode,
			Labels:             labels,
			ChallengeUnixMs:    ts,
			ChallengeSignature: sig,
		})
		if err != nil {
			t.Fatal(err)
		}

		resp, err := client.Post(baseURL+"/register", "application/x-protobuf", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /register: %v", err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("a role granting no labels refuses one", func(t *testing.T) {
		seedPolicy(t, nil)
		if got := register(t, map[string]string{"region": "us-east-1"}); got != http.StatusForbidden {
			t.Errorf("status = %d, want %d", got, http.StatusForbidden)
		}
	})

	t.Run("declaring no labels still enrols", func(t *testing.T) {
		seedPolicy(t, nil)
		if got := register(t, nil); got != http.StatusOK {
			t.Errorf("status = %d, want %d", got, http.StatusOK)
		}
	})

	t.Run("a granted label is accepted", func(t *testing.T) {
		seedPolicy(t, []string{"region=*"})
		if got := register(t, map[string]string{"region": "us-east-1"}); got != http.StatusOK {
			t.Errorf("status = %d, want %d", got, http.StatusOK)
		}
	})

	t.Run("a label outside the grant is refused", func(t *testing.T) {
		seedPolicy(t, []string{"region=*"})
		if got := register(t, map[string]string{"team": "platform"}); got != http.StatusForbidden {
			t.Errorf("status = %d, want %d", got, http.StatusForbidden)
		}
	})

	t.Run("an exact grant refuses another value", func(t *testing.T) {
		seedPolicy(t, []string{"region=us-east-1"})
		if got := register(t, map[string]string{"region": "eu-west-1"}); got != http.StatusForbidden {
			t.Errorf("status = %d, want %d", got, http.StatusForbidden)
		}
	})
}

func TestAllowedLabelPatternsCollectsEveryResolvedRole(t *testing.T) {
	policyRoles := []*api.PolicyRole{
		{Name: "a", AllowedLabels: []string{"region=*"}},
		{Name: "b", AllowedLabels: []string{"team=platform"}},
		{Name: "c", AllowedLabels: []string{"secret=*"}},
		nil,
	}

	got := allowedLabelPatterns([]string{"a", "b"}, policyRoles)
	if len(got) != 2 {
		t.Fatalf("allowedLabelPatterns = %v, want the grants of a and b only", got)
	}

	// A role the identity does not hold must not contribute its grants.
	if err := api.LabelPatternsAllow(got, map[string]string{"secret": "value"}); err == nil {
		t.Error("a grant from an unheld role was applied")
	}
	if err := api.LabelPatternsAllow(got, map[string]string{"region": "eu", "team": "platform"}); err != nil {
		t.Errorf("grants from held roles did not combine: %v", err)
	}
}
