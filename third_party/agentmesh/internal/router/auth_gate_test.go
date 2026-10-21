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

package router

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	dto "github.com/prometheus/client_model/go"

	"github.com/google/agentmesh/api"
)

const echoProtocol protocol.ID = "/test/echo/1.0.0"

// newGatedRouter is a router whose DHT and gossip host is the gate, with a
// short auth deadline so an unauthenticated peer is cut off quickly.
func newGatedRouter(t *testing.T) (*Router, *authGate) {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &Router{Host: h, ctx: ctx, directory: api.DefaultDirectorySettings(), config: Options{AuthDeadline: 2 * time.Second}}
	return r, &authGate{Host: h, r: r}
}

func newClient(t *testing.T, r *Router) host.Host {
	t.Helper()
	c, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx, peer.AddrInfo{ID: r.Host.ID(), Addrs: r.Host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	return c
}

// waitForInboundStream returns once the router holds a negotiated stream
// on proto from p, so the gate's handler has been dispatched for it.
func waitForInboundStream(t *testing.T, r *Router, p peer.ID, proto protocol.ID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, conn := range r.Host.Network().ConnsToPeer(p) {
			for _, st := range conn.GetStreams() {
				if st.Protocol() == proto {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the router never saw a %s stream from %s", proto, p)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func unauthenticatedRefusals(t *testing.T, proto protocol.ID) float64 {
	t.Helper()
	var m dto.Metric
	if err := streamsRefusedTotal.WithLabelValues(streamScopeUnauthenticated, string(proto)).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

// A handler registered through the gate serves a peer only once it
// authenticated: a stream that arrives first waits for the handshake and is
// then served; a peer that never authenticates gets nothing, and its stream
// is counted as refused when its connection closes at the auth deadline.
func TestGateServesOnlyAuthenticatedPeers(t *testing.T) {
	r, gated := newGatedRouter(t)
	gated.SetStreamHandler(echoProtocol, func(s network.Stream) {
		defer func() { _ = s.Close() }()
		_, _ = io.Copy(s, s)
	})

	t.Run("a stream opened before the handshake is served after it", func(t *testing.T) {
		c := newClient(t, r)
		s, err := c.NewStream(context.Background(), r.Host.ID(), echoProtocol)
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

		r.authenticatedPeers.Store(c.ID(), time.Now().Add(time.Hour))
		_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(s, buf); err != nil {
			t.Fatalf("read after the handshake: %v", err)
		}
		if string(buf) != "hello" {
			t.Fatalf("got %q, want %q", buf, "hello")
		}
	})

	t.Run("a peer that never authenticates is refused and counted", func(t *testing.T) {
		before := unauthenticatedRefusals(t, echoProtocol)
		c := newClient(t, r)
		s, err := c.NewStream(context.Background(), r.Host.ID(), echoProtocol)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		waitForInboundStream(t, r, c.ID(), echoProtocol)
		// The connection goes, as it would at the auth deadline.
		if err := c.Network().ClosePeer(r.Host.ID()); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for unauthenticatedRefusals(t, echoProtocol) != before+1 {
			if time.Now().After(deadline) {
				t.Fatalf("refusals = %v, want %v: a stream from a peer that never authenticated must be counted", unauthenticatedRefusals(t, echoProtocol), before+1)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run("a stream the router opens waits for the handshake", func(t *testing.T) {
		c := newClient(t, r)
		c.SetStreamHandler(echoProtocol, func(s network.Stream) { _ = s.Close() })
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if _, err := gated.NewStream(ctx, c.ID(), echoProtocol); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("NewStream to an unauthenticated peer: err = %v, want the context's deadline", err)
		}
		r.authenticatedPeers.Store(c.ID(), time.Now().Add(time.Hour))
		s, err := gated.NewStream(context.Background(), c.ID(), echoProtocol)
		if err != nil {
			t.Fatalf("NewStream to an authenticated peer: %v", err)
		}
		_ = s.Close()
	})
}

// The router's DHT, built on the gate, answers a member's query only once
// the member authenticated, and its table admits authenticated peer routers
// and nothing else.
func TestRouterDHTServesAuthenticatedMembersAndTablesPeerRouters(t *testing.T) {
	r, gated := newGatedRouter(t)
	kdht, err := dht.New(gated, r.dhtOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kdht.Close() })
	r.DHT = kdht

	// A member: a DHT client that asks the router for the peers closest to a key.
	member := newClient(t, r)
	memberDHT, err := dht.New(member, dht.Mode(dht.ModeClient), dht.ProtocolPrefix(api.DHTProtocolPrefix), dht.BucketSize(3), dht.DisableAutoRefresh())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = memberDHT.Close() })
	if _, err := memberDHT.RoutingTable().TryAddPeer(r.Host.ID(), true, false); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	if _, err := memberDHT.GetClosestPeers(ctx, "a key"); err == nil {
		t.Fatal("an unauthenticated member's query was answered")
	}
	cancel()

	r.authenticatedPeers.Store(member.ID(), time.Now().Add(time.Hour))
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := memberDHT.GetClosestPeers(ctx, "a key"); err != nil {
		t.Fatalf("an authenticated member's query failed: %v", err)
	}
	if kdht.RoutingTable().Find(member.ID()) != "" {
		t.Fatal("a member entered the router's routing table")
	}

	// A peer router in server mode: in the table once it authenticates, not
	// before. The DHT hears of a peer when identify completes.
	identified, err := r.Host.EventBus().Subscribe(new(event.EvtPeerIdentificationCompleted))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = identified.Close() })
	other := newClient(t, r)
	otherDHT, err := dht.New(other, dht.Mode(dht.ModeServer), dht.ProtocolPrefix(api.DHTProtocolPrefix))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherDHT.Close() })
	for seen := false; !seen; {
		select {
		case e := <-identified.Out():
			seen = e.(event.EvtPeerIdentificationCompleted).Peer == other.ID()
		case <-time.After(5 * time.Second):
			t.Fatal("the router never identified the peer")
		}
	}
	if kdht.RoutingTable().Find(other.ID()) != "" {
		t.Fatal("a server-mode peer that never authenticated entered the routing table")
	}
	r.authenticatedPeers.Store(other.ID(), time.Now().Add(time.Hour))
	// kad-dht handles the identify event on its own schedule and drops a
	// peer its filter refused at that instant; the federation pass admits
	// an authenticated router again on every tick, which this stands in for.
	deadline := time.Now().Add(5 * time.Second)
	for r.admitPeerRouter(other.ID()); kdht.RoutingTable().Find(other.ID()) == ""; r.admitPeerRouter(other.ID()) {
		if time.Now().After(deadline) {
			t.Fatal("an authenticated peer router is not in the routing table")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.forgetPeer(other.ID())
	if kdht.RoutingTable().Find(other.ID()) != "" {
		t.Fatal("a peer router whose authentication lapsed is still in the routing table")
	}
}
