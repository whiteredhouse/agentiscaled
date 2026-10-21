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

// Package p2plimits builds the libp2p resource manager a mesh component
// runs with: limits sized from its connection budget, a per-source-address
// connection cap and rate in place of libp2p's default of 8, applied to the
// address a connection comes from and not to a relay it comes through, and
// a wrapper that counts and logs every inbound connection the manager
// refuses. libp2p refuses a connection before any byte of TLS and, for its
// per-IP and rate-limit paths, without a trace event; a dialer sees the
// connection close or a QUIC CONNECTION_REFUSED, and nothing else records
// it.
package p2plimits

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/network"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	xrate "github.com/libp2p/go-libp2p/x/rate"
	"github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/prometheus/client_golang/prometheus"
)

// Why the resource manager refused an inbound connection; the label values
// of a refusal counter.
const (
	RefusedPerIP     = "per_ip_limit"
	RefusedRateLimit = "rate_limit"
	RefusedScope     = "scope_limit"
	RefusedOther     = "other"
)

// Reasons lists every refusal label, for a counter to pre-register.
var Reasons = []string{RefusedPerIP, RefusedRateLimit, RefusedScope, RefusedOther}

// What Observed answers a direct inbound connection with past the per-source
// cap or rate; the texts are libp2p's own for the same refusals.
var (
	ErrPerSourceConns = errors.New("connections per ip limit exceeded")
	ErrPerSourceRate  = errors.New("rate limit exceeded")
)

// ConnsPerSourceIPShare is the fraction of the inbound connection budget one
// source address may hold when no cap is configured.
const ConnsPerSourceIPShare = 4

// DefaultConnsPerSourceIP is the per-source-address cap derived from a
// connection manager high watermark. Members share addresses (pod SNAT,
// Cloud NAT, offices, a load generator), and the handshake, not the
// address, is the trust boundary.
func DefaultConnsPerSourceIP(highWaterMark int) int {
	return max(highWaterMark/ConnsPerSourceIPShare, 1)
}

// libp2pDefaultConnsPerIP is the cap the resource manager applies when none
// is configured; the per-source rate is scaled from it.
const libp2pDefaultConnsPerIP = 8

// NewResourceManager is libp2p's resource manager with the given limits and
// its own per-address limiters switched off: Observe applies the
// per-source cap and rate, which lets it tell a connection that comes from
// an address apart from one that comes through a relay. opts are passed
// on, for a MetricsReporter among others.
func NewResourceManager(limits rcmgr.ConcreteLimitConfig, opts ...rcmgr.Option) (network.ResourceManager, error) {
	return rcmgr.NewResourceManager(
		rcmgr.NewFixedLimiter(limits),
		append([]rcmgr.Option{
			// Empty, not nil: nil keeps libp2p's default of 8 per address.
			rcmgr.WithLimitPerSubnet([]rcmgr.ConnLimitPerSubnet{}, []rcmgr.ConnLimitPerSubnet{}),
			rcmgr.WithConnRateLimiters(&xrate.Limiter{}),
		}, opts...)...,
	)
}

// New is NewResourceManager wrapped in Observe: the manager a component's
// host runs with. A cap under one would refuse every peer, so it is an
// error here rather than a silent listener.
func New(limits rcmgr.ConcreteLimitConfig, perIP int, refused *prometheus.CounterVec, warnf func(format string, args ...any), opts ...rcmgr.Option) (*Observed, error) {
	if perIP <= 0 {
		return nil, fmt.Errorf("connections per source address must be at least 1, got %d", perIP)
	}
	inner, err := NewResourceManager(limits, opts...)
	if err != nil {
		return nil, err
	}
	return Observe(inner, perIP, refused, warnf), nil
}

// sourceRateLimiter is the per-source connection rate for a cap of perIP:
// libp2p's default rate scaled with the cap, a burst of twice the cap.
func sourceRateLimiter(perIP int) *xrate.Limiter {
	scale := float64(perIP) / libp2pDefaultConnsPerIP
	return &xrate.Limiter{
		SubnetRateLimiter: xrate.SubnetLimiter{
			IPv4SubnetLimits: []xrate.SubnetLimit{
				{PrefixLength: 32, Limit: xrate.Limit{RPS: 0.2 * scale, Burst: 2 * perIP}},
			},
			IPv6SubnetLimits: []xrate.SubnetLimit{
				{PrefixLength: 56, Limit: xrate.Limit{RPS: 0.2 * scale, Burst: 2 * perIP}},
			},
			GracePeriod: time.Minute,
		},
	}
}

// Streams is a stream budget of n each way, with the memory the streams
// reserve: yamux reserves one receive window per open stream through the
// resource manager, so a budget of streams is a budget of memory, and
// libp2p's default for the latter is scaled from host memory, which on the
// testnet's routers was 1.1 GiB and full at 1500 members over TCP. The
// memory is a reservation, released with the stream, and allocated only
// as data arrives and waits to be read.
func Streams(n int) rcmgr.ResourceLimits {
	return rcmgr.ResourceLimits{
		Streams:         rcmgr.LimitVal(2 * n),
		StreamsInbound:  rcmgr.LimitVal(n),
		StreamsOutbound: rcmgr.LimitVal(n),
		Memory:          rcmgr.LimitVal64(int64(2*n) * StreamWindow),
	}
}

// StreamWindow is the receive window yamux reserves for a stream when it
// opens (go-yamux's initialStreamWindow); a stream that carries a fast
// transfer to a slow reader may grow it, up to the muxer's maximum.
const StreamWindow = 256 << 10

// RefusalLogInterval bounds how often one source IP's refusals are logged;
// the counter carries the rate.
const RefusalLogInterval = time.Minute

// Observed applies the per-source cap and rate to the inbound connections
// that come from an address, and counts and logs what it or the wrapped
// manager refuses. A connection that comes through a relay carries the
// relay's address, not its source's: libp2p charged it to the relay, so a
// service called by many members through a router saw them all as one
// address and refused most. Such a connection is bounded by the manager's
// connection budget and its per-peer scope, and at the relay by the
// relay's own per-peer and per-address budgets, so no per-source rule is
// applied to it here. Loopback is exempt, as in libp2p's default, so a
// local mesh never meets the cap. Observed keeps the wrapped manager's
// connection limit visible to libp2p's connmgr sanity check.
type Observed struct {
	network.ResourceManager
	perIP     int
	rate      *xrate.Limiter
	refused   *prometheus.CounterVec
	warnf     func(format string, args ...any)
	mu        sync.Mutex
	conns     map[netip.Addr]int
	lastLogAt map[string]time.Time
	now       func() time.Time
}

// Observe wraps inner with a per-source cap of perIP. refused is counted
// by reason (see Reasons) and warnf written once per source address per
// RefusalLogInterval; a nil warnf only counts.
func Observe(inner network.ResourceManager, perIP int, refused *prometheus.CounterVec, warnf func(format string, args ...any)) *Observed {
	for _, v := range Reasons {
		refused.WithLabelValues(v)
	}
	if warnf == nil {
		warnf = func(string, ...any) {}
	}
	return &Observed{
		ResourceManager: inner,
		perIP:           perIP,
		rate:            sourceRateLimiter(perIP),
		refused:         refused,
		warnf:           warnf,
		conns:           map[netip.Addr]int{},
		lastLogAt:       map[string]time.Time{},
		now:             time.Now,
	}
}

// SetClock replaces the clock the log limiter reads; for tests.
func (m *Observed) SetClock(now func() time.Time) { m.now = now }

// SourcesLogged is how many source addresses the log limiter remembers.
func (m *Observed) SourcesLogged() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.lastLogAt)
}

// LastLoggedAt is when source was last logged, and whether it is remembered.
func (m *Observed) LastLoggedAt(source string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.lastLogAt[source]
	return t, ok
}

func (m *Observed) OpenConnection(dir network.Direction, usefd bool, endpoint multiaddr.Multiaddr) (network.ConnManagementScope, error) {
	src, direct := sourceAddr(endpoint)
	direct = direct && dir == network.DirInbound
	if direct {
		if err := m.reserve(src); err != nil {
			m.refusedConn(endpoint, err)
			return nil, err
		}
	}
	scope, err := m.ResourceManager.OpenConnection(dir, usefd, endpoint)
	if err != nil {
		if direct {
			m.release(src)
		}
		if dir == network.DirInbound {
			m.refusedConn(endpoint, err)
		}
		return nil, err
	}
	if direct {
		return &sourceScope{ConnManagementScope: scope, release: func() { m.release(src) }}, nil
	}
	return scope, nil
}

// SourceConns is how many inbound connections from addr are open.
func (m *Observed) SourceConns(addr netip.Addr) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns[addr]
}

// reserve admits one more connection from src or says why not.
func (m *Observed) reserve(src netip.Addr) error {
	if !m.rate.Allow(src) {
		return ErrPerSourceRate
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conns[src] >= m.perIP {
		return fmt.Errorf("%w for %s", ErrPerSourceConns, src)
	}
	m.conns[src]++
	return nil
}

func (m *Observed) release(src netip.Addr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conns[src] <= 1 {
		delete(m.conns, src)
		return
	}
	m.conns[src]--
}

// sourceAddr is the address a connection comes from, and whether the
// per-source rules apply to it: not to a relayed connection, whose address
// is the relay's, nor to loopback, nor to an endpoint without an address.
func sourceAddr(endpoint multiaddr.Multiaddr) (netip.Addr, bool) {
	if endpoint == nil {
		return netip.Addr{}, false
	}
	for _, c := range endpoint {
		if c.Protocol().Code == multiaddr.P_CIRCUIT {
			return netip.Addr{}, false
		}
	}
	ip, err := manet.ToIP(endpoint)
	if err != nil {
		return netip.Addr{}, false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}
	addr = addr.Unmap()
	if addr.IsLoopback() {
		return netip.Addr{}, false
	}
	return addr, true
}

// sourceScope gives the connection's place at its source address back when
// the connection is done, as the wrapped manager's scope does for its own
// books.
type sourceScope struct {
	network.ConnManagementScope
	once    sync.Once
	release func()
}

func (s *sourceScope) Done() {
	s.ConnManagementScope.Done()
	s.once.Do(s.release)
}

// GetConnLimit is what libp2p compares the connection manager's high water
// mark against at host construction.
func (m *Observed) GetConnLimit() int {
	if l, ok := m.ResourceManager.(connmgr.GetConnLimiter); ok {
		return l.GetConnLimit()
	}
	return 0
}

var _ connmgr.GetConnLimiter = (*Observed)(nil)

func (m *Observed) refusedConn(endpoint multiaddr.Multiaddr, err error) {
	reason := RefusalReason(err)
	m.refused.WithLabelValues(reason).Inc()

	// The QUIC transport passes no address when it could not build one.
	source := "?"
	if endpoint != nil {
		if ip, ipErr := manet.ToIP(endpoint); ipErr == nil {
			source = ip.String()
		}
	}
	m.mu.Lock()
	last, seen := m.lastLogAt[source]
	now := m.now()
	shouldLog := !seen || now.Sub(last) >= RefusalLogInterval
	if shouldLog {
		m.lastLogAt[source] = now
		for k, t := range m.lastLogAt {
			if now.Sub(t) >= RefusalLogInterval {
				delete(m.lastLogAt, k)
			}
		}
	}
	m.mu.Unlock()
	if shouldLog {
		m.warnf("[Conn] Refused inbound connection from %s (%s): %v; the dialer sees the connection close before TLS", source, reason, err)
	}
}

// RefusalReason maps the resource manager's errors to the counter's labels.
// The per-IP and rate-limit paths return plain strings, not sentinel errors.
func RefusalReason(err error) string {
	switch {
	case errors.Is(err, ErrPerSourceConns):
		return RefusedPerIP
	case errors.Is(err, ErrPerSourceRate):
		return RefusedRateLimit
	case errors.Is(err, network.ErrResourceLimitExceeded):
		return RefusedScope
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connections per ip limit"):
		return RefusedPerIP
	case strings.Contains(msg, "rate limit exceeded"):
		return RefusedRateLimit
	}
	return RefusedOther
}
