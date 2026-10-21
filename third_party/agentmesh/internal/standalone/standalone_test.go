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

package standalone

import (
	"crypto/tls"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

func TestWsListenMultiaddr(t *testing.T) {
	cases := []struct {
		bind    string
		want    string
		wantErr bool
	}{
		{bind: "0.0.0.0:8080", want: "/ip4/0.0.0.0/tcp/8080/ws"},
		{bind: "127.0.0.1:0", want: "/ip4/127.0.0.1/tcp/0/ws"},
		{bind: ":9090", want: "/ip4/0.0.0.0/tcp/9090/ws"},
		{bind: "[::1]:8080", want: "/ip6/::1/tcp/8080/ws"},
		{bind: "example.com:8080", wantErr: true},
		{bind: "8080", wantErr: true},
	}
	for _, tc := range cases {
		got, err := wsListenMultiaddr(tc.bind)
		if tc.wantErr {
			if err == nil {
				t.Errorf("wsListenMultiaddr(%q) = %q, want error", tc.bind, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("wsListenMultiaddr(%q) failed: %v", tc.bind, err)
			continue
		}
		if got != tc.want {
			t.Errorf("wsListenMultiaddr(%q) = %q, want %q", tc.bind, got, tc.want)
		}
	}
}

func TestExternalMultiaddr(t *testing.T) {
	cases := []struct {
		url     string
		want    string
		wantErr bool
	}{
		{url: "https://my-agentmesh.a.run.app", want: "/dns4/my-agentmesh.a.run.app/tcp/443/wss"},
		{url: "http://192-168-1-50.nip.io:8080", want: "/dns4/192-168-1-50.nip.io/tcp/8080/ws"},
		{url: "http://192.168.1.50:8080", want: "/ip4/192.168.1.50/tcp/8080/ws"},
		{url: "https://mesh.example:8443", want: "/dns4/mesh.example/tcp/8443/wss"},
		{url: "ftp://mesh.example", wantErr: true},
		{url: "http://", wantErr: true},
	}
	for _, tc := range cases {
		got, err := externalMultiaddr(tc.url)
		if tc.wantErr {
			if err == nil {
				t.Errorf("externalMultiaddr(%q) = %q, want error", tc.url, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("externalMultiaddr(%q) failed: %v", tc.url, err)
			continue
		}
		if got != tc.want {
			t.Errorf("externalMultiaddr(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestEnsureRouterKeyDeterministic(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	key1 := filepath.Join(dir1, routerKeyFile)
	key2 := filepath.Join(dir2, routerKeyFile)

	if err := ensureRouterKey(key1, "mesh_adm_fixed_secret"); err != nil {
		t.Fatalf("ensureRouterKey(key1): %v", err)
	}
	if err := ensureRouterKey(key2, "mesh_adm_fixed_secret"); err != nil {
		t.Fatalf("ensureRouterKey(key2): %v", err)
	}

	b1, err := os.ReadFile(key1)
	if err != nil {
		t.Fatalf("ReadFile(key1): %v", err)
	}
	b2, err := os.ReadFile(key2)
	if err != nil {
		t.Fatalf("ReadFile(key2): %v", err)
	}
	p1, err := crypto.UnmarshalPrivateKey(b1)
	if err != nil {
		t.Fatalf("UnmarshalPrivateKey(b1): %v", err)
	}
	p2, err := crypto.UnmarshalPrivateKey(b2)
	if err != nil {
		t.Fatalf("UnmarshalPrivateKey(b2): %v", err)
	}
	id1, _ := peer.IDFromPrivateKey(p1)
	id2, _ := peer.IDFromPrivateKey(p2)
	if id1 != id2 {
		t.Fatalf("peer IDs differ for same adminToken: %s vs %s", id1, id2)
	}

	// Existing key file must not be overwritten by a different admin token.
	if err := ensureRouterKey(key1, "mesh_adm_other_secret"); err != nil {
		t.Fatalf("ensureRouterKey existing: %v", err)
	}
	b1After, err := os.ReadFile(key1)
	if err != nil {
		t.Fatalf("ReadFile(key1 after): %v", err)
	}
	if string(b1) != string(b1After) {
		t.Fatalf("ensureRouterKey overwrote an existing router.key")
	}
}

func TestInferredRequestMultiaddr(t *testing.T) {
	const testPeerID = "12D3KooWBzUDQCkZhz2rWrYBhpjcCH8VnrRNcwCW6DoF36iADYrY"

	reqCloudRun, _ := http.NewRequest(http.MethodGet, "http://0.0.0.0:8080/info", nil)
	reqCloudRun.Host = "agentmesh-one-xyz-uc.a.run.app"
	reqCloudRun.Header.Set("X-Forwarded-Proto", "https")
	got, ok := inferredRequestMultiaddr(reqCloudRun, testPeerID)
	want := "/dns4/agentmesh-one-xyz-uc.a.run.app/tcp/443/wss/p2p/" + testPeerID
	if !ok || got != want {
		t.Fatalf("CloudRun inferredRequestMultiaddr = (%q, %v), want (%q, true)", got, ok, want)
	}

	reqTLS, _ := http.NewRequest(http.MethodGet, "https://mesh.example.com:8443/info", nil)
	reqTLS.Host = "mesh.example.com:8443"
	reqTLS.TLS = &tls.ConnectionState{}
	got, ok = inferredRequestMultiaddr(reqTLS, testPeerID)
	want = "/dns4/mesh.example.com/tcp/8443/wss/p2p/" + testPeerID
	if !ok || got != want {
		t.Fatalf("TLS inferredRequestMultiaddr = (%q, %v), want (%q, true)", got, ok, want)
	}

	reqLoopback, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8080/info", nil)
	reqLoopback.Host = "127.0.0.1:8080"
	if got, ok := inferredRequestMultiaddr(reqLoopback, testPeerID); ok {
		t.Fatalf("Loopback HTTP should not infer external multiaddr, got %q", got)
	}
}

func TestPrependInferredRouterAddr(t *testing.T) {
	const inferred = "/dns4/agentmesh-one-xyz.a.run.app/tcp/443/wss/p2p/12D3KooWBzUDQCkZhz2rWrYBhpjcCH8VnrRNcwCW6DoF36iADYrY"
	const local = "/ip4/127.0.0.1/tcp/8080/ws/p2p/12D3KooWBzUDQCkZhz2rWrYBhpjcCH8VnrRNcwCW6DoF36iADYrY"

	enrollIn, err := proto.Marshal(&api.BootstrapEnrollResponse{
		RouterAddresses: []string{local},
	})
	if err != nil {
		t.Fatalf("Marshal BootstrapEnrollResponse: %v", err)
	}
	enrollOutBytes, ok := prependInferredRouterAddr("/enroll", enrollIn, inferred)
	if !ok {
		t.Fatal("prependInferredRouterAddr(/enroll) returned false")
	}
	var enrollOut api.BootstrapEnrollResponse
	if err := proto.Unmarshal(enrollOutBytes, &enrollOut); err != nil {
		t.Fatalf("Unmarshal BootstrapEnrollResponse: %v", err)
	}
	if len(enrollOut.RouterAddresses) != 2 || enrollOut.RouterAddresses[0] != inferred || enrollOut.RouterAddresses[1] != local {
		t.Fatalf("BootstrapEnrollResponse.RouterAddresses = %v, want [%s %s]", enrollOut.RouterAddresses, inferred, local)
	}

	regIn, err := proto.Marshal(&api.EnrollResponse{
		BiscuitToken:    []byte("tok"),
		RouterAddresses: []string{local},
	})
	if err != nil {
		t.Fatalf("Marshal EnrollResponse: %v", err)
	}
	regOutBytes, ok := prependInferredRouterAddr("/register", regIn, inferred)
	if !ok {
		t.Fatal("prependInferredRouterAddr(/register) returned false")
	}
	var regOut api.EnrollResponse
	if err := proto.Unmarshal(regOutBytes, &regOut); err != nil {
		t.Fatalf("Unmarshal EnrollResponse: %v", err)
	}
	if len(regOut.RouterAddresses) != 2 || regOut.RouterAddresses[0] != inferred || regOut.RouterAddresses[1] != local {
		t.Fatalf("EnrollResponse.RouterAddresses = %v, want [%s %s]", regOut.RouterAddresses, inferred, local)
	}

	// /info carries the per-router list a member prefers; the entry for this
	// router gets the inferred address, another router's entry is untouched.
	const otherPeer = "12D3KooWDk5pWkyxVERi6kt3TyRZCaXWqg441QEQxaGkn5rWHB21"
	const otherAddr = "/dns4/router-b.example.com/tcp/443/wss/p2p/" + otherPeer
	infoIn, err := proto.Marshal(&api.ControlPlaneInfoResponse{
		RouterAddresses: []string{local},
		Routers: []*api.RouterInfo{
			{PeerId: "12D3KooWBzUDQCkZhz2rWrYBhpjcCH8VnrRNcwCW6DoF36iADYrY", Addresses: []string{local}},
			{PeerId: otherPeer, Addresses: []string{otherAddr}},
			{PeerId: "not-a-peer-id", Addresses: []string{"/ip4/10.0.0.1/tcp/1/ws"}},
		},
	})
	if err != nil {
		t.Fatalf("Marshal ControlPlaneInfoResponse: %v", err)
	}
	infoOutBytes, ok := prependInferredRouterAddr("/info", infoIn, inferred)
	if !ok {
		t.Fatal("prependInferredRouterAddr(/info) returned false")
	}
	var infoOut api.ControlPlaneInfoResponse
	if err := proto.Unmarshal(infoOutBytes, &infoOut); err != nil {
		t.Fatalf("Unmarshal ControlPlaneInfoResponse: %v", err)
	}
	if len(infoOut.RouterAddresses) != 2 || infoOut.RouterAddresses[0] != inferred || infoOut.RouterAddresses[1] != local {
		t.Fatalf("ControlPlaneInfoResponse.RouterAddresses = %v, want [%s %s]", infoOut.RouterAddresses, inferred, local)
	}
	if len(infoOut.Routers) != 3 {
		t.Fatalf("ControlPlaneInfoResponse.Routers has %d entries, want 3", len(infoOut.Routers))
	}
	if got := infoOut.Routers[0].Addresses; len(got) != 2 || got[0] != inferred || got[1] != local {
		t.Fatalf("Routers[0].Addresses = %v, want [%s %s]", got, inferred, local)
	}
	if got := infoOut.Routers[1].Addresses; len(got) != 1 || got[0] != otherAddr {
		t.Fatalf("Routers[1].Addresses = %v, want [%s] (another router's entry must not change)", got, otherAddr)
	}
	if got := infoOut.Routers[2].Addresses; len(got) != 1 || got[0] != "/ip4/10.0.0.1/tcp/1/ws" {
		t.Fatalf("Routers[2].Addresses = %v, want the entry with an undecodable peer id untouched", got)
	}

	if returnsRouterAddresses("/refresh") {
		t.Fatal("returnsRouterAddresses(/refresh) = true, want false")
	}
	if !returnsRouterAddresses("/register") {
		t.Fatal("returnsRouterAddresses(/register) = false, want true")
	}
}

func TestResponseRecorder(t *testing.T) {
	rec := newResponseRecorder()
	rec.Header().Set("Content-Length", "12")
	rec.WriteHeader(http.StatusCreated)
	if _, err := rec.Write([]byte("hello world!")); err != nil {
		t.Fatalf("rec.Write: %v", err)
	}
	if rec.code != http.StatusCreated {
		t.Fatalf("rec.code = %d, want %d", rec.code, http.StatusCreated)
	}
	if got := rec.body.String(); got != "hello world!" {
		t.Fatalf("rec.body = %q, want %q", got, "hello world!")
	}
	if got := rec.Header().Get("Content-Length"); got != "12" {
		t.Fatalf("rec.Header(Content-Length) = %q, want 12", got)
	}
}
