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
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/proto"

	"github.com/google/agentmesh/api"
)

// goAwayListener is a peer that takes go-aways and records them.
type goAwayListener struct {
	host host.Host
	mu   sync.Mutex
	got  []*api.RouterGoAway
}

func newGoAwayListener(t *testing.T) *goAwayListener {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	l := &goAwayListener{host: h}
	h.SetStreamHandler(api.GoAwayProtocolID, func(s network.Stream) {
		defer func() { _ = s.Close() }()
		msg, err := msgio.NewVarintReaderSize(s, 1024).ReadMsg()
		if err != nil {
			return
		}
		var goAway api.RouterGoAway
		if err := proto.Unmarshal(msg, &goAway); err != nil {
			return
		}
		l.mu.Lock()
		l.got = append(l.got, &goAway)
		l.mu.Unlock()
	})
	return l
}

func (l *goAwayListener) received() []*api.RouterGoAway {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*api.RouterGoAway(nil), l.got...)
}

// connectAuthenticated dials the router from p and marks p as a member.
func connectAuthenticated(t *testing.T, r *Router, p host.Host) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Connect(ctx, peer.AddrInfo{ID: r.Host.ID(), Addrs: r.Host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	r.authenticatedPeers.Store(p.ID(), time.Now().Add(time.Hour))
}

func newGoAwayRouter(t *testing.T, high int) *Router {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return &Router{Host: h, config: Options{HighWaterMark: high, LowWaterMark: DefaultLowWaterMark(high)}}
}

// A router at its high watermark of members sends enough of them away to be
// back at the low one, says why and for how long, and closes their
// connections after the grace; a peer router is never among them, a
// connection that never authenticated is not counted, and a member that
// does not speak the protocol is closed all the same.
func TestShedIfOverloaded(t *testing.T) {
	prev := goAwayGrace
	goAwayGrace = 50 * time.Millisecond
	t.Cleanup(func() { goAwayGrace = prev })

	// High 3, low 2: at 3 members, one goes.
	r := newGoAwayRouter(t, 3)
	peerRouter := newGoAwayListener(t)
	connectAuthenticated(t, r, peerRouter.host)
	r.peerRouters.Store(peerRouter.host.ID(), true)
	members := []*goAwayListener{newGoAwayListener(t), newGoAwayListener(t)}
	for _, m := range members {
		connectAuthenticated(t, r, m.host)
	}
	mute, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mute.Close() })
	connectAuthenticated(t, r, mute)
	// A stranger: connected, never authenticated. Five inbound connections,
	// three members.
	stranger := newGoAwayListener(t)
	if err := stranger.host.Connect(context.Background(), peer.AddrInfo{ID: r.Host.ID(), Addrs: r.Host.Addrs()}); err != nil {
		t.Fatal(err)
	}

	if got := r.inboundConnections(); got != 5 {
		t.Fatalf("inbound = %d, want 5", got)
	}
	if got := len(r.members()); got != 3 {
		t.Fatalf("members = %d, want 3 (the peer router and the stranger are not members)", got)
	}

	shed := r.shedIfOverloaded(context.Background())
	if shed != 1 {
		t.Fatalf("shed %d members, want 1", shed)
	}
	var told int
	for _, m := range members {
		for _, g := range m.received() {
			told++
			if g.GetReason() != api.RouterGoAway_OVERLOADED || g.GetRetryAfter().AsDuration() != DefaultOverloadRetryAfter {
				t.Fatalf("go-away = %v, want OVERLOADED for %s", g, DefaultOverloadRetryAfter)
			}
		}
	}
	gone := 0
	for _, p := range []host.Host{members[0].host, members[1].host, mute} {
		if r.Host.Network().Connectedness(p.ID()) != network.Connected {
			gone++
			if r.isPeerAuthenticated(p.ID()) {
				t.Fatalf("%s was sent away but is still authenticated", p.ID())
			}
		}
	}
	if gone != 1 || told > 1 {
		t.Fatalf("after the shed: %d connections closed, %d go-aways delivered; want 1 and at most 1", gone, told)
	}
	if r.Host.Network().Connectedness(peerRouter.host.ID()) != network.Connected || len(peerRouter.received()) != 0 {
		t.Fatal("the peer router was sent away")
	}
	if r.Host.Network().Connectedness(stranger.host.ID()) != network.Connected || len(stranger.received()) != 0 {
		t.Fatal("the stranger was sent away; it was never a member")
	}
	if got := len(r.members()); got != 2 {
		t.Fatalf("members after the shed = %d, want 2", got)
	}
	if again := r.shedIfOverloaded(context.Background()); again != 0 {
		t.Fatalf("below the high watermark the router shed %d more", again)
	}
}

// drainMembers tells every member the router is stopping, for the time the
// control plane will keep it listed.
func TestDrainMembersSaysDraining(t *testing.T) {
	prev := goAwayGrace
	goAwayGrace = 50 * time.Millisecond
	t.Cleanup(func() { goAwayGrace = prev })

	r := newGoAwayRouter(t, 100)
	r.config.ShutdownLeaseTTL = 45 * time.Second
	a, b := newGoAwayListener(t), newGoAwayListener(t)
	connectAuthenticated(t, r, a.host)
	connectAuthenticated(t, r, b.host)

	r.drainMembers()

	for _, m := range []*goAwayListener{a, b} {
		got := m.received()
		if len(got) != 1 || got[0].GetReason() != api.RouterGoAway_DRAINING || got[0].GetRetryAfter().AsDuration() != 45*time.Second {
			t.Fatalf("%s received %v, want one DRAINING go-away for 45s", m.host.ID(), got)
		}
		if r.Host.Network().Connectedness(m.host.ID()) == network.Connected {
			t.Fatalf("%s is still connected after the drain", m.host.ID())
		}
	}
}

// Past the high watermark the connection manager closes connections, lowest
// tag first: a member that authenticated carries a tag, a stranger none, so
// the strangers go and the members stay; a peer router is protected and
// stays whatever its age. The trim counts protected connections out, so
// with low 1 and three candidates it closes two.
func TestTrimTakesUnauthenticatedConnectionsFirst(t *testing.T) {
	cm, err := connmgr.NewConnManager(1, 3, connmgr.WithGracePeriod(0))
	if err != nil {
		t.Fatal(err)
	}
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"), libp2p.ConnectionManager(cm))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	r := &Router{Host: h}

	newPeer := func() host.Host {
		p, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connect := func(p host.Host) {
		if err := p.Connect(ctx, peer.AddrInfo{ID: h.ID(), Addrs: h.Addrs()}); err != nil {
			t.Fatal(err)
		}
	}
	// Oldest first, the order a trim prefers among equal tags: a stranger,
	// then a member, then a peer router, then a second stranger that tips
	// the count over the high watermark.
	stranger := newPeer()
	connect(stranger)
	member := newPeer()
	connect(member)
	r.admitPeer(member.ID(), time.Now().Add(time.Hour))
	peerRouter := newPeer()
	connect(peerRouter)
	cm.Protect(peerRouter.ID(), federationTag)
	late := newPeer()
	connect(late)

	cm.TrimOpenConns(ctx)

	// The closes propagate through the swarm after TrimOpenConns returns.
	kept := 2
	deadline := time.Now().Add(5 * time.Second)
	for kept > 0 && time.Now().Before(deadline) {
		kept = 0
		for _, p := range []host.Host{stranger, late} {
			if h.Network().Connectedness(p.ID()) == network.Connected {
				kept++
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if kept != 0 {
		t.Fatalf("the trim kept %d of 2 unauthenticated connections while over the high watermark", kept)
	}
	if h.Network().Connectedness(member.ID()) != network.Connected {
		t.Fatal("the trim closed the member's connection")
	}
	if h.Network().Connectedness(peerRouter.ID()) != network.Connected {
		t.Fatal("the trim closed the peer router's connection")
	}
}

// A connection whose peer has not passed the handshake by the deadline is
// closed; one that has, and a peer router's, stay. A member that holds
// several connections is judged as a peer, not per connection.
func TestAuthDeadlineClosesStrangers(t *testing.T) {
	r := newGoAwayRouter(t, 100)
	r.config.AuthDeadline = 300 * time.Millisecond
	r.ctx = context.Background()
	r.Host.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, c network.Conn) { r.enforceAuthDeadline(c) },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial := func(p host.Host) {
		if err := p.Connect(ctx, peer.AddrInfo{ID: r.Host.ID(), Addrs: r.Host.Addrs()}); err != nil {
			t.Fatal(err)
		}
	}
	stranger := newGoAwayListener(t)
	dial(stranger.host)
	member := newGoAwayListener(t)
	dial(member.host)
	r.authenticatedPeers.Store(member.host.ID(), time.Now().Add(time.Hour))
	peerRouter := newGoAwayListener(t)
	r.peerRouters.Store(peerRouter.host.ID(), true)
	dial(peerRouter.host)
	// Outbound connections are the router's own business.
	dialed := newGoAwayListener(t)
	if err := r.Host.Connect(ctx, peer.AddrInfo{ID: dialed.host.ID(), Addrs: dialed.host.Addrs()}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for r.Host.Network().Connectedness(stranger.host.ID()) == network.Connected && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if r.Host.Network().Connectedness(stranger.host.ID()) == network.Connected {
		t.Fatal("the stranger's connection survived the deadline")
	}
	for name, p := range map[string]host.Host{"member": member.host, "peer router": peerRouter.host, "dialed peer": dialed.host} {
		if r.Host.Network().Connectedness(p.ID()) != network.Connected {
			t.Errorf("the %s's connection was closed", name)
		}
	}
}
