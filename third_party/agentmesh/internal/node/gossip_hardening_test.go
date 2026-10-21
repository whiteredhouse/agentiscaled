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
	"crypto/rand"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/ratelimit"
	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pubsub_pb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// signedMeshEvent returns a BANNED event for target signed by cpPriv, as the
// control plane would publish it.
func signedMeshEvent(t *testing.T, cpPriv ed25519.PrivateKey, target peer.ID, at time.Time) []byte {
	t.Helper()
	event := &api.MeshEvent{Type: api.MeshEvent_BANNED, PeerId: target.String(), EventTime: timestamppb.New(at)}
	unsigned, err := proto.MarshalOptions{Deterministic: true}.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	event.Signature = ed25519.Sign(cpPriv, unsigned)
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func randomPeerID(t *testing.T) peer.ID {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// The GossipSub validator is the first thing a mesh event meets. It has to
// reject (drop, do not forward) what is unsigned or forged, ignore what is
// stale, and accept a fresh event from a trusted control plane.
func TestValidateMeshEvent(t *testing.T) {
	cpPub, cpPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	node := &AgentMeshNode{
		trustedKeys:    []TrustedKey{{Key: cpPub, ReceivedAt: time.Now()}},
		BiscuitTimeout: 500 * time.Millisecond,
	}
	from := randomPeerID(t)
	target := randomPeerID(t)

	tests := []struct {
		name string
		data []byte
		want pubsub.ValidationResult
	}{
		{"fresh event from trusted control plane", signedMeshEvent(t, cpPriv, target, time.Now()), pubsub.ValidationAccept},
		{"event signed by an untrusted key", signedMeshEvent(t, otherPriv, target, time.Now()), pubsub.ValidationReject},
		{"undecodable payload", []byte("junk"), pubsub.ValidationReject},
		{"stale event", signedMeshEvent(t, cpPriv, target, time.Now().Add(-2*FreshnessThreshold)), pubsub.ValidationIgnore},
		{"future event", signedMeshEvent(t, cpPriv, target, time.Now().Add(2*FreshnessThreshold)), pubsub.ValidationIgnore},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &pubsub.Message{Message: &pubsub_pb.Message{From: []byte(from), Data: tt.data}}
			if got := node.validateMeshEvent(context.Background(), from, msg); got != tt.want {
				t.Errorf("validateMeshEvent = %v, want %v", got, tt.want)
			}
		})
	}
}

// gossipPeer is a plain libp2p host with its own GossipSub and no validator:
// any peer that can reach the topic, or a router forwarding one. It is
// subscribed so it is a topic peer to whoever it connects to.
type gossipPeer struct {
	host  host.Host
	ps    *pubsub.PubSub
	topic *pubsub.Topic
	sub   *pubsub.Subscription
}

func newGossipPeer(t *testing.T, ctx context.Context) *gossipPeer {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	topic, err := ps.Join(api.GossipEvents)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := topic.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Cancel)
	return &gossipPeer{host: h, ps: ps, topic: topic, sub: sub}
}

func (g *gossipPeer) connect(t *testing.T, ctx context.Context, h host.Host) {
	t.Helper()
	if err := g.host.Connect(ctx, peer.AddrInfo{ID: h.ID(), Addrs: h.Addrs()}); err != nil {
		t.Fatalf("connect %s -> %s: %v", g.host.ID(), h.ID(), err)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A flood of junk on the events topic must neither be re-forwarded by the
// node nor cost it the signed event that follows. Before the validator the
// node forwarded every message and rate-limited on the forwarding peer, so a
// flood via the router spent the router's budget and real bans were dropped.
func TestGossipJunkFloodIsRejectedNotForwarded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	victim, cleanup := startBareNode(t, ctx)
	defer cleanup()

	cpPub, cpPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	victim.keysMu.Lock()
	victim.trustedKeys = append(victim.trustedKeys, TrustedKey{Key: cpPub, ReceivedAt: time.Now()})
	victim.keysMu.Unlock()

	// attacker -> victim -> observer. The observer only ever hears what the
	// victim chooses to forward. Both authenticated with the victim, as a
	// caller does: the victim gossips with no one who did not.
	attacker := newGossipPeer(t, ctx)
	observer := newGossipPeer(t, ctx)
	for _, g := range []*gossipPeer{attacker, observer} {
		victim.authPeers.Store(g.host.ID(), time.Now().Add(time.Hour))
		g.connect(t, ctx, victim.Host)
	}

	var mu sync.Mutex
	var forwardedJunk []string
	seenSigned := map[peer.ID]bool{}
	sawSigned := func(pid peer.ID) bool {
		mu.Lock()
		defer mu.Unlock()
		return seenSigned[pid]
	}
	go func() {
		for {
			msg, err := observer.sub.Next(ctx)
			if err != nil {
				return
			}
			// Only what came through the victim counts: a direct
			// attacker->observer connection would say nothing about the
			// victim.
			if msg.ReceivedFrom != victim.Host.ID() {
				continue
			}
			mu.Lock()
			var event api.MeshEvent
			if err := proto.Unmarshal(msg.Data, &event); err != nil || len(event.Signature) == 0 {
				forwardedJunk = append(forwardedJunk, string(msg.Data))
			} else if pid, err := peer.Decode(event.PeerId); err == nil {
				seenSigned[pid] = true
			}
			mu.Unlock()
		}
	}()

	waitUntil(t, 10*time.Second, "topic peers to see each other", func() bool {
		victimSees := victim.PubSub.ListPeers(api.GossipEvents)
		return slices.Contains(victimSees, attacker.host.ID()) &&
			slices.Contains(victimSees, observer.host.ID()) &&
			slices.Contains(attacker.ps.ListPeers(api.GossipEvents), victim.Host.ID()) &&
			slices.Contains(observer.ps.ListPeers(api.GossipEvents), victim.Host.ID())
	})

	// Probe: a signed event travels attacker -> victim -> observer, so the
	// forwarding path is up before the flood starts.
	first := randomPeerID(t)
	waitUntil(t, 10*time.Second, "probe event to be forwarded", func() bool {
		_ = attacker.topic.Publish(ctx, signedMeshEvent(t, cpPriv, first, time.Now()))
		return sawSigned(first)
	})

	// Flood: far more than the per-peer budget, all unsigned.
	for i := range 5 * ratelimit.PeerBurst {
		if err := attacker.topic.Publish(ctx, fmt.Appendf(nil, "junk-%d", i)); err != nil {
			t.Fatalf("publish junk: %v", err)
		}
	}

	// The event that matters, from the same forwarding peer, right behind
	// the flood.
	second := randomPeerID(t)
	if err := attacker.topic.Publish(ctx, signedMeshEvent(t, cpPriv, second, time.Now())); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, "signed event to be forwarded after the flood", func() bool {
		return sawSigned(second)
	})
	if !victim.revokedPeers.Contains(second.String()) {
		t.Error("the signed ban behind the flood was not applied by the node")
	}

	// Anything the victim forwarded is ordered on the observer's stream, so
	// junk it let through has arrived by now.
	mu.Lock()
	defer mu.Unlock()
	if len(forwardedJunk) != 0 {
		t.Errorf("victim forwarded %d unsigned messages; the validator must drop them at the first hop (e.g. %q)", len(forwardedJunk), forwardedJunk[0])
	}
}
