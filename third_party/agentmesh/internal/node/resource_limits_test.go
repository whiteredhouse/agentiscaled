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

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/p2plimits"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/connmgr"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	circuit "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/proto"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify"
)

// A provider in a small container must admit as many callers as its
// connection budget says, not as many as libp2p derives from its memory.
func TestNodeLimitsFollowTheConnectionBudget(t *testing.T) {
	cfg := nodeLimits(ConnHighWaterMark).ToPartialLimitConfig()
	want := func(name string, got rcmgr.LimitVal, n int) {
		t.Helper()
		if int(got) != n {
			t.Errorf("%s = %d, want %d", name, got, n)
		}
	}
	want("system inbound conns", cfg.System.ConnsInbound, ConnHighWaterMark)
	want("system conns", cfg.System.Conns, 2*ConnHighWaterMark)
	want("system inbound streams", cfg.System.StreamsInbound, 8*ConnHighWaterMark)
	want("transient inbound conns", cfg.Transient.ConnsInbound, ConnHighWaterMark)
	want("identify service inbound streams", cfg.Service[identify.ServiceName].StreamsInbound, ConnHighWaterMark)
	want("auth protocol inbound streams", cfg.Protocol[api.AuthProtocolID].StreamsInbound, ConnHighWaterMark)
	// A caller holds its MCP, DHT and gossip streams for as long as it stays.
	want("mcp protocol inbound streams", cfg.Protocol[api.MCPProtocolID].StreamsInbound, 8*ConnHighWaterMark)
	want("dht protocol inbound streams", cfg.Protocol[api.DHTProtocolID].StreamsInbound, ConnHighWaterMark)
	for _, p := range pubsub.GossipSubDefaultProtocols {
		want("gossip protocol "+string(p)+" inbound streams", cfg.Protocol[p].StreamsInbound, ConnHighWaterMark)
	}
	// Every caller through one router is a stop stream from that one peer.
	want("relay stop protocol inbound streams", cfg.Protocol[circuit.ProtoIDv2Stop].StreamsInbound, ConnHighWaterMark)
	want("relay stop protocol per-peer inbound streams", cfg.ProtocolPeer[circuit.ProtoIDv2Stop].StreamsInbound, ConnHighWaterMark)
	// A router peer carries one stop stream per relayed caller; yamux
	// reserves one window per open stream, so memory follows the streams.
	want("peer inbound streams", cfg.PeerDefault.StreamsInbound, ConnHighWaterMark)
	if got, min := int64(cfg.System.Memory), int64(cfg.System.Streams)*p2plimits.StreamWindow; got < min {
		t.Errorf("system memory = %d, want at least a window per stream, %d", got, min)
	}
	if got, min := int64(cfg.PeerDefault.Memory), int64(ConnHighWaterMark)*p2plimits.StreamWindow; got < min {
		t.Errorf("peer memory = %d, want at least a window per stop stream, %d", got, min)
	}

	if ConnsPerSourceIP != ConnHighWaterMark/4 {
		t.Errorf("ConnsPerSourceIP = %d, want a quarter of the high watermark, %d", ConnsPerSourceIP, ConnHighWaterMark/4)
	}
	rm, err := newResourceManager()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rm.Close() }()
	// libp2p refuses a host whose connection manager's high watermark is
	// above what its resource manager admits.
	if l := rm.(connmgr.GetConnLimiter).GetConnLimit(); l < ConnHighWaterMark {
		t.Errorf("resource manager admits %d connections, below the high watermark %d", l, ConnHighWaterMark)
	}
}
