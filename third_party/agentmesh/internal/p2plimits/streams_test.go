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
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/test"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus"
)

func newStreamsRefusedCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_streams_refused_total"}, []string{"scope", "protocol"})
}

// A stream the manager refuses at a protocol scope is counted under that
// protocol, which libp2p's own gauge does not say; a granted one is not.
func TestStreamRefusalsCountTheProtocol(t *testing.T) {
	const proto protocol.ID = "/test/kad/1.0.0"
	limits := rcmgr.PartialLimitConfig{
		Protocol: map[protocol.ID]rcmgr.ResourceLimits{proto: Streams(1)},
	}.Build(rcmgr.InfiniteLimits)
	refused := newStreamsRefusedCounter()
	m, err := NewResourceManager(limits, rcmgr.WithMetrics(CountStreamRefusals(refused, nil, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	p := test.RandPeerIDFatal(t)
	var scopes []network.StreamManagementScope
	for i := 0; i < 2; i++ {
		s, err := m.OpenStream(p, network.DirInbound)
		if err != nil {
			t.Fatalf("stream %d refused before any protocol: %v", i+1, err)
		}
		scopes = append(scopes, s)
	}
	if err := scopes[0].SetProtocol(proto); err != nil {
		t.Fatalf("first stream refused on %s: %v", proto, err)
	}
	if err := scopes[1].SetProtocol(proto); err == nil {
		t.Fatalf("second stream admitted on %s with a budget of one", proto)
	}
	if got := counterValue(t, refused.WithLabelValues(StreamScopeProtocol, string(proto))); got != 1 {
		t.Errorf("refusals on %s = %v, want 1", proto, got)
	}
	for _, s := range scopes {
		s.Done()
	}
}

func TestStreamRefusalsLogOneProtocolOncePerInterval(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var lines []string
	s := CountStreamRefusals(newStreamsRefusedCounter(), nil, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	s.SetClock(func() time.Time { return now })

	for i := 0; i < 3; i++ {
		s.BlockProtocol("/a")
	}
	s.BlockProtocolPeer("/a", "")
	s.BlockService("relay")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want one per scope and protocol: %q", len(lines), lines)
	}

	// The next line for a protocol carries what was refused since the last.
	now = now.Add(RefusalLogInterval)
	s.BlockProtocol("/a")
	if len(lines) != 4 || lines[3] != "[Stream] Refused 3 stream(s) on /a at the protocol scope since the last report; the peer sees its stream reset" {
		t.Errorf("after the interval: %q", lines[len(lines)-1])
	}
	// A scope with no protocol to name is said without one.
	s.BlockPeer("")
	if lines[len(lines)-1] != "[Stream] Refused 1 stream(s) at the peer scope since the last report; the peer sees its stream reset" {
		t.Errorf("peer scope: %q", lines[len(lines)-1])
	}
}

// A refused memory reservation is counted and said once per interval; the
// muxer makes one per stream it opens, so a full memory budget is a stream
// the peer never gets, with nothing in the stream counters.
func TestStreamRefusalsCountRefusedMemory(t *testing.T) {
	memory := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_memory_refused_total"})
	var lines []string
	now := time.Unix(1_700_000_000, 0)
	s := CountStreamRefusals(newStreamsRefusedCounter(), memory, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	s.SetClock(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		s.BlockMemory(256 << 10)
	}
	if got := counterValue(t, memory); got != 3 {
		t.Errorf("memory refusals counted = %v, want 3", got)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "Refused 1 memory reservation(s)") || !strings.Contains(lines[0], "262144 bytes") {
		t.Fatalf("lines = %q", lines)
	}
	now = now.Add(RefusalLogInterval)
	s.BlockMemory(4096)
	if len(lines) != 2 || !strings.Contains(lines[1], "Refused 3 memory reservation(s)") {
		t.Errorf("after the interval: %q", lines)
	}
}

// The memory a stream budget carries admits the streams it admits: a
// manager built from Streams(n) opens n streams on one yamux-sized
// reservation each, and refuses the reservation past the budget.
func TestStreamsBudgetCarriesTheMuxerWindows(t *testing.T) {
	const n = 4
	limits := rcmgr.PartialLimitConfig{System: Streams(n), Transient: Streams(n), PeerDefault: Streams(n)}.Build(rcmgr.InfiniteLimits)
	m, err := NewResourceManager(limits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	p := test.RandPeerIDFatal(t)
	conn, err := m.OpenConnection(network.DirInbound, true, multiaddr.StringCast("/ip4/203.0.113.9/tcp/1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetPeer(p); err != nil {
		t.Fatal(err)
	}
	// Streams(n) admits 2n streams in all; each takes one window, as yamux
	// reserves it, so the 2n+1-th window is refused.
	for i := 0; i < 2*n; i++ {
		if err := conn.ReserveMemory(StreamWindow, network.ReservationPriorityAlways); err != nil {
			t.Fatalf("window %d refused: %v", i+1, err)
		}
	}
	if err := conn.ReserveMemory(StreamWindow, network.ReservationPriorityAlways); err == nil {
		t.Fatal("a window past the stream budget was admitted")
	}
}
