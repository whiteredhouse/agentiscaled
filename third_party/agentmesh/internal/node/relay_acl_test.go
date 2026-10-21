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
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNodeRelayACL_AllowConnect(t *testing.T) {
	node := &AgentMeshNode{BiscuitTimeout: 500 * time.Millisecond}
	acl := &nodeRelayACL{node: node}

	srcPeer := peer.ID("src-peer")
	destPeer := peer.ID("dest-peer")
	srcAddr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/1234")

	// Neither is authenticated
	if acl.AllowConnect(srcPeer, srcAddr, destPeer) {
		t.Errorf("Expected AllowConnect to return false when dest is not authenticated")
	}

	// Src is authenticated, dest is not -> should fail
	node.authPeers.Store(srcPeer, time.Now().Add(time.Hour))
	if acl.AllowConnect(srcPeer, srcAddr, destPeer) {
		t.Errorf("Expected AllowConnect to return false when dest is not authenticated, even if src is")
	}

	// Dest is authenticated, src is not -> should fail: an unauthenticated
	// source must not reach admitted peers through this node's relay.
	node.authPeers.Delete(srcPeer)
	node.authPeers.Store(destPeer, time.Now().Add(time.Hour))
	if acl.AllowConnect(srcPeer, srcAddr, destPeer) {
		t.Errorf("Expected AllowConnect to return false when src is not authenticated, even if dest is")
	}

	// Both authenticated -> should succeed
	node.authPeers.Store(srcPeer, time.Now().Add(time.Hour))
	if !acl.AllowConnect(srcPeer, srcAddr, destPeer) {
		t.Errorf("Expected AllowConnect to return true when both src and dest are authenticated")
	}
}

func TestNodeRelayACL_AllowReserve(t *testing.T) {
	node := &AgentMeshNode{BiscuitTimeout: 500 * time.Millisecond}
	acl := &nodeRelayACL{node: node}

	peerID := peer.ID("some-peer")
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/1234")

	if acl.AllowReserve(peerID, addr) {
		t.Errorf("Expected AllowReserve to return false when peer is not authenticated")
	}

	node.authPeers.Store(peerID, time.Now().Add(time.Hour))
	if !acl.AllowReserve(peerID, addr) {
		t.Errorf("Expected AllowReserve to return true when peer is authenticated")
	}
}

// TestExpiredAdmissionLosesRelayRights covers a token lapsing after the peer was
// admitted. The handshake only proves validity at that instant, so the ACL has
// to re-check, otherwise one handshake buys relay rights forever.
func TestExpiredAdmissionLosesRelayRights(t *testing.T) {
	node := &AgentMeshNode{BiscuitTimeout: 500 * time.Millisecond}
	acl := &nodeRelayACL{node: node}

	peerID := peer.ID("lapsed-peer")
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/1234")

	node.authPeers.Store(peerID, time.Now().Add(-time.Second))
	if acl.AllowReserve(peerID, addr) {
		t.Error("expired admission still holds relay rights")
	}
	if _, still := node.authPeers.Load(peerID); still {
		t.Error("expired admission was not evicted")
	}

	// A value of any other type is a bug, not an admission.
	node.authPeers.Store(peerID, true)
	if acl.AllowReserve(peerID, addr) {
		t.Error("malformed admission entry granted relay rights")
	}
}

// TestBannedPeerLosesRelayRights covers a ban arriving after the peer was
// already admitted: the relay ACL reads authPeers, so leaving a stale entry
// there keeps granting reservations to a revoked peer.
func TestBannedPeerLosesRelayRights(t *testing.T) {
	revokedCache, err := lru.New[string, int64](10)
	if err != nil {
		t.Fatal(err)
	}
	node := &AgentMeshNode{revokedPeers: revokedCache, BiscuitTimeout: 500 * time.Millisecond}
	acl := &nodeRelayACL{node: node}

	peerID, err := peer.Decode("12D3KooWAFv4iJst5G6MjwXhZ66K5zS1tP7A9vSg4vK8f1T7X8t9")
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/1234")

	node.authPeers.Store(peerID, time.Now().Add(time.Hour))
	if !acl.AllowReserve(peerID, addr) {
		t.Fatal("expected an admitted peer to hold relay rights")
	}

	node.handleBannedEvent(&api.MeshEvent{
		Type:      api.MeshEvent_BANNED,
		PeerId:    peerID.String(),
		EventTime: timestamppb.Now(),
	})

	if acl.AllowReserve(peerID, addr) {
		t.Error("banned peer still holds relay rights")
	}
}
