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
	"context"
	"crypto/ed25519"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse-biscuit/biscuit-go/v2"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/proto"

	"github.com/google/agentmesh/api"
)

// A router that refuses the first handshakes, as one does when a node shows a
// credential under a key the router has not synced yet, then accepts.
func startRefusingRouter(t *testing.T, cpPriv ed25519.PrivateKey, refusals int32) (host.Host, *atomic.Int32) {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	builder := biscuit.NewBuilder(cpPriv)
	for _, f := range []biscuit.Fact{
		{Predicate: biscuit.Predicate{Name: api.FactNode, IDs: []biscuit.Term{biscuit.String(h.ID().String())}}},
		{Predicate: biscuit.Predicate{Name: api.FactRole, IDs: []biscuit.Term{biscuit.String(api.RoleRouter)}}},
		{Predicate: biscuit.Predicate{Name: api.FactExpiration, IDs: []biscuit.Term{biscuit.Date(time.Now().Add(24 * time.Hour))}}},
		api.MarkerFact(api.FactTargetUnrestricted),
	} {
		if err := builder.AddAuthorityFact(f); err != nil {
			t.Fatal(err)
		}
	}
	tok, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	routerBiscuit, err := tok.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	var handshakes atomic.Int32
	h.SetStreamHandler(api.AuthProtocolID, func(s network.Stream) {
		defer func() { _ = s.Close() }()
		reader := msgio.NewVarintReaderSize(s, 1024*64)
		msg, err := reader.ReadMsg()
		if err != nil {
			return
		}
		reader.ReleaseMsg(msg)
		resp := &api.AuthResponse{Success: true, Biscuit: routerBiscuit}
		if handshakes.Add(1) <= refusals {
			resp = &api.AuthResponse{Success: false, Error: "no valid key found for verification"}
		}
		data, err := proto.Marshal(resp)
		if err != nil {
			return
		}
		_ = msgio.NewVarintWriter(s).WriteMsg(data)
	})
	return h, &handshakes
}

// newReadmitTestNode is a node holding an authenticated session with router,
// connected to it, with a refreshed credential to show.
func newReadmitTestNode(t *testing.T, cpPub ed25519.PublicKey, router host.Host) *AgentMeshNode {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if err := h.Connect(t.Context(), peer.AddrInfo{ID: router.ID(), Addrs: router.Addrs()}); err != nil {
		t.Fatal(err)
	}
	n := &AgentMeshNode{
		Host:                 h,
		BiscuitTimeout:       2 * time.Second,
		authenticatedRouters: map[peer.ID]bool{router.ID(): true},
		trustedKeys:          []TrustedKey{{Key: cpPub, ReceivedAt: time.Now()}},
	}
	n.config.RouterConnectTimeout = 2 * time.Second
	n.cachedIdentity.Store([]byte("refreshed-credential"))
	return n
}

// TestReadmitRetriesARouterThatHasNotSyncedTheNewKey pins the window after a
// key rotation: the node refreshes under the new key and shows it to its
// routers at once; a router that has not synced the key yet refuses. The
// node asks again within seconds and keeps the session, instead of dropping
// the router and serving 503 until the connection monitor's next minute.
func TestReadmitRetriesARouterThatHasNotSyncedTheNewKey(t *testing.T) {
	cpPub, cpPriv, _ := ed25519.GenerateKey(nil)
	router, handshakes := startRefusingRouter(t, cpPriv, 2)
	n := newReadmitTestNode(t, cpPub, router)

	start := time.Now()
	n.readmitRouters(t.Context())
	if got := handshakes.Load(); got != 3 {
		t.Fatalf("router saw %d handshakes, want 3: two refusals and the one that succeeded", got)
	}
	if !n.IsConnected() {
		t.Fatal("the router accepted the third handshake, but the node dropped it from the authenticated set")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("re-admission took %s; the retries should fit in a few seconds", elapsed)
	}
}

// A router that keeps refusing is dropped after the retries, so the monitor
// dials and handshakes afresh, as before.
func TestReadmitDropsARouterThatKeepsRefusing(t *testing.T) {
	cpPub, cpPriv, _ := ed25519.GenerateKey(nil)
	router, handshakes := startRefusingRouter(t, cpPriv, 100)
	n := newReadmitTestNode(t, cpPub, router)

	n.readmitRouters(context.Background())
	if got, want := handshakes.Load(), int32(len(readmitRetryDelays)+1); got != want {
		t.Fatalf("router saw %d handshakes, want %d", got, want)
	}
	if n.IsConnected() {
		t.Fatal("a router that refused every handshake is still in the authenticated set")
	}
}
