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
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/test"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func newRefusedCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_refused_total"}, []string{"reason"})
}

// refusingResourceManager answers every OpenConnection with one error.
type refusingResourceManager struct {
	network.NullResourceManager
	err   error
	limit int
}

func (m *refusingResourceManager) OpenConnection(network.Direction, bool, multiaddr.Multiaddr) (network.ConnManagementScope, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &network.NullScope{}, nil
}

func (m *refusingResourceManager) GetConnLimit() int { return m.limit }

func TestObservedCountsAndClassifiesRefusals(t *testing.T) {
	endpoint := multiaddr.StringCast("/ip4/104.197.44.137/tcp/51234")
	refused := newRefusedCounter()
	quiet := func(string, ...any) {}
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{fmt.Errorf("connections per ip limit exceeded for %s", endpoint), RefusedPerIP},
		{errors.New("rate limit exceeded"), RefusedRateLimit},
		{fmt.Errorf("cannot reserve inbound connection: %w", network.ErrResourceLimitExceeded), RefusedScope},
		{errors.New("something else"), RefusedOther},
	} {
		before := counterValue(t, refused.WithLabelValues(tc.reason))
		m := Observe(&refusingResourceManager{err: tc.err}, 100, refused, quiet)
		if _, err := m.OpenConnection(network.DirInbound, true, endpoint); err == nil {
			t.Fatalf("%s: refusal not returned", tc.reason)
		}
		if got := counterValue(t, refused.WithLabelValues(tc.reason)); got != before+1 {
			t.Errorf("%s: counter %v, want %v", tc.reason, got, before+1)
		}
	}

	// An outbound failure is not an inbound refusal.
	before := counterValue(t, refused.WithLabelValues(RefusedOther))
	m := Observe(&refusingResourceManager{err: errors.New("dial side")}, 100, refused, quiet)
	_, _ = m.OpenConnection(network.DirOutbound, true, endpoint)
	if got := counterValue(t, refused.WithLabelValues(RefusedOther)); got != before {
		t.Errorf("outbound failure counted as an inbound refusal")
	}

	// A granted connection is passed through and not counted.
	ok := Observe(&refusingResourceManager{limit: 42}, 100, refused, quiet)
	if _, err := ok.OpenConnection(network.DirInbound, true, endpoint); err != nil {
		t.Fatalf("granted connection refused: %v", err)
	}
	if ok.GetConnLimit() != 42 {
		t.Errorf("GetConnLimit = %d, want the wrapped manager's 42", ok.GetConnLimit())
	}

	// The QUIC transport passes no address when it could not build one, and
	// a caller may pass no logger; both are counted and nothing more.
	before = counterValue(t, refused.WithLabelValues(RefusedOther))
	bare := Observe(&refusingResourceManager{err: errors.New("no address")}, 100, refused, nil)
	if _, err := bare.OpenConnection(network.DirInbound, true, nil); err == nil {
		t.Fatal("refusal with no endpoint not returned")
	}
	if got := counterValue(t, refused.WithLabelValues(RefusedOther)); got != before+1 {
		t.Errorf("refusal with no endpoint: counter %v, want %v", got, before+1)
	}
	if _, logged := bare.LastLoggedAt("?"); !logged {
		t.Error("refusal with no endpoint not attributed to the unknown source")
	}
}

func TestObservedLogsOneSourceOncePerInterval(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var lines []string
	m := Observe(&refusingResourceManager{err: errors.New("rate limit exceeded")}, 100, newRefusedCounter(),
		func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	m.SetClock(func() time.Time { return now })
	a := multiaddr.StringCast("/ip4/10.0.0.1/tcp/1")
	b := multiaddr.StringCast("/ip4/10.0.0.2/tcp/1")

	for i := 0; i < 3; i++ {
		_, _ = m.OpenConnection(network.DirInbound, true, a)
	}
	_, _ = m.OpenConnection(network.DirInbound, true, b)
	if m.SourcesLogged() != 2 || len(lines) != 2 {
		t.Fatalf("sources remembered = %d, lines = %d; want 2 and 2", m.SourcesLogged(), len(lines))
	}
	if at, _ := m.LastLoggedAt("10.0.0.1"); !at.Equal(now) {
		t.Errorf("first source logged at %v, want the first refusal's time", at)
	}

	// Past the interval the source is logged again and stale ones are dropped.
	now = now.Add(RefusalLogInterval)
	_, _ = m.OpenConnection(network.DirInbound, true, a)
	if at, _ := m.LastLoggedAt("10.0.0.1"); !at.Equal(now) {
		t.Errorf("source not logged again after the interval")
	}
	if _, kept := m.LastLoggedAt("10.0.0.2"); kept {
		t.Errorf("a source silent for the whole interval is still remembered")
	}
	if len(lines) != 3 {
		t.Errorf("lines = %d, want 3", len(lines))
	}
}

// The per-source cap is applied to the address a connection comes from:
// a third direct connection from one address is refused and counted, the
// place comes back when a connection is done, and loopback is exempt. A
// connection that comes through a relay carries the relay's address and is
// not charged to it: a service called by many members through one router
// saw them all as one address. A cap under one would refuse every peer and
// is refused itself.
func TestObservedCapsOneSourceAddressNotItsRelay(t *testing.T) {
	refused := newRefusedCounter()
	if _, err := New(rcmgr.InfiniteLimits, 0, refused, nil); err == nil {
		t.Fatal("a cap of 0 was accepted")
	}
	m, err := New(rcmgr.InfiniteLimits, 2, refused, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	direct := multiaddr.StringCast("/ip4/203.0.113.7/tcp/1")
	open := func(ma multiaddr.Multiaddr) (network.ConnManagementScope, error) {
		return m.OpenConnection(network.DirInbound, true, ma)
	}

	var scopes []network.ConnManagementScope
	for i := 0; i < 2; i++ {
		s, err := open(direct)
		if err != nil {
			t.Fatalf("connection %d from one address refused: %v", i+1, err)
		}
		scopes = append(scopes, s)
	}
	before := counterValue(t, refused.WithLabelValues(RefusedPerIP))
	if _, err := open(direct); err == nil || RefusalReason(err) != RefusedPerIP {
		t.Fatalf("third connection from one address: err = %v, want the per-IP refusal", err)
	}
	if got := counterValue(t, refused.WithLabelValues(RefusedPerIP)); got != before+1 {
		t.Errorf("per-IP refusals counted: %v, want %v", got, before+1)
	}
	if got := m.SourceConns(netip.MustParseAddr("203.0.113.7")); got != 2 {
		t.Errorf("connections held for the address = %d, want 2", got)
	}
	scopes[0].Done()
	scopes[0].Done() // a second Done gives nothing back twice
	if got := m.SourceConns(netip.MustParseAddr("203.0.113.7")); got != 1 {
		t.Errorf("connections held after one is done = %d, want 1", got)
	}
	s, err := open(direct)
	if err != nil {
		t.Fatalf("connection after one was done refused: %v", err)
	}
	s.Done()
	scopes[1].Done()

	// Through a relay: five callers, each more than the cap, and none refused.
	relayed := multiaddr.StringCast("/ip4/203.0.113.7/tcp/1/p2p/" + test.RandPeerIDFatal(t).String() + "/p2p-circuit")
	for i := 0; i < 5; i++ {
		s, err := open(relayed)
		if err != nil {
			t.Fatalf("relayed connection %d refused: %v", i+1, err)
		}
		defer s.Done()
	}
	if got := m.SourceConns(netip.MustParseAddr("203.0.113.7")); got != 0 {
		t.Errorf("relayed connections charged to the relay's address: %d", got)
	}
	for i := 0; i < 5; i++ {
		s, err := open(multiaddr.StringCast("/ip4/127.0.0.1/tcp/1"))
		if err != nil {
			t.Fatalf("loopback connection %d refused: %v", i+1, err)
		}
		defer s.Done()
	}
	// Outbound connections are this host's own; the cap is on what arrives.
	for i := 0; i < 5; i++ {
		s, err := m.OpenConnection(network.DirOutbound, true, direct)
		if err != nil {
			t.Fatalf("outbound connection %d refused: %v", i+1, err)
		}
		defer s.Done()
	}
}

// The per-source rate follows the cap: a burst of twice the cap from one
// address passes, the next connection in the same instant is refused as a
// rate refusal, and the inner manager is never asked for a connection the
// source rules refused.
func TestObservedRateLimitsOneSourceAddress(t *testing.T) {
	refused := newRefusedCounter()
	inner := &countingResourceManager{}
	m := Observe(inner, 4, refused, nil)
	direct := multiaddr.StringCast("/ip4/198.51.100.9/udp/1/quic-v1")
	var opened int
	for i := 0; i < 8; i++ {
		s, err := m.OpenConnection(network.DirInbound, true, direct)
		if err != nil {
			t.Fatalf("connection %d within the burst refused: %v", i+1, err)
		}
		s.Done()
		opened++
	}
	_, err := m.OpenConnection(network.DirInbound, true, direct)
	if err == nil || RefusalReason(err) != RefusedRateLimit {
		t.Fatalf("connection past the burst: err = %v, want the rate refusal", err)
	}
	if inner.opened != opened {
		t.Errorf("inner manager asked for %d connections, want %d: a source refusal must not reach it", inner.opened, opened)
	}
	if got := counterValue(t, refused.WithLabelValues(RefusedRateLimit)); got != 1 {
		t.Errorf("rate refusals counted: %v, want 1", got)
	}
}

// countingResourceManager grants every connection and counts them.
type countingResourceManager struct {
	network.NullResourceManager
	opened int
}

func (m *countingResourceManager) OpenConnection(network.Direction, bool, multiaddr.Multiaddr) (network.ConnManagementScope, error) {
	m.opened++
	return &network.NullScope{}, nil
}

func TestDefaultConnsPerSourceIP(t *testing.T) {
	if got := DefaultConnsPerSourceIP(4000); got != 1000 {
		t.Errorf("4000 -> %d, want 1000", got)
	}
	if got := DefaultConnsPerSourceIP(2); got != 1 {
		t.Errorf("2 -> %d, want the floor of 1", got)
	}
}
