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
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/logging"
	golog "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// An enrollment the policy refuses is a decision about a member, and is
// logged like the other refusals of /register (a banned node or identity, a
// label the role does not grant): an operator reading the control plane's
// log must be able to tell a missing binding from a ban, and see which
// identity was refused and what it did resolve to.
func TestRegisterLogsARoleNotBoundToTheIdentity(t *testing.T) {
	prev := golog.GetConfig()
	logging.Setup(logging.Options{RingBuffer: true})
	t.Cleanup(func() { golog.SetupLogging(prev) })

	issuer, mintToken := startCustomMockOIDC(t)
	srv, store, baseURL := setupTestServer(t, issuer)
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("closing the server: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})

	// Only group admins may enroll a node; the caller is in no group.
	roles := []*api.PolicyRole{{Name: api.RoleNode, AllowedServices: []string{"mcp://*"}}}
	bindings := []*api.PolicyBinding{{Role: api.RoleNode, Members: []string{"group:admins"}}}
	if err := store.SaveMeshPolicy(context.Background(), roles, bindings); err != nil {
		t.Fatalf("SaveMeshPolicy: %v", err)
	}

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
		Jwt:                mintToken(map[string]interface{}{"sub": "node-bob"}),
		PeerId:             pID.String(),
		PublicKey:          pubBytes,
		RequestedRole:      api.RoleNode,
		ChallengeUnixMs:    ts,
		ChallengeSignature: sig,
	})
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(baseURL+"/register", "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /register: %v", err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("reading the /register response: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("closing the /register response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	var refusal string
	for _, line := range logging.RecentLogs() {
		if strings.Contains(line, "Rejected enrollment for a role not bound to the identity") && strings.Contains(line, pID.String()) {
			refusal = line
		}
	}
	if refusal == "" {
		t.Fatalf("no log line for the refused enrollment of %s; recent logs:\n%s", pID, strings.Join(logging.RecentLogs(), "\n"))
	}
	for _, want := range []string{api.RoleNode, issuer + "|node-bob"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("refusal log line = %q, want it to name %q", refusal, want)
		}
	}
}
