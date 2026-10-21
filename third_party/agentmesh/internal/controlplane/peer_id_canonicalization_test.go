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
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/storage"
	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

func cidAlias(t *testing.T, id peer.ID) string {
	t.Helper()
	alias := peer.ToCid(id).String()
	if alias == id.String() {
		t.Fatalf("peer.ToCid(%s) did not produce a second spelling", id)
	}
	decoded, err := peer.Decode(alias)
	if err != nil {
		t.Fatalf("alias %q does not decode: %v", alias, err)
	}
	if decoded != id {
		t.Fatalf("alias %q decodes to %s, want %s", alias, decoded, id)
	}
	return alias
}

func newTestKey(t *testing.T) (crypto.PrivKey, peer.ID) {
	t.Helper()
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to derive peer id: %v", err)
	}
	return priv, id
}

type recordingMesh struct {
	mu     sync.Mutex
	banned []string
}

func (m *recordingMesh) PublishEvent(ctx context.Context, eventType api.MeshEvent_Type, peerID string, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if eventType == api.MeshEvent_BANNED {
		m.banned = append(m.banned, peerID)
	}
	return nil
}

func (m *recordingMesh) DiscoverServices(ctx context.Context, serviceType string) ([]*ServiceAnnouncement, error) {
	return nil, nil
}

func (m *recordingMesh) GetNodeStatus(ctx context.Context, peerID string) (*NodeStatus, error) {
	return nil, nil
}

func (m *recordingMesh) Close() error {
	return nil
}

func (m *recordingMesh) bannedPeers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.banned...)
}

func enrollNodeRow(t *testing.T, store storage.Store, peerID string, pubKey []byte, ownerID string) {
	t.Helper()
	err := store.EnrollNode(context.Background(), &storage.EnrolledNode{
		PeerID:    peerID,
		PublicKey: pubKey,
		Biscuit:   []byte("biscuit"),
		Role:      api.RoleNode,
		OwnerID:   ownerID,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("failed to enroll node %q: %v", peerID, err)
	}
}

func TestBannedNodeCannotRegisterUnderAnAlias(t *testing.T) {
	issuer, mintToken := startCustomMockOIDC(t)
	srv, store, baseURL := setupTestServer(t, issuer)
	defer func() {
		_ = srv.Close()
		_ = store.Close()
	}()
	srv.config.AdminToken = "super-secret-admin-token"

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}

	if err := store.SaveMeshPolicy(ctx, nil, []*api.PolicyBinding{
		{Role: api.RoleNode, Members: []string{api.SystemAuthenticated}},
	}); err != nil {
		t.Fatal(err)
	}

	register := func(t *testing.T, priv crypto.PrivKey, peerID, sub string) int {
		t.Helper()
		pubBytes, err := crypto.MarshalPublicKey(priv.GetPublic())
		if err != nil {
			t.Fatal(err)
		}
		// The challenge is over the canonical id whatever spelling the request
		// carries, as the control plane canonicalizes before checking it.
		decoded, err := peer.Decode(peerID)
		if err != nil {
			t.Fatal(err)
		}
		ts, sig := registerPoP(t, priv, decoded.String())
		reqData, err := proto.Marshal(&api.EnrollRequest{
			Jwt:                mintToken(map[string]interface{}{"sub": sub}),
			PeerId:             peerID,
			PublicKey:          pubBytes,
			RequestedRole:      api.RoleNode,
			ChallengeUnixMs:    ts,
			ChallengeSignature: sig,
		})
		if err != nil {
			t.Fatalf("failed to marshal enroll request: %v", err)
		}
		resp, err := client.Post(baseURL+"/register", "application/x-protobuf", bytes.NewReader(reqData))
		if err != nil {
			t.Fatalf("/register failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	priv, id := newTestKey(t)
	alias := cidAlias(t, id)

	if status := register(t, priv, id.String(), "alias-attacker"); status != http.StatusOK {
		t.Fatalf("initial registration: got %d, want 200", status)
	}
	if _, err := store.GetNode(ctx, id.String()); err != nil {
		t.Fatalf("node was not stored under its canonical id: %v", err)
	}

	revokeData, err := proto.Marshal(&api.TokenRevokeRequest{PeerId: id.String()})
	if err != nil {
		t.Fatalf("failed to marshal revoke request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/admin/revoke", bytes.NewReader(revokeData))
	if err != nil {
		t.Fatalf("failed to create revoke request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer super-secret-admin-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("/admin/revoke failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/admin/revoke: got %d, want 200", resp.StatusCode)
	}

	if status := register(t, priv, alias, "alias-attacker"); status != http.StatusForbidden {
		t.Errorf("re-registration under an alias: got %d, want 403", status)
	}
	if status := register(t, priv, alias, "a-brand-new-identity"); status != http.StatusForbidden {
		t.Errorf("re-registration under an alias with a fresh identity: got %d, want 403", status)
	}
	if _, err := store.GetNode(ctx, alias); err != storage.ErrNotFound {
		t.Errorf("the alias grew a row of its own: err = %v", err)
	}

	otherPriv, otherID := newTestKey(t)
	if status := register(t, otherPriv, cidAlias(t, otherID), "somebody-else"); status != http.StatusOK {
		t.Errorf("unbanned identity under an alias: got %d, want 200", status)
	}
	if _, err := store.GetNode(ctx, otherID.String()); err != nil {
		t.Errorf("unbanned identity was not stored under its canonical id: %v", err)
	}
}

func TestRevokeBansTheIdentityNotTheSpelling(t *testing.T) {
	cases := []struct {
		name   string
		revoke func(t *testing.T, baseURL, peerID, userToken, adminToken string, client *http.Client) int
	}{
		{
			name: "admin revoke",
			revoke: func(t *testing.T, baseURL, peerID, userToken, adminToken string, client *http.Client) int {
				t.Helper()
				body, err := proto.Marshal(&api.TokenRevokeRequest{PeerId: peerID})
				if err != nil {
					t.Fatalf("failed to marshal revoke request: %v", err)
				}
				req, err := http.NewRequest(http.MethodPost, baseURL+"/admin/revoke", bytes.NewReader(body))
				if err != nil {
					t.Fatalf("failed to create revoke request: %v", err)
				}
				req.Header.Set("Authorization", "Bearer "+adminToken)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("/admin/revoke failed: %v", err)
				}
				defer func() { _ = resp.Body.Close() }()
				return resp.StatusCode
			},
		},
		{
			name: "user revoke",
			revoke: func(t *testing.T, baseURL, peerID, userToken, adminToken string, client *http.Client) int {
				t.Helper()
				req, err := http.NewRequest(http.MethodPost, baseURL+"/user/revoke?id="+peerID, nil)
				if err != nil {
					t.Fatalf("failed to create user revoke request: %v", err)
				}
				req.Header.Set("Authorization", "Bearer "+userToken)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("/user/revoke failed: %v", err)
				}
				defer func() { _ = resp.Body.Close() }()
				return resp.StatusCode
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issuer, mintToken := startCustomMockOIDC(t)
			srv, store, baseURL := setupTestServer(t, issuer)
			defer func() {
				_ = srv.Close()
				_ = store.Close()
			}()
			srv.config.AdminToken = "super-secret-admin-token"

			mesh := &recordingMesh{}
			srv.SetMeshAdapter(mesh)

			ctx := context.Background()
			client := &http.Client{Timeout: 5 * time.Second}

			user := &storage.User{
				ID:        "owner-sub",
				Email:     "owner@example.com",
				Role:      "user",
				CreatedAt: time.Now(),
			}
			if err := store.SaveUser(ctx, user); err != nil {
				t.Fatalf("failed to save user: %v", err)
			}

			priv, id := newTestKey(t)
			alias := cidAlias(t, id)
			pubBytes, err := crypto.MarshalPublicKey(priv.GetPublic())
			if err != nil {
				t.Fatal(err)
			}
			enrollNodeRow(t, store, id.String(), pubBytes, user.ID)

			token := mintToken(map[string]interface{}{
				"iss":   issuer,
				"sub":   user.ID,
				"email": user.Email,
				"aud":   "agentmesh-audience",
			})
			if status := tc.revoke(t, baseURL, alias, token, "super-secret-admin-token", client); status != http.StatusOK {
				t.Fatalf("revoking through an alias: got %d, want 200", status)
			}

			node, err := store.GetNode(ctx, id.String())
			if err != nil {
				t.Fatalf("canonical row is gone: %v", err)
			}
			if !node.Banned {
				t.Error("the ban did not land on the identity the alias names")
			}
			if _, err := store.GetNode(ctx, alias); err != storage.ErrNotFound {
				t.Errorf("the revoke created a row under the alias: err = %v", err)
			}

			banned := mesh.bannedPeers()
			if len(banned) != 1 {
				t.Fatalf("published %d BANNED events, want 1: %v", len(banned), banned)
			}
			if banned[0] != id.String() {
				t.Errorf("BANNED event names %q, want the canonical %q", banned[0], id.String())
			}
		})
	}
}

func TestEnrollStatusReachesTheRecordThroughAnAlias(t *testing.T) {
	issuer, _ := startCustomMockOIDC(t)
	srv, store, baseURL := setupTestServer(t, issuer)
	defer func() {
		_ = srv.Close()
		_ = store.Close()
	}()

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}

	priv, id := newTestKey(t)
	alias := cidAlias(t, id)
	pubBytes, err := crypto.MarshalPublicKey(priv.GetPublic())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateEnrollmentRequest(ctx, &storage.EnrollmentRequest{
		ID:        "req-1",
		PeerID:    id.String(),
		PublicKey: pubBytes,
		TokenID:   "token-1",
		Status:    api.EnrollmentStatus_ENROLLMENT_STATUS_PENDING,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("failed to create enrollment request: %v", err)
	}

	status := func(t *testing.T, query, signed string) int {
		t.Helper()
		ts := time.Now().UnixMilli()
		sig, err := priv.Sign(api.EnrollStatusChallenge(signed, ts))
		if err != nil {
			t.Fatalf("failed to sign challenge: %v", err)
		}
		req, err := http.NewRequest(http.MethodGet, baseURL+"/enroll/status?peer_id="+query, nil)
		if err != nil {
			t.Fatalf("failed to create enroll status request: %v", err)
		}
		req.Header.Set(api.HeaderChallengeTimestamp, strconv.FormatInt(ts, 10))
		req.Header.Set(api.HeaderChallengeSignature, base64.RawURLEncoding.EncodeToString(sig))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("/enroll/status failed: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	if got := status(t, id.String(), id.String()); got != http.StatusOK {
		t.Errorf("canonical poll: got %d, want 200", got)
	}
	if got := status(t, alias, id.String()); got != http.StatusOK {
		t.Errorf("poll through an alias: got %d, want 200", got)
	}
	if got := status(t, alias, alias); got != http.StatusUnauthorized {
		t.Errorf("challenge signed over the alias: got %d, want 401", got)
	}
}

func TestPublishEventValidatesPeerID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, err := storage.NewSQLStore("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer func() { _ = store.Close() }()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	if err := store.SaveInitialKey(ctx, priv, pub); err != nil {
		t.Fatalf("failed to save key: %v", err)
	}

	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("failed to create host: %v", err)
	}
	defer func() { _ = h.Close() }()

	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		t.Fatalf("failed to create pubsub: %v", err)
	}
	topic, err := ps.Join(api.GossipEvents)
	if err != nil {
		t.Fatalf("failed to join topic: %v", err)
	}

	adapter, err := NewP2PMeshAdapter(h, topic, store)
	if err != nil {
		t.Fatalf("failed to create P2PMeshAdapter: %v", err)
	}
	defer func() { _ = adapter.Close() }()

	if err := adapter.PublishEvent(ctx, api.MeshEvent_POLICY_UPDATE, "", nil); err != nil {
		t.Errorf("empty peer ID should be accepted: %v", err)
	}

	if err := adapter.PublishEvent(ctx, api.MeshEvent_BANNED, "peer-node-a", nil); err == nil {
		t.Error("expected an error for an undecodable peer ID, got nil")
	}
}
