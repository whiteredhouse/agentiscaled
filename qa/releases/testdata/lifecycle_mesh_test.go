package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/export/standalone"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

const protocolJoinCode = "protocol-JOIN-SENTINEL"
const protocolAdminToken = "protocol-ADMIN-SENTINEL"

func protocolFixture(t *testing.T) (*standalone.Server, *httptest.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	mesh, err := standalone.New(standalone.Options{BindAddress: "127.0.0.1:0", DataDir: t.TempDir(),
		DisableJoinToken: true, AdminToken: protocolAdminToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := mesh.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mesh.Close() })
	control := newFixtureControl(mesh, protocolAdminToken, protocolJoinCode, t.TempDir(), t.TempDir())
	server := httptest.NewServer(control.handler())
	t.Cleanup(server.Close)
	return mesh, server
}

func protocolKey(t *testing.T) (crypto.PrivKey, string) {
	t.Helper()
	key, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}
	id, err := peer.IDFromPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, id.String()
}

func protocolRequest(t *testing.T, base, method, path, authorization string, body any) (int, []byte) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, base+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	output, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, output
}

func protocolEvidence(t *testing.T, base string) (map[string]any, []byte) {
	t.Helper()
	status, output := protocolRequest(t, base, http.MethodGet, "/evidence", "", nil)
	if status != http.StatusOK {
		t.Fatalf("evidence: HTTP %d", status)
	}
	var evidence map[string]any
	if err := json.Unmarshal(output, &evidence); err != nil {
		t.Fatal(err)
	}
	return evidence, output
}

func protocolEnroll(t *testing.T, meshURL, token string, key crypto.PrivKey, id string) api.EnrollmentStatus {
	t.Helper()
	publicKey, err := crypto.MarshalPublicKey(key.GetPublic())
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Now().UnixMilli()
	signature, err := key.Sign(api.EnrollChallenge(id, timestamp))
	if err != nil {
		t.Fatal(err)
	}
	body, err := proto.Marshal(&api.BootstrapEnrollRequest{BootstrapToken: token, PeerId: id,
		PublicKey: publicKey, RequestedRole: api.RoleNode, ChallengeUnixMs: timestamp, ChallengeSignature: signature})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(meshURL+"/enroll", "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	output, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("mesh bootstrap: HTTP %d", response.StatusCode)
	}
	var enrollment api.BootstrapEnrollResponse
	if err := proto.Unmarshal(output, &enrollment); err != nil {
		t.Fatal(err)
	}
	return enrollment.Status
}

func TestJoinCodeProtocolAndBoundBootstrap(t *testing.T) {
	mesh, control := protocolFixture(t)
	key, id := protocolKey(t)
	otherKey, otherID := protocolKey(t)
	valid := map[string]string{"code": protocolJoinCode, "peer_id": id, "machine_name": "qa-headless"}
	for _, invalid := range []struct {
		name, method, path, authorization string
		body                              map[string]string
	}{
		{"wrong code", http.MethodPost, "/v1/join", "", map[string]string{"code": "wrong", "peer_id": id, "machine_name": "qa-headless"}},
		{"wrong machine", http.MethodPost, "/v1/join", "", map[string]string{"code": protocolJoinCode, "peer_id": id, "machine_name": "another-machine"}},
		{"invalid peer", http.MethodPost, "/v1/join", "", map[string]string{"code": protocolJoinCode, "peer_id": "invalid", "machine_name": "qa-headless"}},
		{"extra field", http.MethodPost, "/v1/join", "", map[string]string{"code": protocolJoinCode, "peer_id": id, "machine_name": "qa-headless", "extra": "forbidden"}},
		{"authorization", http.MethodPost, "/v1/join", "Bearer unexpected", valid},
		{"query", http.MethodPost, "/v1/join?path=ignored", "", valid},
		{"method", http.MethodGet, "/v1/join", "", valid},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			status, output := protocolRequest(t, control.URL, invalid.method, invalid.path, invalid.authorization, invalid.body)
			if status < 400 || status >= 500 {
				t.Fatalf("invalid join request: HTTP %d, want refusal", status)
			}
			if bytes.Contains(output, []byte(protocolJoinCode)) {
				t.Error("join refusal echoed the code")
			}
			evidence, _ := protocolEvidence(t, control.URL)
			if peers, ok := evidence["join_code_peers"].([]any); !ok || len(peers) != 0 {
				t.Fatal("invalid request was recorded as an accepted join")
			}
		})
	}
	status, output := protocolRequest(t, control.URL, http.MethodPost, "/v1/join", "", valid)
	if status != http.StatusOK {
		t.Fatalf("valid join after refusals: HTTP %d", status)
	}
	var answer struct {
		NetworkID      string `json:"network_id"`
		MeshURL        string `json:"mesh_url"`
		BootstrapToken string `json:"bootstrap_token"`
	}
	if err := json.Unmarshal(output, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.NetworkID == "" || answer.MeshURL != mesh.PublicURL() || answer.BootstrapToken == "" {
		t.Fatal("join did not return a complete real mesh bootstrap answer")
	}
	if got := protocolEnroll(t, answer.MeshURL, answer.BootstrapToken, otherKey, otherID); got != api.EnrollmentStatus_ENROLLMENT_STATUS_REJECTED {
		t.Fatalf("token admitted another key before its intended peer: %v", got)
	}
	if got := protocolEnroll(t, answer.MeshURL, answer.BootstrapToken, key, id); got != api.EnrollmentStatus_ENROLLMENT_STATUS_APPROVED {
		t.Fatalf("bound peer was not admitted after another key's refusal: %v", got)
	}
	if got := protocolEnroll(t, answer.MeshURL, answer.BootstrapToken, key, id); got != api.EnrollmentStatus_ENROLLMENT_STATUS_REJECTED {
		t.Fatalf("exhausted single-use token was accepted again: %v", got)
	}
	status, _ = protocolRequest(t, control.URL, http.MethodPost, "/v1/join", "", valid)
	if status != http.StatusForbidden {
		t.Fatalf("repeated code: HTTP %d", status)
	}
	evidence, encoded := protocolEvidence(t, control.URL)
	if !reflect.DeepEqual(evidence["join_code_peers"], []any{id}) {
		t.Fatal("accepted join peer evidence differs from the real enrolled peer")
	}
	if !reflect.DeepEqual(evidence["join_requests"], []any{map[string]any{"peer_id": id, "machine_name": "qa-headless"}}) {
		t.Fatal("accepted join evidence lost or added request fields")
	}
	for _, secret := range []string{protocolJoinCode, protocolAdminToken, answer.BootstrapToken} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Error("evidence contains a fixture secret")
		}
	}
}

func TestEnrollmentLocksKeepDiagnosticsAndJoinStageAvailable(t *testing.T) {
	_, control := protocolFixture(t)
	_, primaryID := protocolKey(t)
	for _, path := range []string{"/oauth2/device/auth", "/oauth2/token"} {
		if status, _ := protocolRequest(t, control.URL, http.MethodPost, path, "", nil); status != http.StatusOK {
			t.Fatalf("initial OAuth fixture %s: HTTP %d", path, status)
		}
	}
	if status, _ := protocolRequest(t, control.URL, http.MethodPost, "/v1/enroll", "Bearer ci-access", map[string]string{"peer_id": primaryID, "machine_name": "qa"}); status != http.StatusOK {
		t.Fatalf("primary enrollment: HTTP %d", status)
	}
	if status, _ := protocolRequest(t, control.URL, http.MethodPost, "/lock-signin", "", nil); status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("lock sign-in: HTTP %d", status)
	}
	for _, path := range []string{"/oauth2/device/auth", "/oauth2/token", "/v1/enroll"} {
		if status, _ := protocolRequest(t, control.URL, http.MethodPost, path, "Bearer ci-access", map[string]string{"peer_id": primaryID}); status != http.StatusForbidden {
			t.Fatalf("locked sign-in endpoint %s: HTTP %d", path, status)
		}
	}
	_, secondaryID := protocolKey(t)
	join := map[string]string{"code": protocolJoinCode, "peer_id": secondaryID, "machine_name": "qa-headless"}
	if status, _ := protocolRequest(t, control.URL, http.MethodPost, "/v1/join", "", join); status != http.StatusOK {
		t.Fatalf("sign-in lock also prevented headless enrollment: HTTP %d", status)
	}
	if status, _ := protocolRequest(t, control.URL, http.MethodPost, "/lock-enrollment", "", nil); status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("lock all enrollment: HTTP %d", status)
	}
	if status, _ := protocolRequest(t, control.URL, http.MethodPost, "/v1/join", "", join); status != http.StatusForbidden {
		t.Fatalf("locked join endpoint: HTTP %d", status)
	}
	evidence, _ := protocolEvidence(t, control.URL)
	if !reflect.DeepEqual(evidence["enrollments"], []any{primaryID}) || !reflect.DeepEqual(evidence["join_code_peers"], []any{secondaryID}) {
		t.Fatal("locks changed accepted primary/secondary enrollment evidence")
	}
	for key, expected := range map[string]any{"oauth_device_requests": float64(2), "oauth_token_requests": float64(2),
		"signin_enrollment_requests": float64(2), "denied_auth_requests": float64(3), "denied_join_requests": float64(1),
		"signin_locked": true, "enrollment_locked": true} {
		if evidence[key] != expected {
			t.Errorf("evidence %s = %v, want %v", key, evidence[key], expected)
		}
	}
	if status, _ := protocolRequest(t, control.URL, http.MethodGet, "/inspect-secondary?path=/unconfigured", "", nil); status < 400 || status >= 500 {
		t.Fatalf("inspection accepted a caller-selected path: HTTP %d", status)
	}
}
