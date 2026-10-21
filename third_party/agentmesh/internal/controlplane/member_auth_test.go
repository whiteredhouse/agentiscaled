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
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/identity"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// A member credential that is presented and not accepted is counted under
// the reason it was refused for, and one that is accepted, or absent, is
// not counted at all.
func TestAdmittedNodeCountsRefusalsByReason(t *testing.T) {
	issuer, _ := startCustomMockOIDC(t)
	srv, store, baseURL := setupTestServer(t, issuer)
	defer func() {
		_ = srv.Close()
		_ = store.Close()
	}()
	const adminToken = "super-secret-admin-token"
	srv.config.AdminToken = adminToken
	srv.config.AutoApproveEnrollment = true
	ctx := context.Background()
	if err := store.SaveMeshPolicy(ctx, []*api.PolicyRole{{Name: api.RoleNode}}, nil); err != nil {
		t.Fatal(err)
	}

	enroll := func(t *testing.T) (*api.BootstrapEnrollResponse, peer.ID) {
		t.Helper()
		token := createAdminBootstrapToken(t, baseURL, adminToken, api.RoleNode, 1)
		priv, pub, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
		if err != nil {
			t.Fatal(err)
		}
		pID, err := peer.IDFromPrivateKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		pubBytes, err := crypto.MarshalPublicKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		out := bootstrapEnroll(t, baseURL, token, priv, pID, pubBytes, api.RoleNode, nil)
		if out.Status != api.EnrollmentStatus_ENROLLMENT_STATUS_APPROVED {
			t.Fatalf("enroll: status %v (%s)", out.Status, out.ErrorMessage)
		}
		return out, pID
	}
	request := func(bearer string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/policies", nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		return r
	}
	encode := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

	admitted, admittedID := enroll(t)
	banned, bannedID := enroll(t)
	if err := store.SetNodeBanned(ctx, bannedID.String(), true); err != nil {
		t.Fatal(err)
	}
	expired, expiredID := enroll(t)
	expiredRecord, err := store.GetNode(ctx, expiredID.String())
	if err != nil {
		t.Fatal(err)
	}
	expiredRecord.ExpiresAt = time.Now().Add(-time.Minute)
	if err := store.EnrollNode(ctx, expiredRecord); err != nil {
		t.Fatal(err)
	}
	// A credential this control plane signed for a node its store has no
	// record of: what every enrolled node holds after the store is reset.
	signingKey, _, err := store.GetCurrentKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, strangerID := newTestKey(t)
	stranger, err := identity.MintBootstrapBiscuitToken(signingKey, strangerID, api.RoleNode, time.Now().Add(time.Hour), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A credential signed by a key this control plane never had.
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := identity.MintBootstrapBiscuitToken(otherKey, admittedID, api.RoleNode, time.Now().Add(time.Hour), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	reasons := []string{refusalMalformed, refusalInvalidBiscuit, refusalUnknownNode, refusalBanned, refusalSessionExpired}
	cases := []struct {
		name   string
		bearer string
		admit  bool
		reason string
	}{
		{name: "no credential", bearer: "", admit: false, reason: ""},
		{name: "admitted node", bearer: encode(admitted.BiscuitToken), admit: true, reason: ""},
		{name: "not base64", bearer: "not*base64", admit: false, reason: refusalMalformed},
		{name: "not a biscuit", bearer: encode([]byte("not a biscuit")), admit: false, reason: refusalInvalidBiscuit},
		{name: "signed by another control plane", bearer: encode(forged), admit: false, reason: refusalInvalidBiscuit},
		{name: "node unknown to the store", bearer: encode(stranger), admit: false, reason: refusalUnknownNode},
		{name: "banned node", bearer: encode(banned.BiscuitToken), admit: false, reason: refusalBanned},
		{name: "session expired", bearer: encode(expired.BiscuitToken), admit: false, reason: refusalSessionExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := make(map[string]float64, len(reasons))
			for _, reason := range reasons {
				before[reason] = counterValue(t, memberAuthRefusalsTotal, reason)
			}
			record, err := srv.admittedNode(request(tc.bearer))
			if err != nil {
				t.Fatalf("admittedNode: %v", err)
			}
			if (record != nil) != tc.admit {
				t.Fatalf("admitted = %v, want %v", record != nil, tc.admit)
			}
			for _, reason := range reasons {
				want := before[reason]
				if reason == tc.reason {
					want++
				}
				if got := counterValue(t, memberAuthRefusalsTotal, reason); got != want {
					t.Errorf("refusals{reason=%q} = %v, want %v", reason, got, want)
				}
			}
		})
	}
}
