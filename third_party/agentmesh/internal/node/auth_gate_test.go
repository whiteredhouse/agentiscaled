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
	"errors"
	"io"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/proto"

	"github.com/google/agentmesh/api"
)

const echoProtocol protocol.ID = "/test/echo/1.0.0"

func echo(s network.Stream) {
	defer func() { _ = s.Close() }()
	_, _ = io.Copy(s, s)
}

// A stream the node opens to a listed router it has not authenticated with
// runs the handshake first, once, shared with the one the connection
// notifee starts; a stream to a peer the node cannot vouch for is refused
// at once, so a lookup is never held on it.
func TestNodeStreamsToARouterFollowItsHandshake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	r1 := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")
	r2 := newFakeRouterWithKey(t, r1.cpPub, r1.cpPriv, "/ip4/127.0.0.1/tcp/0")
	node := r1.startNode(t, ctx, append(r1.routerAddrs, r2.routerAddrs...), func(o *Options) {
		o.Routers = 1
		o.RouterRedialDelay = -1
	})
	attached, other := r1, r2
	if node.attachedRouters()[0] == r2.h.ID() {
		attached, other = r2, r1
	}
	other.h.SetStreamHandler(echoProtocol, echo)
	gated := &meshHost{Host: node.Host, n: node}

	s, err := gated.NewStream(ctx, other.h.ID(), echoProtocol)
	if err != nil {
		t.Fatalf("NewStream to a listed router: %v", err)
	}
	_ = s.Close()
	if !node.isAuthenticatedAndConnected(other.h.ID()) {
		t.Fatal("the stream opened before the handshake with the router")
	}
	if got := other.handshakes.Load(); got != 1 {
		t.Errorf("the router saw %d handshakes, want 1", got)
	}

	// An attached router whose session dropped is left to redialRouter and
	// the connection monitor; an in-flight stream on gated must not
	// re-handshake it on its own.
	dropFromRouter(t, attached.h, node)
	if _, err := gated.NewStream(ctx, attached.h.ID(), echoProtocol); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("NewStream to a dropped attached router: err = %v, want %v", err, errNotAuthenticated)
	}
	if got := attached.handshakes.Load(); got != 1 {
		t.Fatalf("dropped attached router saw %d handshakes, want 1", got)
	}

	stranger, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stranger.Close() })
	stranger.SetStreamHandler(echoProtocol, echo)
	if err := node.Host.Connect(ctx, peer.AddrInfo{ID: stranger.ID(), Addrs: stranger.Addrs()}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := gated.NewStream(ctx, stranger.ID(), echoProtocol); !errors.Is(err, errNotAuthenticated) {
		t.Fatalf("NewStream to a peer that never authenticated: err = %v, want %v", err, errNotAuthenticated)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the refusal took %v; a lookup must not wait on a peer the node cannot vouch for", took)
	}
}

// A handler the node registers through its gated host serves a peer only
// once that peer authenticated with the node; a stream that arrives first
// waits for the handshake and is then served.
func TestNodeServesOnlyAuthenticatedPeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	r := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")
	node := r.startNode(t, ctx, r.routerAddrs)
	gated := &meshHost{Host: node.Host, n: node}
	gated.SetStreamHandler(echoProtocol, echo)

	caller, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = caller.Close() })
	if err := caller.Connect(ctx, peer.AddrInfo{ID: node.Host.ID(), Addrs: node.Host.Addrs()}); err != nil {
		t.Fatal(err)
	}

	s, err := caller.NewStream(ctx, node.Host.ID(), echoProtocol)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	_ = s.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := s.Read(buf); err == nil {
		t.Fatalf("read %q before the handshake; the stream must wait for it", buf[:n])
	}

	// The caller authenticates with the node, as it does before a call.
	as, err := caller.NewStream(ctx, node.Host.ID(), api.AuthProtocolID)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := proto.Marshal(&api.AuthFrame{Biscuit: r.mint(caller.ID().String(), api.RoleNode)})
	if err != nil {
		t.Fatal(err)
	}
	if err := msgio.NewVarintWriter(as).WriteMsg(frame); err != nil {
		t.Fatal(err)
	}
	reply, err := msgio.NewVarintReaderSize(as, 1024*64).ReadMsg()
	if err != nil {
		t.Fatalf("handshake reply: %v", err)
	}
	var resp api.AuthResponse
	if err := proto.Unmarshal(reply, &resp); err != nil || !resp.Success {
		t.Fatalf("handshake refused: %v %s", err, resp.GetError())
	}
	_ = as.Close()

	_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatalf("read after the handshake: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("got %q, want %q", buf, "hello")
	}
}
