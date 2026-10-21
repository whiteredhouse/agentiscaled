// agentiscale fork patch (see AGENTISCALE.md).

package controlplane

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

func TestBootstrapTokenBoundToOnePeer(t *testing.T) {
	issuer, _ := startCustomMockOIDC(t)
	srv, store, baseURL := setupTestServer(t, issuer)
	defer func() {
		_ = srv.Close()
		_ = store.Close()
	}()
	srv.config.AdminToken = "super-secret-admin-token"
	srv.config.AutoApproveEnrollment = true
	client := &http.Client{Timeout: 5 * time.Second}

	createToken := func(t *testing.T, description string, maxUsages int) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"role": api.RoleNode, "ttl_hours": 1, "max_usages": maxUsages, "description": description})
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/admin/bootstrap-tokens", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer super-secret-admin-token")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var created struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&created)
		return resp.StatusCode, created.Token
	}
	enroll := func(t *testing.T, token string, priv crypto.PrivKey, peerID string) api.EnrollmentStatus {
		t.Helper()
		decoded, err := peer.Decode(peerID)
		if err != nil {
			t.Fatal(err)
		}
		pub, _ := crypto.MarshalPublicKey(priv.GetPublic())
		ts, sig := enrollPoP(t, priv, decoded.String())
		data, _ := proto.Marshal(&api.BootstrapEnrollRequest{
			BootstrapToken:     token,
			PeerId:             peerID,
			PublicKey:          pub,
			RequestedRole:      api.RoleNode,
			ChallengeUnixMs:    ts,
			ChallengeSignature: sig,
		})
		resp, err := client.Post(baseURL+"/enroll", "application/x-protobuf", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		var out api.BootstrapEnrollResponse
		if err := proto.Unmarshal(body, &out); err != nil {
			t.Fatalf("enroll: %s: %s", resp.Status, body)
		}
		return out.Status
	}

	privA, idA := newTestKey(t)
	privB, idB := newTestKey(t)

	// Two usages, so only the binding can refuse the second key.
	status, token := createToken(t, boundPeerPrefix+cidAlias(t, idA)+" join", 2)
	if status != http.StatusCreated {
		t.Fatalf("create bound token: %d", status)
	}
	if got := enroll(t, token, privB, idB.String()); got != api.EnrollmentStatus_ENROLLMENT_STATUS_REJECTED {
		t.Fatalf("another peer enrolling with a bound token: %v, want rejected", got)
	}
	if _, err := store.GetNode(t.Context(), idB.String()); err == nil {
		t.Fatal("the other peer was enrolled")
	}
	// The binding names the peer in either spelling.
	if got := enroll(t, token, privA, idA.String()); got != api.EnrollmentStatus_ENROLLMENT_STATUS_APPROVED {
		t.Fatalf("the bound peer: %v, want approved", got)
	}
	if got := enroll(t, token, privA, cidAlias(t, idA)); got != api.EnrollmentStatus_ENROLLMENT_STATUS_APPROVED {
		t.Fatalf("the bound peer re-enrolling under its CID spelling: %v, want approved", got)
	}

	if status, _ := createToken(t, boundPeerPrefix+"not-a-peer", 1); status != http.StatusBadRequest {
		t.Fatalf("token bound to an invalid peer ID: %d, want 400", status)
	}
	// An unbound token still admits any key.
	_, open := createToken(t, "plain", 1)
	if got := enroll(t, open, privB, idB.String()); got != api.EnrollmentStatus_ENROLLMENT_STATUS_APPROVED {
		t.Fatalf("unbound token: %v, want approved", got)
	}
}
