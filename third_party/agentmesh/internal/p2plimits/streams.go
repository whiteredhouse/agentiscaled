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

package p2plimits

import (
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/prometheus/client_golang/prometheus"
)

// The scopes a stream refusal is counted under. libp2p's own gauge
// (libp2p_rcmgr_blocked_resources) carries the scope but not the protocol,
// and a hub needs the protocol to tell a flood from a limit set too low.
const (
	StreamScopeProtocol     = "protocol"
	StreamScopeProtocolPeer = "protocol_peer"
	StreamScopeService      = "service"
	StreamScopeServicePeer  = "service_peer"
	StreamScopePeer         = "peer"
	// StreamScopeConn is the system or transient scope, where the manager
	// does not yet know the protocol.
	StreamScopeConn = "conn"
)

// StreamRefusals counts the streams the resource manager refused, by scope
// and protocol (or service), and the memory reservations it refused, and
// logs each once per RefusalLogInterval. It is libp2p's MetricsReporter;
// the Allow side is a no-op.
type StreamRefusals struct {
	refused       *prometheus.CounterVec
	memoryRefused prometheus.Counter
	warnf         func(format string, args ...any)
	mu            sync.Mutex
	since         map[string]uint64
	lastLogAt     map[string]time.Time
	now           func() time.Time
}

var _ rcmgr.MetricsReporter = (*StreamRefusals)(nil)

// CountStreamRefusals returns a reporter for rcmgr.WithMetrics. refused
// carries the labels scope and protocol; memoryRefused counts the memory
// reservations refused, which the stream muxer makes one of per stream it
// opens and the relay one of per connection it relays; a nil warnf only
// counts.
func CountStreamRefusals(refused *prometheus.CounterVec, memoryRefused prometheus.Counter, warnf func(format string, args ...any)) *StreamRefusals {
	if warnf == nil {
		warnf = func(string, ...any) {}
	}
	return &StreamRefusals{
		refused:       refused,
		memoryRefused: memoryRefused,
		warnf:         warnf,
		since:         map[string]uint64{},
		lastLogAt:     map[string]time.Time{},
		now:           time.Now,
	}
}

// SetClock replaces the clock the log interval is measured on.
func (s *StreamRefusals) SetClock(now func() time.Time) { s.now = now }

func (s *StreamRefusals) refusedStream(scope, name string) {
	s.refused.WithLabelValues(scope, name).Inc()
	s.report(scope+" "+name, func(n uint64) string {
		what := "at the " + scope + " scope"
		if name != "" {
			what = "on " + name + " " + what
		}
		return fmt.Sprintf("[Stream] Refused %d stream(s) %s since the last report; the peer sees its stream reset", n, what)
	})
}

// BlockMemory is a memory reservation the manager refused: the muxer's
// window for a new stream, or the relay's buffers for a connection it
// would relay. The budget it ran into is libp2p_rcmgr_limit{resource=
// "memory"} on the scope libp2p_rcmgr_memory shows full.
func (s *StreamRefusals) BlockMemory(size int) {
	if s.memoryRefused != nil {
		s.memoryRefused.Inc()
	}
	s.report("memory", func(n uint64) string {
		return fmt.Sprintf("[Memory] Refused %d memory reservation(s) since the last report, the last for %d bytes: the resource manager's memory budget is full (libp2p_rcmgr_memory against libp2p_rcmgr_limit{resource=\"memory\"}); a new stream or a relayed connection that needs it is refused", n, size)
	})
}

// report counts one refusal under key and, once per RefusalLogInterval,
// logs the line for the count since the last report.
func (s *StreamRefusals) report(key string, line func(n uint64) string) {
	s.mu.Lock()
	s.since[key]++
	n := s.since[key]
	now := s.now()
	last, seen := s.lastLogAt[key]
	shouldLog := !seen || now.Sub(last) >= RefusalLogInterval
	if shouldLog {
		s.lastLogAt[key] = now
		s.since[key] = 0
	}
	s.mu.Unlock()
	if shouldLog {
		s.warnf("%s", line(n))
	}
}

func (s *StreamRefusals) BlockStream(_ peer.ID, _ network.Direction) {
	s.refusedStream(StreamScopeConn, "")
}

func (s *StreamRefusals) BlockPeer(peer.ID) { s.refusedStream(StreamScopePeer, "") }

func (s *StreamRefusals) BlockProtocol(proto protocol.ID) {
	s.refusedStream(StreamScopeProtocol, string(proto))
}

func (s *StreamRefusals) BlockProtocolPeer(proto protocol.ID, _ peer.ID) {
	s.refusedStream(StreamScopeProtocolPeer, string(proto))
}

func (s *StreamRefusals) BlockService(svc string) { s.refusedStream(StreamScopeService, svc) }

func (s *StreamRefusals) BlockServicePeer(svc string, _ peer.ID) {
	s.refusedStream(StreamScopeServicePeer, svc)
}

// Connections and memory are observed elsewhere (Observed and libp2p's own
// gauges); allowances are not of interest.
func (s *StreamRefusals) AllowConn(network.Direction, bool)      {}
func (s *StreamRefusals) BlockConn(network.Direction, bool)      {}
func (s *StreamRefusals) AllowStream(peer.ID, network.Direction) {}
func (s *StreamRefusals) AllowPeer(peer.ID)                      {}
func (s *StreamRefusals) AllowProtocol(protocol.ID)              {}
func (s *StreamRefusals) AllowService(string)                    {}
func (s *StreamRefusals) AllowMemory(int)                        {}
