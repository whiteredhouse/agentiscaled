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
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/multiformats/go-multiaddr"
)

// tcpHeadStart is how long the other transports wait when a peer offers
// TCP: the head start libp2p's default ranker gives QUIC over TCP, reversed.
// Once a TCP socket is connected the swarm holds further dials for twice
// this while it upgrades, so the others are dialled only when TCP's SYN
// goes unanswered.
const tcpHeadStart = swarm.PublicTCPDelay

// tcpFirstDialRanker orders a peer's addresses so the node ends up on a TCP
// connection when the peer offers one. libp2p's default dials QUIC first,
// and a QUIC connection admits 256 streams from the far end; the node's
// connection to a router is a hub link that carries one stream per peer
// relayed over it, so a service called through a router over QUIC had 256
// callers at most through it, and a caller reaching many peers through one
// router had the same wall. yamux over TCP has no per-connection stream
// limit; the resource manager's budgets are the only ones. The transport
// makes no difference to a connection that carries one session, so the
// rule is applied to every dial rather than to routers alone, which would
// have to be enforced at each place the node learns a router's address.
// Relay addresses wait swarm.RelayDelay after direct ones, as in the
// default. Hole punching does not use the ranker: a simultaneous-connect
// dial tries every address at once.
func tcpFirstDialRanker(addrs []multiaddr.Multiaddr) []network.AddrDelay {
	var direct, relay []multiaddr.Multiaddr
	for _, a := range addrs {
		if hasCircuit(a) {
			relay = append(relay, a)
		} else {
			direct = append(direct, a)
		}
	}
	var relayOffset time.Duration
	if len(direct) > 0 {
		relayOffset = swarm.RelayDelay
	}
	res := make([]network.AddrDelay, 0, len(addrs))
	res = append(res, rankTCPFirst(direct, 0)...)
	return append(res, rankTCPFirst(relay, relayOffset)...)
}

// rankTCPFirst gives the TCP addresses of one group delay offset and the
// rest offset plus tcpHeadStart, or offset alone when there is no TCP to
// wait for.
func rankTCPFirst(addrs []multiaddr.Multiaddr, offset time.Duration) []network.AddrDelay {
	var otherDelay time.Duration
	for _, a := range addrs {
		if isTCPAddr(a) {
			otherDelay = tcpHeadStart
			break
		}
	}
	res := make([]network.AddrDelay, 0, len(addrs))
	for _, a := range addrs {
		if isTCPAddr(a) {
			res = append(res, network.AddrDelay{Addr: a, Delay: offset})
		}
	}
	for _, a := range addrs {
		if !isTCPAddr(a) {
			res = append(res, network.AddrDelay{Addr: a, Delay: offset + otherDelay})
		}
	}
	return res
}

// isTCPAddr is true for an address dialled over TCP: plain, WebSocket or
// through a relay reached over TCP.
func isTCPAddr(a multiaddr.Multiaddr) bool {
	_, err := a.ValueForProtocol(multiaddr.P_TCP)
	return err == nil
}
