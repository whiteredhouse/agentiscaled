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
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	madns "github.com/multiformats/go-multiaddr-dns"
	"golang.org/x/net/dns/dnsmessage"
)

// fqdnName appends a trailing dot, which dnsmessage.NewName requires for a
// fully-qualified name, unless the caller already supplied one.
func fqdnName(name string) string {
	if strings.HasSuffix(name, ".") {
		return name
	}
	return name + "."
}

// tcpFallbackResolver wraps a madns.BasicResolver and retries LookupTXT with
// a direct DNS-over-TCP exchange whenever the wrapped (UDP-based) lookup
// comes back empty. Some networks silently drop or corrupt UDP DNS responses
// once they're large enough to need fragmentation, instead of returning the
// truncated reply a resolver would normally retry over TCP on its own - which
// breaks resolution of libp2p's dnsaddr TXT records (often several entries)
// even though the record itself is fine and a plain TCP query resolves it.
//
// Observed concretely on a ChromeOS Crostini VM: its DNS proxy answered a
// multi-entry dnsaddr TXT record with an empty result over plain UDP, and a
// direct UDP query to 8.8.8.8/1.1.1.1 from the same VM came back partial and
// flagged as a malformed packet - while `dig +tcp` and a second Linux machine
// on the same network resolved it correctly every time. That points at the
// VM's handling of a fragmented/oversized UDP response, not the record or
// the wider network, so retrying over TCP - which needs no fragmentation -
// is the fix rather than anything specific to that one environment.
type tcpFallbackResolver struct {
	def madns.BasicResolver
	// servers overrides the nameservers used for the TCP retry; only set in
	// tests. Production leaves this nil and reads /etc/resolv.conf fresh on
	// every fallback instead, so nameserver changes take effect immediately.
	servers []string
}

var _ madns.BasicResolver = (*tcpFallbackResolver)(nil)

// newTCPFallbackResolver builds a tcpFallbackResolver backed by def. The
// system's configured nameservers are read from /etc/resolv.conf on demand
// for each TCP retry (see LookupTXT) rather than cached here, so the node
// keeps working across VPN connects, Wi-Fi switches, and DHCP renewals
// without needing a restart.
func newTCPFallbackResolver(def madns.BasicResolver) *tcpFallbackResolver {
	return &tcpFallbackResolver{def: def}
}

func (r *tcpFallbackResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return r.def.LookupIPAddr(ctx, host)
}

func (r *tcpFallbackResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	txt, err := r.def.LookupTXT(ctx, name)
	if err == nil && len(txt) > 0 {
		return txt, nil
	}
	// r.servers is a test-only override; production always re-reads
	// /etc/resolv.conf here so nameserver changes take effect immediately.
	servers := r.servers
	if len(servers) == 0 {
		var sysErr error
		servers, sysErr = systemNameservers()
		if sysErr != nil {
			logger.Debugf("dnstcp: no nameservers for TCP fallback: %v", sysErr)
		}
	}
	if len(servers) == 0 {
		return txt, err
	}
	tcpTXT, tcpErr := lookupTXTOverTCP(ctx, name, servers)
	if tcpErr != nil || len(tcpTXT) == 0 {
		logger.Debugf("dnstcp: TCP fallback for TXT %q also failed: %v", name, tcpErr)
		return txt, err
	}
	logger.Debugf("dnstcp: recovered %d TXT record(s) for %q via TCP after an empty UDP result", len(tcpTXT), name)
	return tcpTXT, nil
}

// lookupTXTOverTCP resolves a TXT record with a direct, length-prefixed
// DNS-over-TCP exchange (RFC 1035 section 4.2.2) against servers in order,
// bypassing the standard resolver's UDP-first behavior entirely.
func lookupTXTOverTCP(ctx context.Context, name string, servers []string) ([]string, error) {
	qname, err := dnsmessage.NewName(fqdnName(name))
	if err != nil {
		return nil, fmt.Errorf("invalid DNS name %q: %w", name, err)
	}
	query := dnsmessage.Message{
		Header: dnsmessage.Header{ID: uint16(time.Now().UnixNano()), RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  qname,
			Type:  dnsmessage.TypeTXT,
			Class: dnsmessage.ClassINET,
		}},
	}
	packed, err := query.Pack()
	if err != nil {
		return nil, fmt.Errorf("failed to build DNS query: %w", err)
	}

	var lastErr error
	for _, server := range servers {
		txt, err := exchangeTCP(ctx, server, packed)
		if err == nil {
			return txt, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func exchangeTCP(ctx context.Context, server string, query []byte) ([]string, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", server)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", server, err)
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	// conn.Write/Read below only respect the deadline above, not ctx
	// cancellation directly; close the connection as soon as the caller's
	// context is done so a cancelled/timed-out caller isn't stuck waiting
	// out the full deadline.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(query)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		return nil, fmt.Errorf("writing length prefix to %s: %w", server, err)
	}
	if _, err := conn.Write(query); err != nil {
		return nil, fmt.Errorf("writing query to %s: %w", server, err)
	}

	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("reading response length from %s: %w", server, err)
	}
	resp := make([]byte, binary.BigEndian.Uint16(lenBuf[:]))
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", server, err)
	}

	var msg dnsmessage.Message
	if err := msg.Unpack(resp); err != nil {
		return nil, fmt.Errorf("parsing DNS response from %s: %w", server, err)
	}
	if msg.RCode != dnsmessage.RCodeSuccess {
		return nil, fmt.Errorf("%s returned %s", server, msg.RCode)
	}

	var out []string
	for _, ans := range msg.Answers {
		if txtRes, ok := ans.Body.(*dnsmessage.TXTResource); ok {
			out = append(out, strings.Join(txtRes.TXT, ""))
		}
	}
	return out, nil
}

// resolvConfPath is a package-level var (rather than a hardcoded literal)
// purely so tests can point it at a fixture without touching the real file.
var resolvConfPath = "/etc/resolv.conf"

// systemNameservers reads the "nameserver" entries from /etc/resolv.conf.
// It returns a nil slice, not an error, when the file doesn't exist (e.g. on
// Windows) so callers can silently skip the TCP fallback there.
func systemNameservers() ([]string, error) {
	data, err := os.ReadFile(resolvConfPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseNameservers(string(data)), nil
}

// parseNameservers extracts "host:port" nameserver entries (port 53) from
// the contents of a resolv.conf file.
func parseNameservers(resolvConf string) []string {
	var servers []string
	for _, line := range strings.Split(resolvConf, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		servers = append(servers, net.JoinHostPort(fields[1], "53"))
	}
	return servers
}

func init() {
	r, err := madns.NewResolver(madns.WithDefaultResolver(newTCPFallbackResolver(net.DefaultResolver)))
	if err != nil {
		logger.Warnf("dnstcp: failed to install TCP-fallback DNS resolver, dnsaddr resolution keeps its default UDP-only behavior: %v", err)
		return
	}
	madns.DefaultResolver = r
}
