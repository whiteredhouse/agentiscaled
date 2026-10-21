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
	"slices"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/test"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/multiformats/go-multiaddr"
)

// A peer reached only through a router this node does not hold: the
// directory names the circuit through that router, and the node handshakes
// with it before dialing, since a relay opens a circuit only for a source it
// admitted. The address the lookup learned is kept, and nothing is left in
// dial backoff.
func TestPreparePeerAddrsUsesWhatAFailedLookupLearned(t *testing.T) {
	held := newFakeRouter(t, "/ip4/127.0.0.1/tcp/0")
	// The held router answers the node's directory requests, as a router does.
	serveDirectory(held.h)
	other := newFakeRouterWithKey(t, held.cpPub, held.cpPriv, "/ip4/127.0.0.1/tcp/0")

	// The peer reserved with the other router, which recorded the circuit;
	// the held router learned it, as a router does through identify.
	target := test.RandPeerIDFatal(t)
	circuit := other.routerAddrs[0].Encapsulate(multiaddr.StringCast("/p2p-circuit"))
	held.h.Peerstore().AddAddrs(target, []multiaddr.Multiaddr{circuit}, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	node := held.startNode(t, ctx, held.routerAddrs)
	before := counterValue(t, peerLookupsTotal.WithLabelValues(lookupFound))

	node.preparePeerAddrs(ctx, target)

	if got := other.handshakes.Load(); got != 1 {
		t.Fatalf("the other router saw %d handshakes, want 1: the node must handshake with the router the lookup named", got)
	}
	var got []string
	for _, a := range node.Host.Peerstore().Addrs(target) {
		got = append(got, a.String())
	}
	slices.Sort(got)
	want := []string{circuit.String(), "/p2p/" + other.h.ID().String() + "/p2p-circuit"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("addresses kept for the peer: %v, want the circuit the lookup learned and its bare form %v", got, want)
	}
	if after := counterValue(t, peerLookupsTotal.WithLabelValues(lookupFound)); after != before+1 {
		t.Errorf("lookups counted as found: %v, want %v", after, before+1)
	}
	sw, ok := node.Host.Network().(*swarm.Swarm)
	if !ok {
		t.Fatal("the node's network is not a swarm")
	}
	if sw.Backoff().Backoff(target, circuit) {
		t.Error("the circuit the lookup's refused dial put in backoff is still there after the handshake")
	}
}
