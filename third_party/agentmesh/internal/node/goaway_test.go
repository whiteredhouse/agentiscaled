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
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"
	"github.com/multiformats/go-multiaddr"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/google/agentmesh/api"
)

// sendGoAway is a router's side of the protocol: one message, then wait
// for the node to close the stream.
func sendGoAway(t *testing.T, from host.Host, to peer.ID, msg *api.RouterGoAway) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := from.NewStream(ctx, to, api.GoAwayProtocolID)
	if err != nil {
		t.Fatalf("open go-away stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	data, _ := proto.Marshal(msg)
	if err := msgio.NewVarintWriter(s).WriteMsg(data); err != nil {
		t.Fatalf("write go-away: %v", err)
	}
	_ = s.CloseWrite()
	_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = s.Read(make([]byte, 1))
}

// A router's go-away moves the node: it lets that router go, attaches to
// another at once rather than on the monitor's next tick, stays off the
// sender for the time it named, and does not redial it when the connection
// then closes. A go-away from a peer that is not a router it holds changes
// nothing.
func TestGoAwayMovesTheNodeToAnotherRouter(t *testing.T) {
	cpPub, cpPriv, _ := ed25519.GenerateKey(nil)
	a := newFakeRouterWithKey(t, cpPub, cpPriv, "/ip4/127.0.0.1/tcp/0")
	b := newFakeRouterWithKey(t, cpPub, cpPriv, "/ip4/127.0.0.1/tcp/0")
	routers := map[peer.ID]*fakeRouter{a.h.ID(): a, b.h.ID(): b}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	addrs := append(append([]multiaddr.Multiaddr{}, a.routerAddrs...), b.routerAddrs...)
	node := a.startNode(t, ctx, addrs, func(o *Options) {
		o.Routers = 1
		o.MonitorBootstrap = time.Hour
		o.RouterRedialDelay = 50 * time.Millisecond
	})
	attached := node.attachedRouters()
	if len(attached) != 1 {
		t.Fatalf("attached to %d routers, want 1", len(attached))
	}
	first, other := routers[attached[0]], a
	if first == a {
		other = b
	}
	if got := other.handshakes.Load(); got != 0 {
		t.Fatalf("the other router saw %d handshakes before the go-away, want 0", got)
	}

	// A stranger's go-away is ignored.
	stranger, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stranger.Close() })
	if err := stranger.Connect(ctx, peer.AddrInfo{ID: node.Host.ID(), Addrs: node.Host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	sendGoAway(t, stranger, node.Host.ID(), &api.RouterGoAway{Reason: api.RouterGoAway_DRAINING})
	if got := node.attachedRouters(); len(got) != 1 || got[0] != first.h.ID() {
		t.Fatalf("after a stranger's go-away the node holds %v, want %s", got, first.h.ID())
	}

	sendGoAway(t, first.h, node.Host.ID(), &api.RouterGoAway{
		Reason:     api.RouterGoAway_OVERLOADED,
		RetryAfter: durationpb.New(time.Minute),
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !node.isAuthenticatedAndConnected(other.h.ID()) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := node.attachedRouters(); len(got) != 1 || got[0] != other.h.ID() {
		t.Fatalf("after the go-away the node holds %v, want the other router %s", got, other.h.ID())
	}
	if node.isAttached(first.h.ID()) {
		t.Fatal("the node still holds the router that sent it away")
	}
	node.mu.Lock()
	until, shunned := node.shunned[first.h.ID()]
	node.mu.Unlock()
	if !shunned || time.Until(until) < 50*time.Second {
		t.Fatalf("shunned until %v (%v), want about a minute from now", until, shunned)
	}

	// The sender closes, as it does after its grace: no redial.
	before := first.handshakes.Load()
	dropFromRouter(t, first.h, node)
	time.Sleep(300 * time.Millisecond)
	if got := first.handshakes.Load(); got != before {
		t.Fatalf("the node handshook the router that sent it away %d more times", got-before)
	}
}
