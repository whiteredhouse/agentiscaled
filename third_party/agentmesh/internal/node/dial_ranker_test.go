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

	"github.com/libp2p/go-libp2p/core/test"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/multiformats/go-multiaddr"
)

// A router offers TCP and QUIC on one port: TCP is dialled at once and
// QUIC only after the head start, so the node holds its hub link over the
// transport without a per-connection stream limit; a peer that offers no
// TCP is dialled with no delay; a WebSocket address counts as TCP; relay
// addresses wait for the direct ones, TCP relays first among them.
func TestTCPFirstDialRanker(t *testing.T) {
	relay := test.RandPeerIDFatal(t).String()
	delays := func(addrs ...string) map[string]time.Duration {
		mas := make([]multiaddr.Multiaddr, 0, len(addrs))
		for _, a := range addrs {
			mas = append(mas, multiaddr.StringCast(a))
		}
		out := map[string]time.Duration{}
		for _, ad := range tcpFirstDialRanker(mas) {
			out[ad.Addr.String()] = ad.Delay
		}
		if len(out) != len(addrs) {
			t.Fatalf("ranked %d of %d addresses", len(out), len(addrs))
		}
		return out
	}
	want := func(got map[string]time.Duration, addr string, d time.Duration) {
		t.Helper()
		if got[addr] != d {
			t.Errorf("%s dialled at %v, want %v", addr, got[addr], d)
		}
	}

	both := delays("/ip4/203.0.113.1/udp/4501/quic-v1", "/ip4/203.0.113.1/tcp/4501", "/ip6/2001:db8::1/tcp/4501", "/ip4/203.0.113.1/udp/4501/quic-v1/webtransport")
	want(both, "/ip4/203.0.113.1/tcp/4501", 0)
	want(both, "/ip6/2001:db8::1/tcp/4501", 0)
	want(both, "/ip4/203.0.113.1/udp/4501/quic-v1", tcpHeadStart)
	want(both, "/ip4/203.0.113.1/udp/4501/quic-v1/webtransport", tcpHeadStart)

	quicOnly := delays("/ip4/203.0.113.1/udp/4501/quic-v1", "/ip6/2001:db8::1/udp/4501/quic-v1")
	want(quicOnly, "/ip4/203.0.113.1/udp/4501/quic-v1", 0)
	want(quicOnly, "/ip6/2001:db8::1/udp/4501/quic-v1", 0)

	ws := delays("/dns4/router.example/tcp/443/wss", "/ip4/203.0.113.1/udp/4501/quic-v1")
	want(ws, "/dns4/router.example/tcp/443/wss", 0)
	want(ws, "/ip4/203.0.113.1/udp/4501/quic-v1", tcpHeadStart)

	relayed := delays(
		"/ip4/198.51.100.7/tcp/5002",
		"/ip4/203.0.113.1/tcp/4501/p2p/"+relay+"/p2p-circuit",
		"/ip4/203.0.113.1/udp/4501/quic-v1/p2p/"+relay+"/p2p-circuit",
		"/p2p/"+relay+"/p2p-circuit",
	)
	want(relayed, "/ip4/198.51.100.7/tcp/5002", 0)
	want(relayed, "/ip4/203.0.113.1/tcp/4501/p2p/"+relay+"/p2p-circuit", swarm.RelayDelay)
	want(relayed, "/ip4/203.0.113.1/udp/4501/quic-v1/p2p/"+relay+"/p2p-circuit", swarm.RelayDelay+tcpHeadStart)
	want(relayed, "/p2p/"+relay+"/p2p-circuit", swarm.RelayDelay+tcpHeadStart)

	// Nothing direct to wait for: the relays are dialled at once.
	relayOnly := delays("/p2p/" + relay + "/p2p-circuit")
	want(relayOnly, "/p2p/"+relay+"/p2p-circuit", 0)
}
