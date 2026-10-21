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
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pubsub_pb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newTestPeerID(t *testing.T) peer.ID {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A relay circuit needs both ends authenticated. Every node authenticates to
// the router on connect, so a source that has not is not a mesh member and
// must not reach admitted peers through the router.
func TestRelayACLAllowConnectRequiresAuthenticatedSource(t *testing.T) {
	r := &Router{}
	acl := &relayACL{r: r}
	src, dest := newTestPeerID(t), newTestPeerID(t)
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/1234")

	r.authenticatedPeers.Store(dest, true)
	if acl.AllowConnect(src, addr, dest) {
		t.Error("an unauthenticated source must not connect to an authenticated destination")
	}

	r.authenticatedPeers.Store(src, true)
	if !acl.AllowConnect(src, addr, dest) {
		t.Error("two authenticated peers must be allowed to connect")
	}

	r.authenticatedPeers.Delete(dest)
	if acl.AllowConnect(src, addr, dest) {
		t.Error("an authenticated source must not connect to an unauthenticated destination")
	}

	r.authenticatedPeers.Store(dest, true)
	r.bannedPeers.Store(src, time.Now())
	if acl.AllowConnect(src, addr, dest) {
		t.Error("a banned source must not connect even while still marked authenticated")
	}
}

func TestRelayACLExpiresAuthenticatedPeers(t *testing.T) {
	r := &Router{}
	acl := &relayACL{r: r}
	src, dest := newTestPeerID(t), newTestPeerID(t)
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/1234")

	r.authenticatedPeers.Store(src, time.Now().Add(time.Hour))
	r.authenticatedPeers.Store(dest, time.Now().Add(time.Hour))
	if !acl.AllowReserve(src, addr) {
		t.Fatal("expected peer with future expiration to be allowed to reserve")
	}
	if !acl.AllowConnect(src, addr, dest) {
		t.Fatal("expected peers with future expiration to be allowed to connect")
	}

	// Expire src's biscuit.
	r.authenticatedPeers.Store(src, time.Now().Add(-time.Second))
	if acl.AllowReserve(src, addr) {
		t.Error("expected peer with expired biscuit to be denied reservation")
	}
	if _, stillPresent := r.authenticatedPeers.Load(src); stillPresent {
		t.Error("expected expired peer to be evicted from authenticatedPeers")
	}
	r.authenticatedPeers.Store(src, time.Now().Add(-time.Second))
	if acl.AllowConnect(src, addr, dest) {
		t.Error("expected expired source peer to be denied relay connect")
	}
}

func TestRouterConnGateRejectsBannedPeers(t *testing.T) {
	r := &Router{}
	gate := &routerConnGate{r: r}
	allowedPeer, bannedPeer := newTestPeerID(t), newTestPeerID(t)
	r.bannedPeers.Store(bannedPeer, time.Now())

	if !gate.InterceptPeerDial(allowedPeer) {
		t.Error("expected allowed peer dial to succeed")
	}
	if gate.InterceptPeerDial(bannedPeer) {
		t.Error("expected banned peer dial to be rejected")
	}
	if !gate.InterceptSecured(0, allowedPeer, nil) {
		t.Error("expected allowed peer secured connection to succeed")
	}
	if gate.InterceptSecured(0, bannedPeer, nil) {
		t.Error("expected banned peer secured connection to be rejected")
	}
}

// The router's GossipSub validator drops unsigned or forged events at the
// first hop instead of fanning them out to every attached node.
func TestRouterValidateMeshEvent(t *testing.T) {
	cpPub, cpPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := &Router{trustedPublicKeys: []ed25519.PublicKey{cpPub}}
	from, target := newTestPeerID(t), newTestPeerID(t)

	signed := func(key ed25519.PrivateKey, at time.Time) []byte {
		event := &api.MeshEvent{Type: api.MeshEvent_BANNED, PeerId: target.String(), EventTime: timestamppb.New(at)}
		unsigned, err := proto.MarshalOptions{Deterministic: true}.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		event.Signature = ed25519.Sign(key, unsigned)
		data, err := proto.MarshalOptions{Deterministic: true}.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	tests := []struct {
		name string
		data []byte
		want pubsub.ValidationResult
	}{
		{"fresh event from trusted control plane", signed(cpPriv, time.Now()), pubsub.ValidationAccept},
		{"event signed by an untrusted key", signed(otherPriv, time.Now()), pubsub.ValidationReject},
		{"undecodable payload", []byte("junk"), pubsub.ValidationReject},
		{"stale event", signed(cpPriv, time.Now().Add(-2*meshEventFreshness)), pubsub.ValidationIgnore},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &pubsub.Message{Message: &pubsub_pb.Message{From: []byte(from), Data: tt.data}}
			if got := r.validateMeshEvent(context.Background(), from, msg); got != tt.want {
				t.Errorf("validateMeshEvent = %v, want %v", got, tt.want)
			}
		})
	}
}
