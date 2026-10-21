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
	"encoding/base64"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	cpclient "github.com/google/agentmesh/internal/controlplane/client"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// TestPoliciesRequiresAnAdmissibleNode pins that reading mesh policy applies the
// same admission rules as refreshing a token. The GET handler used to accept any
// node record that was not explicitly banned, so a node whose OIDC session had
// lapsed kept reading roles, bindings and allowed targets until an operator
// banned it by hand. Passive expiry is the backstop that has to work on its own.
func TestPoliciesRequiresAnAdmissibleNode(t *testing.T) {
	issuer, mintToken := startCustomMockOIDC(t)
	srv, store, baseURL := setupTestServer(t, issuer)
	defer func() {
		_ = srv.Close()
		_ = store.Close()
	}()

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}

	// Enrollment requires the requested role to resolve from a binding.
	nodeBindings := []*api.PolicyBinding{{Role: api.RoleNode, Members: []string{"group:users"}}}
	if err := store.SaveMeshPolicy(ctx, nil, nodeBindings); err != nil {
		t.Fatalf("failed to seed policy: %v", err)
	}

	privNode, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}
	nodePeer, err := peer.IDFromPrivateKey(privNode)
	if err != nil {
		t.Fatal(err)
	}
	nodePubKeyBytes, err := crypto.MarshalPublicKey(privNode.GetPublic())
	if err != nil {
		t.Fatal(err)
	}

	ts, sig := registerPoP(t, privNode, nodePeer.String())
	enrollReq := &api.EnrollRequest{
		Jwt:                mintToken(map[string]interface{}{"sub": "node-alice", "groups": []string{"users"}}),
		PeerId:             nodePeer.String(),
		PublicKey:          nodePubKeyBytes,
		RequestedRole:      api.RoleNode,
		ChallengeUnixMs:    ts,
		ChallengeSignature: sig,
	}
	reqData, err := proto.Marshal(enrollReq)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Post(baseURL+"/register", "application/x-protobuf", bytes.NewReader(reqData))
	if err != nil {
		t.Fatalf("node /register failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("node /register status %s (body: %s)", resp.Status, body)
	}

	var enrollResp api.EnrollResponse
	if err := proto.Unmarshal(body, &enrollResp); err != nil {
		t.Fatalf("failed to unmarshal EnrollResponse: %v", err)
	}

	getPolicies := func(t *testing.T) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, baseURL+"/policies", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(enrollResp.BiscuitToken))
		cTS := time.Now().UnixMilli()
		cSig, err := privNode.Sign(api.PoliciesChallenge(nodePeer.String(), cTS))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(api.HeaderChallengeTimestamp, strconv.FormatInt(cTS, 10))
		req.Header.Set(api.HeaderChallengeSignature, base64.RawURLEncoding.EncodeToString(cSig))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /policies failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			// What a member receives is the policy as Datalog text and nothing else.
			var policy api.PolicyConfigGetResponse
			if err := proto.Unmarshal(body, &policy); err != nil {
				t.Fatalf("decoding PolicyConfigGetResponse: %v", err)
			}
			if want := `role("mesh:role:node") <- group("users")`; !slices.Contains(policy.DatalogRules, want) {
				t.Errorf("datalog_rules = %q, want it to contain %q", policy.DatalogRules, want)
			}
			if len(policy.ProtoReflect().GetUnknown()) > 0 {
				t.Errorf("policy response carries fields outside the contract: %x", policy.ProtoReflect().GetUnknown())
			}
		}
		return resp.StatusCode
	}

	// The token is the same throughout; only the node record changes, so any
	// difference below comes from the admission check and not from the biscuit.
	if got := getPolicies(t); got != http.StatusOK {
		t.Fatalf("freshly enrolled node got status %d, want %d", got, http.StatusOK)
	}

	expireSession := func(t *testing.T, at time.Time) {
		t.Helper()
		record, err := store.GetNode(ctx, nodePeer.String())
		if err != nil {
			t.Fatalf("GetNode: %v", err)
		}
		record.ExpiresAt = at
		if err := store.EnrollNode(ctx, record); err != nil {
			t.Fatalf("EnrollNode: %v", err)
		}
	}

	expireSession(t, time.Now().Add(-time.Hour))
	if got := getPolicies(t); got != http.StatusUnauthorized {
		t.Errorf("node with a lapsed session got status %d, want %d", got, http.StatusUnauthorized)
	}

	expireSession(t, time.Now().Add(time.Hour))
	if got := getPolicies(t); got != http.StatusOK {
		t.Errorf("node with a renewed session got status %d, want %d", got, http.StatusOK)
	}

	if err := store.SetNodeBanned(ctx, nodePeer.String(), true); err != nil {
		t.Fatalf("SetNodeBanned: %v", err)
	}
	if got := getPolicies(t); got != http.StatusUnauthorized {
		t.Errorf("banned node got status %d, want %d", got, http.StatusUnauthorized)
	}
}

func TestReadAndReportEndpointsRequireProofOfPossession(t *testing.T) {
	issuer, mintToken := startCustomMockOIDC(t)
	srv, store, baseURL := setupTestServer(t, issuer)
	defer func() {
		_ = srv.Close()
		_ = store.Close()
	}()

	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}

	nodeBindings := []*api.PolicyBinding{{Role: api.RoleNode, Members: []string{"group:users"}}}
	if err := store.SaveMeshPolicy(ctx, nil, nodeBindings); err != nil {
		t.Fatalf("failed to seed policy: %v", err)
	}

	privNode, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}
	nodePeer, err := peer.IDFromPrivateKey(privNode)
	if err != nil {
		t.Fatal(err)
	}
	nodePubKeyBytes, err := crypto.MarshalPublicKey(privNode.GetPublic())
	if err != nil {
		t.Fatal(err)
	}
	otherPriv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}

	ts, sig := registerPoP(t, privNode, nodePeer.String())
	enrollReq := &api.EnrollRequest{
		Jwt:                mintToken(map[string]interface{}{"sub": "node-pop", "groups": []string{"users"}}),
		PeerId:             nodePeer.String(),
		PublicKey:          nodePubKeyBytes,
		RequestedRole:      api.RoleNode,
		ChallengeUnixMs:    ts,
		ChallengeSignature: sig,
	}
	reqData, _ := proto.Marshal(enrollReq)
	resp, err := client.Post(baseURL+"/register", "application/x-protobuf", bytes.NewReader(reqData))
	if err != nil {
		t.Fatalf("node /register failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("node /register status %s (body: %s)", resp.Status, body)
	}
	var enrollResp api.EnrollResponse
	if err := proto.Unmarshal(body, &enrollResp); err != nil {
		t.Fatal(err)
	}
	authHeader := "Bearer " + base64.StdEncoding.EncodeToString(enrollResp.BiscuitToken)
	catalogBody, _ := proto.Marshal(&api.NodeCatalogReport{})

	endpoints := []struct {
		name       string
		method     string
		path       string
		body       []byte
		challenge  func(string, int64) []byte
		wrongDom   func(string, int64) []byte
		wantStatus int
	}{
		{
			name:       "GET /policies",
			method:     http.MethodGet,
			path:       "/policies",
			challenge:  api.PoliciesChallenge,
			wrongDom:   api.EgressChallenge,
			wantStatus: http.StatusOK,
		},
		{
			name:       "GET /egress",
			method:     http.MethodGet,
			path:       "/egress",
			challenge:  api.EgressChallenge,
			wrongDom:   api.PoliciesChallenge,
			wantStatus: http.StatusOK,
		},
		{
			name:       "GET /revocations",
			method:     http.MethodGet,
			path:       "/revocations",
			challenge:  api.RevocationsChallenge,
			wrongDom:   api.PoliciesChallenge,
			wantStatus: http.StatusOK,
		},
		{
			name:       "POST /nodes/catalog",
			method:     http.MethodPost,
			path:       "/nodes/catalog",
			body:       catalogBody,
			challenge:  api.NodesCatalogChallenge,
			wrongDom:   api.PoliciesChallenge,
			wantStatus: http.StatusNoContent,
		},
	}

	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			doReq := func(tsHeader string, sig []byte) (*http.Response, string) {
				var rBody io.Reader
				if ep.body != nil {
					rBody = bytes.NewReader(ep.body)
				}
				req, err := http.NewRequest(ep.method, baseURL+ep.path, rBody)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", authHeader)
				if ep.body != nil {
					req.Header.Set("Content-Type", "application/x-protobuf")
				}
				if tsHeader != "" {
					req.Header.Set(api.HeaderChallengeTimestamp, tsHeader)
				}
				if sig != nil {
					req.Header.Set(api.HeaderChallengeSignature, base64.RawURLEncoding.EncodeToString(sig))
				}
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(res.Body)
				_ = res.Body.Close()
				return res, string(b)
			}

			// 1. Bare bearer Biscuit without challenge headers -> 401.
			if res, _ := doReq("", nil); res.StatusCode != http.StatusUnauthorized {
				t.Errorf("bare biscuit got %d, want 401", res.StatusCode)
			}

			// 2. Stale timestamp (+10 min) -> 401 with stale challenge message and Date header.
			staleTS := time.Now().Add(10 * time.Minute).UnixMilli()
			staleSig, _ := privNode.Sign(ep.challenge(nodePeer.String(), staleTS))
			res, msg := doReq(strconv.FormatInt(staleTS, 10), staleSig)
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("stale challenge got %d, want 401", res.StatusCode)
			}
			if !strings.Contains(msg, api.ErrStaleChallengeTimestampMessage) {
				t.Errorf("stale challenge body = %q, want %q", msg, api.ErrStaleChallengeTimestampMessage)
			}
			if res.Header.Get("Date") == "" {
				t.Errorf("stale challenge 401 missing Date header")
			}

			// 3. Signature for another endpoint -> 401.
			nowTS := time.Now().UnixMilli()
			wrongDomSig, _ := privNode.Sign(ep.wrongDom(nodePeer.String(), nowTS))
			if res, _ := doReq(strconv.FormatInt(nowTS, 10), wrongDomSig); res.StatusCode != http.StatusUnauthorized {
				t.Errorf("wrong domain challenge got %d, want 401", res.StatusCode)
			}

			// 4. Signature by another key -> 401.
			wrongKeySig, _ := otherPriv.Sign(ep.challenge(nodePeer.String(), nowTS))
			if res, _ := doReq(strconv.FormatInt(nowTS, 10), wrongKeySig); res.StatusCode != http.StatusUnauthorized {
				t.Errorf("wrong key challenge got %d, want 401", res.StatusCode)
			}

			// 5. Fresh valid challenge -> wantStatus.
			validSig, _ := privNode.Sign(ep.challenge(nodePeer.String(), nowTS))
			if res, body := doReq(strconv.FormatInt(nowTS, 10), validSig); res.StatusCode != ep.wantStatus {
				t.Errorf("valid challenge got %d (%s), want %d", res.StatusCode, body, ep.wantStatus)
			}
		})
	}
}

func TestChallengeClockSkewRecoveryViaDateHeader(t *testing.T) {
	issuer, mintToken := startCustomMockOIDC(t)
	srv, store, baseURL := setupTestServer(t, issuer)
	defer func() {
		_ = srv.Close()
		_ = store.Close()
	}()

	ctx := context.Background()
	nodeBindings := []*api.PolicyBinding{{Role: api.RoleNode, Members: []string{"group:users"}}}
	if err := store.SaveMeshPolicy(ctx, nil, nodeBindings); err != nil {
		t.Fatalf("failed to seed policy: %v", err)
	}

	privNode, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}
	nodePeer, err := peer.IDFromPrivateKey(privNode)
	if err != nil {
		t.Fatal(err)
	}
	nodePubKeyBytes, err := crypto.MarshalPublicKey(privNode.GetPublic())
	if err != nil {
		t.Fatal(err)
	}

	// Verify GET /enroll/status checks timestamp freshness before looking up the peer ID,
	// so it is never a peer-ID existence oracle and skewed callers get 401 (not 404).
	staleTS := time.Now().Add(10 * time.Minute).UnixMilli()
	staleSig, _ := privNode.Sign(api.EnrollStatusChallenge(nodePeer.String(), staleTS))
	statusReq, _ := http.NewRequest(http.MethodGet, baseURL+"/enroll/status?peer_id="+nodePeer.String(), nil)
	statusReq.Header.Set(api.HeaderChallengeTimestamp, strconv.FormatInt(staleTS, 10))
	statusReq.Header.Set(api.HeaderChallengeSignature, base64.RawURLEncoding.EncodeToString(staleSig))
	statusResp, err := http.DefaultClient.Do(statusReq)
	if err != nil {
		t.Fatal(err)
	}
	statusBody, _ := io.ReadAll(statusResp.Body)
	_ = statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(statusBody), api.ErrStaleChallengeTimestampMessage) {
		t.Fatalf("GET /enroll/status with stale timestamp got %d (%q), want 401 with %q", statusResp.StatusCode, statusBody, api.ErrStaleChallengeTimestampMessage)
	}

	// Enroll the node using a client with a +10 min clock via DoWithChallenge.
	var roundTrips atomic.Int32
	countingTransport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		roundTrips.Add(1)
		return http.DefaultTransport.RoundTrip(req)
	})
	httpClient := &http.Client{Timeout: 5 * time.Second, Transport: countingTransport}
	skewedClock := func() time.Time {
		return time.Now().Add(10 * time.Minute)
	}
	skewedCP := cpclient.New(baseURL, httpClient).WithIdentity(nodePeer.String(), privNode).WithClock(skewedClock)

	regResp, err := cpclient.DoWithChallenge(httpClient, skewedClock, func(cTS int64) (*http.Request, error) {
		cSig, signErr := privNode.Sign(api.RegisterChallenge(nodePeer.String(), cTS))
		if signErr != nil {
			return nil, signErr
		}
		payload, marshalErr := proto.Marshal(&api.EnrollRequest{
			Jwt:                mintToken(map[string]interface{}{"sub": "node-skewed", "groups": []string{"users"}}),
			PeerId:             nodePeer.String(),
			PublicKey:          nodePubKeyBytes,
			RequestedRole:      api.RoleNode,
			ChallengeUnixMs:    cTS,
			ChallengeSignature: cSig,
		})
		if marshalErr != nil {
			return nil, marshalErr
		}
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/register", bytes.NewReader(payload))
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Content-Type", "application/x-protobuf")
		return req, nil
	})
	if err != nil {
		t.Fatalf("skewed /register failed: %v", err)
	}
	regBody, _ := io.ReadAll(regResp.Body)
	_ = regResp.Body.Close()
	if regResp.StatusCode != http.StatusOK {
		t.Fatalf("skewed /register status %d: %s", regResp.StatusCode, regBody)
	}
	var enrollResp api.EnrollResponse
	if err := proto.Unmarshal(regBody, &enrollResp); err != nil {
		t.Fatal(err)
	}
	if got := roundTrips.Load(); got != 2 {
		t.Errorf("skewed /register took %d round trips, want 2", got)
	}

	roundTrips.Store(0)
	if _, err := skewedCP.FetchPolicy(ctx, enrollResp.BiscuitToken); err != nil {
		t.Fatalf("skewed FetchPolicy failed: %v", err)
	}
	if got := roundTrips.Load(); got != 2 {
		t.Errorf("skewed FetchPolicy took %d round trips, want 2", got)
	}

	roundTrips.Store(0)
	if _, err := skewedCP.FetchEgress(ctx, enrollResp.BiscuitToken); err != nil {
		t.Fatalf("skewed FetchEgress failed: %v", err)
	}
	if got := roundTrips.Load(); got != 2 {
		t.Errorf("skewed FetchEgress took %d round trips, want 2", got)
	}

	roundTrips.Store(0)
	if _, err := skewedCP.FetchRevocations(ctx, enrollResp.BiscuitToken); err != nil {
		t.Fatalf("skewed FetchRevocations failed: %v", err)
	}
	if got := roundTrips.Load(); got != 2 {
		t.Errorf("skewed FetchRevocations took %d round trips, want 2", got)
	}

	roundTrips.Store(0)
	if err := skewedCP.ReportCatalog(ctx, enrollResp.BiscuitToken, nil); err != nil {
		t.Fatalf("skewed ReportCatalog failed: %v", err)
	}
	if got := roundTrips.Load(); got != 2 {
		t.Errorf("skewed ReportCatalog took %d round trips, want 2", got)
	}

	// Other 401s (e.g., invalid biscuit) are not retried.
	roundTrips.Store(0)
	if _, err := skewedCP.FetchPolicy(ctx, []byte("not-a-valid-biscuit")); err == nil {
		t.Fatal("expected error for invalid biscuit, got nil")
	}
	if got := roundTrips.Load(); got != 1 {
		t.Errorf("non-stale 401 took %d round trips, want 1 (no retry)", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
