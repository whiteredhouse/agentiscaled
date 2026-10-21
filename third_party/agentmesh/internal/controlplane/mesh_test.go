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

package controlplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"

	"github.com/libp2p/go-libp2p"
	"google.golang.org/protobuf/proto"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/identity"
	"github.com/google/agentmesh/internal/storage"
)

func TestNopMeshAdapter(t *testing.T) {
	adapter := NewNopMeshAdapter()
	ctx := context.Background()

	if err := adapter.PublishEvent(ctx, api.MeshEvent_POLICY_UPDATE, "", nil); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	services, err := adapter.DiscoverServices(ctx, "test")
	if err != nil || len(services) != 0 {
		t.Fatalf("unexpected DiscoverServices output: %v, %v", services, err)
	}

	status, err := adapter.GetNodeStatus(ctx, "peer1")
	if err != nil || status != nil {
		t.Fatalf("unexpected GetNodeStatus output: %v, %v", status, err)
	}

	if err := adapter.Close(); err != nil {
		t.Fatalf("expected nil error on Close, got %v", err)
	}
}

func TestP2PMeshAdapter_PublishAndSubscribe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Create Control Plane store & initial keyring
	store, err := storage.NewSQLStore("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer func() { _ = store.Close() }()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	if err := store.SaveInitialKey(ctx, priv, pub); err != nil {
		t.Fatalf("failed to save key: %v", err)
	}

	// 2. Create libp2p host and PubSub for Control Plane P2PMeshAdapter
	cpHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("failed to create cp host: %v", err)
	}
	defer func() { _ = cpHost.Close() }()

	cpPS, err := pubsub.NewGossipSub(ctx, cpHost)
	if err != nil {
		t.Fatalf("failed to create cp pubsub: %v", err)
	}
	cpTopic, err := cpPS.Join(api.GossipEvents)
	if err != nil {
		t.Fatalf("failed to join cp topic: %v", err)
	}

	adapter, err := NewP2PMeshAdapter(cpHost, cpTopic, store)
	if err != nil {
		t.Fatalf("failed to create P2PMeshAdapter: %v", err)
	}
	defer func() { _ = adapter.Close() }()

	// 3. Create subscriber node host & PubSub
	subHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("failed to create sub host: %v", err)
	}
	defer func() { _ = subHost.Close() }()

	subPS, err := pubsub.NewGossipSub(ctx, subHost)
	if err != nil {
		t.Fatalf("failed to create sub pubsub: %v", err)
	}

	subTopic, err := subPS.Join(api.GossipEvents)
	if err != nil {
		t.Fatalf("failed to join topic: %v", err)
	}
	sub, err := subTopic.Subscribe()
	if err != nil {
		t.Fatalf("failed to subscribe to topic: %v", err)
	}

	// 4. Connect subscriber node to Control Plane host
	subHost.Peerstore().AddAddrs(cpHost.ID(), cpHost.Addrs(), 10*time.Second)
	if err := subHost.Connect(ctx, peer.AddrInfo{ID: cpHost.ID(), Addrs: cpHost.Addrs()}); err != nil {
		t.Fatalf("failed to connect subHost to cpHost: %v", err)
	}

	// Allow GossipSub mesh overlay connection to settle
	time.Sleep(500 * time.Millisecond)

	// 5. Test publishing BANNED event
	_, targetID := newTestKey(t)
	targetPeerID := targetID.String()
	if err := adapter.PublishEvent(ctx, api.MeshEvent_BANNED, targetPeerID, nil); err != nil {
		t.Fatalf("failed to publish BANNED event: %v", err)
	}

	msg, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("failed to receive event on subscriber: %v", err)
	}

	var event api.MeshEvent
	if err := proto.Unmarshal(msg.Data, &event); err != nil {
		t.Fatalf("failed to unmarshal received MeshEvent: %v", err)
	}

	if event.Type != api.MeshEvent_BANNED {
		t.Fatalf("expected event type %v, got %v", api.MeshEvent_BANNED, event.Type)
	}
	if event.PeerId != targetPeerID {
		t.Fatalf("expected peerID %q, got %q", targetPeerID, event.PeerId)
	}
	if len(event.Signature) == 0 {
		t.Fatalf("expected signed event signature, got empty signature")
	}

	// Verify Ed25519 signature against Control Plane public key
	sig := event.Signature
	event.Signature = nil
	eventData, err := proto.Marshal(&event)
	if err != nil {
		t.Fatalf("failed to marshal event for sig verification: %v", err)
	}
	if !ed25519.Verify(pub, eventData, sig) {
		t.Fatalf("ed25519 signature verification failed for published MeshEvent")
	}
}

// A KEY_ROTATION announces a key nobody trusts yet, so it has to be signed by
// the key being retired: that is the only signature a node holding the old
// key can check. Signing with the new key (the store's current key after the
// rotation) made every node drop the announcement as a spoofing attempt.
func TestKeyRotationEventIsSignedByTheRetiringKey(t *testing.T) {
	ctx := context.Background()
	store, err := storage.NewSQLStore("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	oldPub, oldPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveInitialKey(ctx, oldPriv, oldPub); err != nil {
		t.Fatal(err)
	}
	newPub, newPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RotateKeys(ctx, newPriv, newPub, time.Hour); err != nil {
		t.Fatal(err)
	}

	adapter := &P2PMeshAdapter{store: store}
	signer, err := adapter.signingKeyFor(ctx, api.MeshEvent_KEY_ROTATION, newPub)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(signer, oldPriv) {
		t.Error("KEY_ROTATION must be signed by the retiring key, which is the one receivers still trust")
	}

	// Every other event is signed by the current key.
	signer, err = adapter.signingKeyFor(ctx, api.MeshEvent_BANNED, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(signer, newPriv) {
		t.Error("a BANNED event after rotation must be signed by the current key")
	}
}

// The shipped control plane's mesh presence (#317): a publish-only peer that
// finds the routers through their leases, dials them, and gets an event to a
// subscriber on the other side. Nothing is listening on the publisher's
// side, so nothing can dial it.
func TestMeshPublisherReachesLeasedRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, err := storage.NewSQLStore("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer func() { _ = store.Close() }()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	if err := store.SaveInitialKey(ctx, priv, pub); err != nil {
		t.Fatalf("failed to save key: %v", err)
	}

	// A router as the publisher sees it: a host subscribed to the events
	// topic, known only through the lease it renews.
	routerHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("failed to create router host: %v", err)
	}
	defer func() { _ = routerHost.Close() }()
	routerPS, err := pubsub.NewGossipSub(ctx, routerHost, pubsub.WithMessageSignaturePolicy(pubsub.StrictSign))
	if err != nil {
		t.Fatalf("failed to create router pubsub: %v", err)
	}
	routerTopic, err := routerPS.Join(api.GossipEvents)
	if err != nil {
		t.Fatalf("failed to join topic: %v", err)
	}
	sub, err := routerTopic.Subscribe()
	if err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}
	// The router admits peers on the handshake; the publisher is one of them.
	// What it presents must verify under the control plane's key, be bound
	// to the publisher's peer and carry the control-plane role.
	admitted := make(chan error, 4)
	routerHost.SetStreamHandler(api.AuthProtocolID, func(s network.Stream) {
		defer func() { _ = s.Close() }()
		msg, err := msgio.NewVarintReaderSize(s, 64*1024).ReadMsg()
		if err != nil {
			admitted <- err
			return
		}
		var frame api.AuthFrame
		if err := proto.Unmarshal(msg, &frame); err != nil {
			admitted <- err
			return
		}
		if _, err := identity.VerifyBiscuitAndGetExpiry(frame.Biscuit, s.Conn().RemotePeer(), []ed25519.PublicKey{pub}, 5*time.Second); err != nil {
			admitted <- fmt.Errorf("credential: %w", err)
			return
		}
		if err := identity.VerifyBiscuitRole(frame.Biscuit, pub, api.RoleControlPlane, 5*time.Second); err != nil {
			admitted <- fmt.Errorf("role: %w", err)
			return
		}
		out, _ := proto.Marshal(&api.AuthResponse{Success: true})
		admitted <- msgio.NewVarintWriter(s).WriteMsg(out)
	})
	now := time.Now()
	lease := storage.RouterLease{PeerID: routerHost.ID().String(), LastRenewal: now, ExpiresAt: now.Add(time.Hour)}
	for _, a := range routerHost.Addrs() {
		lease.Addresses = append(lease.Addresses, a.String()+"/p2p/"+routerHost.ID().String())
	}
	// An expired lease must not be dialed: it is a router that is gone.
	stale := storage.RouterLease{PeerID: routerHost.ID().String() + "x", Addresses: []string{"/ip4/127.0.0.1/tcp/1"}, LastRenewal: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	for _, l := range []*storage.RouterLease{&lease, &stale} {
		if err := store.UpsertRouterLease(ctx, l); err != nil {
			t.Fatalf("lease %s: %v", l.PeerID, err)
		}
	}

	publisher, err := NewMeshPublisher(ctx, store, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("NewMeshPublisher: %v", err)
	}
	defer func() {
		if err := publisher.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if addrs := publisher.host.Network().ListenAddresses(); len(addrs) != 0 {
		t.Errorf("publisher must not listen, has %v", addrs)
	}

	// The router announces its subscription on connect; the publisher's
	// first publish must have somewhere to go. The publisher itself never
	// subscribes, so the router's peer list for the topic stays empty.
	for routerHost.Network().Connectedness(publisher.host.ID()) != network.Connected {
		select {
		case <-ctx.Done():
			t.Fatal("publisher never connected to the leased router")
		case <-time.After(20 * time.Millisecond):
		}
	}
	for len(publisher.topic.ListPeers()) == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("publisher never learned that the router is subscribed to the events topic")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if peers := routerPS.ListPeers(api.GossipEvents); len(peers) != 0 {
		t.Errorf("publisher must not subscribe to the topic, router sees %v", peers)
	}
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("publisher's handshake: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("publisher never ran the handshake with the router")
	}
	// admitted fires as soon as the router writes its reply; the publisher
	// records the admission once it reads that reply off the stream.
	for {
		if until, ok := publisher.admittedAt(routerHost.ID()); ok {
			if time.Until(until) < publisherCredentialTTL/2 {
				t.Fatalf("publisher does not record its admission at the router (until %v, %v)", until, ok)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("publisher never recorded its admission at the router")
		case <-time.After(10 * time.Millisecond):
		}
	}

	_, target := newTestKey(t)
	if err := publisher.PublishEvent(ctx, api.MeshEvent_BANNED, target.String(), nil); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	msg, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("router did not receive the event: %v", err)
	}
	var event api.MeshEvent
	if err := proto.Unmarshal(msg.Data, &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if event.Type != api.MeshEvent_BANNED || event.PeerId != target.String() {
		t.Fatalf("got event %v for %q, want BANNED for %q", event.Type, event.PeerId, target)
	}
}

// Lease addresses carry a trailing /p2p/<id>; the dialer gets the id once
// and the addresses bare, and an address naming another peer is dropped.
func TestRouterAddrInfo(t *testing.T) {
	_, pid := newTestKey(t)
	_, other := newTestKey(t)

	info, err := routerAddrInfo(storage.RouterLease{
		PeerID: pid.String(),
		Addresses: []string{
			"/ip4/10.0.0.1/tcp/4501/p2p/" + pid.String(),
			"/dnsaddr/bootstrap.example/p2p/" + pid.String(),
			"/ip4/10.0.0.2/tcp/4501/p2p/" + other.String(),
			"/ip4/10.0.0.3/tcp/4501",
			"not a multiaddr",
		},
	})
	if err != nil {
		t.Fatalf("routerAddrInfo: %v", err)
	}
	if info.ID != pid {
		t.Errorf("peer = %s, want %s", info.ID, pid)
	}
	var got []string
	for _, a := range info.Addrs {
		got = append(got, a.String())
	}
	want := []string{"/ip4/10.0.0.1/tcp/4501", "/dnsaddr/bootstrap.example", "/ip4/10.0.0.3/tcp/4501"}
	if len(got) != len(want) {
		t.Fatalf("addrs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("addrs[%d] = %s, want %s", i, got[i], want[i])
		}
	}

	if _, err := routerAddrInfo(storage.RouterLease{PeerID: "not-a-peer", Addresses: []string{"/ip4/10.0.0.1/tcp/4501"}}); err == nil {
		t.Error("an undecodable peer ID must be rejected")
	}
	if _, err := routerAddrInfo(storage.RouterLease{PeerID: pid.String(), Addresses: []string{"/ip4/10.0.0.2/tcp/4501/p2p/" + other.String()}}); err == nil {
		t.Error("a lease with no address for its own peer must be rejected")
	}
}
