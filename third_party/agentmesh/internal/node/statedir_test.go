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
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p/core/crypto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestStateDirRoundTrip(t *testing.T) {
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}
	privKey, _ := crypto.MarshalPrivateKey(priv)
	want := &api.MemberCredential{
		ControlPlaneUrl: "https://cp.example",
		Biscuit:         []byte("biscuit"),
		ExpireTime:      timestamppb.New(time.Unix(1790000000, 0)),
		TrustedKeys:     []*api.TrustedSigningKey{{PublicKey: bytes.Repeat([]byte{3}, ed25519.PublicKeySize)}},
		IssuedUnderKeys: [][]byte{bytes.Repeat([]byte{3}, ed25519.PublicKeySize)},
		RouterAddresses: []string{"/dns4/router.example/tcp/4001/p2p/12D3KooWP8iKhDf3iCMo2H3butNVfdTUtYwYWYQ75jTGnynXPFMp"},
		OidcSession:     &api.OIDCSession{Issuer: "https://issuer.example", RefreshToken: "secret"},
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := WriteStateDir(dir, privKey, want); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{StateDirIdentityFile, StateDirCredentialFile} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o, want 0600", f, info.Mode().Perm())
		}
	}

	// The file is protojson with proto field names and RFC 3339 instants,
	// what the SDKs read and write.
	raw, _ := os.ReadFile(filepath.Join(dir, StateDirCredentialFile))
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"control_plane_url", "biscuit", "expire_time", "trusted_keys", "issued_under_keys", "router_addresses", "oidc_session"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("credential.json lacks %q: %s", k, raw)
		}
	}
	if exp, _ := fields["expire_time"].(string); !strings.HasSuffix(exp, "Z") {
		t.Errorf("expire_time = %v, want RFC 3339", fields["expire_time"])
	}

	gotKey, got, err := ReadStateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotKey, privKey) || !proto.Equal(got, want) {
		t.Fatalf("round trip differs:\n got %v\nwant %v", got, want)
	}
}

func TestStateDirRejectsUnknownFieldsAndForeignKeys(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := ReadStateDir(dir); err == nil || !strings.Contains(err.Error(), "no identity") {
		t.Fatalf("empty dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, StateDirIdentityFile), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadStateDir(dir); err == nil || !strings.Contains(err.Error(), "not a libp2p private key") {
		t.Fatalf("garbage key: %v", err)
	}
	priv, _, _ := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	privKey, _ := crypto.MarshalPrivateKey(priv)
	if err := os.WriteFile(filepath.Join(dir, StateDirIdentityFile), privKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadStateDir(dir); err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("missing credential: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, StateDirCredentialFile), []byte(`{"control_plane_url": "https://cp.example", "surprise": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadStateDir(dir); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("unknown field: %v", err)
	}
	if err := WriteStateDir(dir, []byte("not a key"), &api.MemberCredential{}); err == nil {
		t.Fatal("WriteStateDir accepted a private key that is not a libp2p key")
	}
}
