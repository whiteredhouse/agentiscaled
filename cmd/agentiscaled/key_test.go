package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// TestJoiningSendsOnlyThePublicKey follows a first start with a join code:
// the node makes its key itself, the control
// service gets only the peer ID, and the mesh gets the public key and a
// signature, never the private key.
func TestJoiningSendsOnlyThePublicKey(t *testing.T) {
	var mu sync.Mutex
	// Every request is kept whole: method, URL, headers and body.
	var sent [][]byte
	record := func(r *http.Request) []byte {
		dump, err := httputil.DumpRequest(r, true)
		if err != nil {
			t.Errorf("dump request: %v", err)
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent = append(sent, dump)
		mu.Unlock()
		return b
	}

	var enrolls []*api.BootstrapEnrollRequest
	mesh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := record(r)
		if r.URL.Path == "/enroll" {
			var req api.BootstrapEnrollRequest
			if err := proto.Unmarshal(b, &req); err != nil {
				t.Errorf("enroll body: %v", err)
			}
			mu.Lock()
			enrolls = append(enrolls, &req)
			mu.Unlock()
		}
		http.Error(w, "stop here", http.StatusForbidden)
	}))
	defer mesh.Close()

	var joins []map[string]any
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := record(r)
		if r.URL.Path != "/v1/join" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("join body: %v", err)
		}
		mu.Lock()
		joins = append(joins, body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(joinAnswer{NetworkID: "n1", MeshURL: mesh.URL, BootstrapToken: "bootstrap"})
	}))
	defer control.Close()

	dir := t.TempDir()
	codePath := filepath.Join(dir, "code")
	if err := os.WriteFile(codePath, []byte("JOIN-CODE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := run(ctx, config{
		control:      control.URL,
		joinCodePath: codePath,
		dataDir:      filepath.Join(dir, "node"),
		listen:       "/ip4/127.0.0.1/tcp/0",
		logLevel:     "error",
	})
	if err == nil || !strings.Contains(err.Error(), "enroll") {
		t.Fatalf("run = %v, want the refused enrollment", err)
	}

	store, err := meshnode.NewStore(filepath.Join(dir, "node"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := store.LoadKey()
	store.Close()
	if len(raw) == 0 {
		t.Fatal("the node stored no key")
	}
	priv, err := crypto.UnmarshalPrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if priv.Type() != crypto.Ed25519 {
		t.Errorf("key type = %v, want Ed25519", priv.Type())
	}
	privRaw, _ := priv.Raw()
	pubBytes, _ := crypto.MarshalPublicKey(priv.GetPublic())
	peerID, _ := peer.IDFromPrivateKey(priv)

	// The key stays in the node's data directory, readable by its owner only.
	entries, err := os.ReadDir(filepath.Join(dir, "node"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("data directory: %v, %d entries", err, len(entries))
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s mode = %v, want owner-only", e.Name(), fi.Mode().Perm())
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(joins) != 1 {
		t.Fatalf("joins = %d, want 1", len(joins))
	}
	if len(joins[0]) != 2 || joins[0]["code"] != "JOIN-CODE" || joins[0]["peer_id"] != peerID.String() {
		t.Errorf("join body = %v, want only the code and the peer ID %s", joins[0], peerID)
	}
	if len(enrolls) == 0 {
		t.Fatal("the node never asked the mesh to enroll")
	}
	for _, e := range enrolls {
		if e.PeerId != peerID.String() || !bytes.Equal(e.PublicKey, pubBytes) {
			t.Errorf("enroll sent peer %s and key %x, want %s and the node's public key", e.PeerId, e.PublicKey, peerID)
		}
		ok, err := priv.GetPublic().Verify(api.EnrollChallenge(peerID.String(), e.ChallengeUnixMs), e.ChallengeSignature)
		if err != nil || !ok {
			t.Errorf("enroll signature does not verify under the node's public key: %v", err)
		}
	}
	// Neither the private key nor its seed leaves the node in any encoding,
	// in the URL, a header or the body.
	secrets := [][]byte{raw, privRaw, privRaw[:32]}
	for _, s := range append([][]byte(nil), secrets...) {
		std := base64.StdEncoding.EncodeToString(s)
		secrets = append(secrets,
			[]byte(std),
			[]byte(url.QueryEscape(std)),
			[]byte(base64.RawStdEncoding.EncodeToString(s)),
			[]byte(base64.URLEncoding.EncodeToString(s)),
			[]byte(base64.RawURLEncoding.EncodeToString(s)),
			[]byte(hex.EncodeToString(s)))
	}
	if len(sent) < 2 {
		t.Fatalf("recorded %d requests, want the join and the enrollment", len(sent))
	}
	for i, req := range sent {
		for _, s := range secrets {
			if bytes.Contains(bytes.ToLower(req), bytes.ToLower(s)) {
				t.Errorf("request %d carries the node's private key", i)
			}
		}
	}
}
