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
	"slices"
	"testing"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/p2plimits"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"
	circuit "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/proto"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify"
)

// TestHubLimitsFollowTheConnectionBudget pins the scopes that refused a
// joining fleet on the testnet to the connection budget instead of the
// host's memory: a router on a small VM must admit as many members as its
// watermarks say, and the connection cap sits above the watermark so the
// router sheds members before the manager refuses them.
func TestHubLimitsFollowTheConnectionBudget(t *testing.T) {
	const highWaterMark, conns, circuits = 6000, 1500, 1024
	cfg := hubLimits(highWaterMark, conns, circuits).ToPartialLimitConfig()

	want := func(name string, got rcmgr.LimitVal, n int) {
		t.Helper()
		if int(got) != n {
			t.Errorf("%s = %d, want %d", name, got, n)
		}
	}
	want("system inbound conns", cfg.System.ConnsInbound, connCapHeadroom*highWaterMark)
	want("system conns", cfg.System.Conns, 2*connCapHeadroom*highWaterMark)
	want("system inbound streams", cfg.System.StreamsInbound, 8*connCapHeadroom*highWaterMark)
	want("transient inbound conns", cfg.Transient.ConnsInbound, highWaterMark)
	want("transient inbound streams", cfg.Transient.StreamsInbound, highWaterMark)
	want("identify service inbound streams", cfg.Service[identify.ServiceName].StreamsInbound, connCapHeadroom*highWaterMark)
	want("identify protocol inbound streams", cfg.Protocol[identify.ID].StreamsInbound, connCapHeadroom*highWaterMark)
	want("auth protocol inbound streams", cfg.Protocol[api.AuthProtocolID].StreamsInbound, connCapHeadroom*highWaterMark)
	// Every connection holds one DHT stream and one gossip stream for as
	// long as it lasts, and a router carries connections from members
	// attached elsewhere as well as its own, so these follow the connection
	// cap; libp2p's default capped them at a few hundred in all and the rest
	// of the fleet fell out of discovery.
	want("dht protocol inbound streams", cfg.Protocol[api.DHTProtocolID].StreamsInbound, connCapHeadroom*highWaterMark)
	for _, p := range pubsub.GossipSubDefaultProtocols {
		want("gossip protocol "+string(p)+" inbound streams", cfg.Protocol[p].StreamsInbound, connCapHeadroom*highWaterMark)
	}
	want("relay service inbound streams", cfg.Service[relay.ServiceName].StreamsInbound, highWaterMark)
	// yamux reserves one window per open stream through the manager, so
	// the memory budget follows the stream budget; libp2p's default is
	// scaled from host memory and was full at 1500 members over TCP.
	if got, min := int64(cfg.System.Memory), int64(cfg.System.Streams)*p2plimits.StreamWindow; got < min {
		t.Errorf("system memory = %d, want at least a window per stream, %d", got, min)
	}
	if got, min := int64(cfg.PeerDefault.Memory), int64(circuits)*p2plimits.StreamWindow; got < min {
		t.Errorf("peer memory = %d, want at least a window per circuit, %d", got, min)
	}
	// A service many members call holds one stop stream per caller on the
	// router's peer scope for it.
	want("peer outbound streams", cfg.PeerDefault.StreamsOutbound, circuits)
	want("hop protocol inbound streams", cfg.Protocol[circuit.ProtoIDv2Hop].StreamsInbound, highWaterMark)
	// A destination many members call holds one stop stream per caller.
	want("relay per-peer outbound streams", cfg.ServicePeer[relay.ServiceName].StreamsOutbound, circuits)
	want("stop protocol per-peer outbound streams", cfg.ProtocolPeer[circuit.ProtoIDv2Stop].StreamsOutbound, circuits)

	// A per-address cap above the watermark widens the budget to it; a small
	// one or a small watermark does not shrink the router below the default.
	wide := hubLimits(100, 8000, circuits).ToPartialLimitConfig()
	want("system inbound conns with a wide per-address cap", wide.System.ConnsInbound, connCapHeadroom*8000)
	small := hubLimits(100, 8, circuits).ToPartialLimitConfig()
	want("system inbound conns with a small watermark and cap", small.System.ConnsInbound, connCapHeadroom*DefaultHighWaterMark)
}

// The DHT protocol the limits budget for is the one a DHT built the way the
// router builds it registers; a renamed prefix would silently fall back to
// libp2p's default budget.
func TestDHTProtocolIDIsWhatTheRouterServes(t *testing.T) {
	h, err := mocknet.New().GenPeer()
	if err != nil {
		t.Fatal(err)
	}
	kad, err := dht.New(h, dht.Mode(dht.ModeServer), dht.ProtocolPrefix(api.DHTProtocolPrefix))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = kad.Close() }()
	if got := h.Mux().Protocols(); !slices.Contains(got, api.DHTProtocolID) {
		t.Errorf("DHT server registers %v, not %s", got, api.DHTProtocolID)
	}
	if !slices.Contains(memberProtocols, api.DHTProtocolID) {
		t.Error("the DHT protocol is not among the member protocols the limits budget for")
	}
}
