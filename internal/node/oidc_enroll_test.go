package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	meshnode "github.com/google/agentmesh/export/node"
)

// A workflow's node sends its OIDC token, peer ID and network, and sends
// the same token again when the answer is lost or the network is starting.
func TestEnrollOIDCRetriesWithTheSameToken(t *testing.T) {
	old := enrollRetry
	enrollRetry = 10 * time.Millisecond
	defer func() { enrollRetry = old }()

	var got []map[string]string
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll/oidc" || r.Header.Get("Authorization") != "" {
			http.Error(w, `{"error":"wrong route"}`, http.StatusNotFound)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		switch len(got) {
		case 1:
			http.Error(w, `{"error":"network is not ready yet"}`, http.StatusServiceUnavailable)
		case 2:
			// The answer is lost: the connection closes without one.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		default:
			_ = json.NewEncoder(w).Encode(joinAnswer{NetworkID: "n1", MeshURL: "https://n1.mesh.test", BootstrapToken: "bt", ExpiresAt: "2026-11-15T12:00:00Z"})
		}
	}))
	defer control.Close()

	answer, err := enrollOIDC(context.Background(), control.URL+"/", "GH-TOKEN", "n1", "12D3KooWPeer", "runner")
	if err != nil || answer.BootstrapToken != "bt" || answer.ExpiresAt == "" {
		t.Fatalf("enrollOIDC = %+v, %v", answer, err)
	}
	if len(got) != 3 {
		t.Fatalf("%d requests, want 3", len(got))
	}
	for _, b := range got {
		if b["id_token"] != "GH-TOKEN" || b["peer_id"] != "12D3KooWPeer" || b["network_id"] != "n1" || b["machine_name"] != "runner" {
			t.Fatalf("request %v", b)
		}
	}
}

// A refusal is final: the token is not sent again.
func TestEnrollOIDCRefusalIsFinal(t *testing.T) {
	var calls int
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, `{"error":"workflow does not match the trust"}`, http.StatusForbidden)
	}))
	defer control.Close()
	_, err := enrollOIDC(context.Background(), control.URL, "GH-TOKEN", "n1", "p", "")
	if err == nil || !strings.Contains(err.Error(), "workflow does not match the trust") || calls != 1 {
		t.Fatalf("refused enroll: %v after %d calls", err, calls)
	}
}

// The token needs the network it joins, and cannot be combined with a code.
func TestOIDCEnrollmentFlags(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("GH-TOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := meshnode.NewStore(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	priv := meshnode.GetOrGenerateKey(store)
	for name, c := range map[string]config{
		"no network": {control: "https://control.test", oidcTokenPath: tokenFile},
		"with code":  {control: "https://control.test", oidcTokenPath: tokenFile, joinCodePath: tokenFile, networkID: "n1"},
	} {
		if _, err := enroll(context.Background(), c, store, priv, nil); err == nil {
			t.Errorf("%s: enrolled", name)
		}
	}
}
