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
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestParseNameservers(t *testing.T) {
	const resolvConf = `# comment, ignored
nameserver 127.0.0.53
options edns0 trust-ad
nameserver 2001:db8::1
search example.com
`
	got := parseNameservers(resolvConf)
	want := []string{"127.0.0.53:53", "[2001:db8::1]:53"}
	if len(got) != len(want) {
		t.Fatalf("parseNameservers() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseNameservers()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSystemNameservers_NotCachedAcrossCalls(t *testing.T) {
	oldPath := resolvConfPath
	path := t.TempDir() + "/resolv.conf"
	resolvConfPath = path
	t.Cleanup(func() { resolvConfPath = oldPath })

	if err := os.WriteFile(path, []byte("nameserver 10.0.0.1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := systemNameservers()
	if err != nil {
		t.Fatalf("systemNameservers() error = %v", err)
	}
	if want := []string{"10.0.0.1:53"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("systemNameservers() = %v, want %v", got, want)
	}

	// A network change (VPN, Wi-Fi switch, DHCP renewal) rewrites
	// resolv.conf; the next call must reflect it immediately, not a value
	// cached from construction time.
	if err := os.WriteFile(path, []byte("nameserver 10.0.0.2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err = systemNameservers()
	if err != nil {
		t.Fatalf("systemNameservers() error = %v", err)
	}
	if want := []string{"10.0.0.2:53"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("systemNameservers() after resolv.conf changed = %v, want %v", got, want)
	}
}

// startFakeDNSOverTCP starts a minimal length-prefixed DNS-over-TCP server
// that always answers with the given TXT strings, and returns its address.
func startFakeDNSOverTCP(t *testing.T, txt []string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start fake DNS server: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		var lenBuf [2]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			return
		}
		query := make([]byte, binary.BigEndian.Uint16(lenBuf[:]))
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		var q dnsmessage.Message
		if err := q.Unpack(query); err != nil {
			return
		}

		resp := dnsmessage.Message{
			Header:    dnsmessage.Header{ID: q.ID, Response: true, RCode: dnsmessage.RCodeSuccess},
			Questions: q.Questions,
		}
		for _, s := range txt {
			resp.Answers = append(resp.Answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{
					Name:  q.Questions[0].Name,
					Type:  dnsmessage.TypeTXT,
					Class: dnsmessage.ClassINET,
					TTL:   300,
				},
				Body: &dnsmessage.TXTResource{TXT: []string{s}},
			})
		}
		packed, err := resp.Pack()
		if err != nil {
			return
		}
		var out [2]byte
		binary.BigEndian.PutUint16(out[:], uint16(len(packed)))
		_, _ = conn.Write(out[:])
		_, _ = conn.Write(packed)
	}()

	return ln.Addr().String()
}

func TestLookupTXTOverTCP(t *testing.T) {
	want := []string{"dnsaddr=/ip4/1.2.3.4/tcp/4501/p2p/foo", "dnsaddr=/ip4/1.2.3.4/udp/4501/quic-v1/p2p/foo"}
	server := startFakeDNSOverTCP(t, want)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := lookupTXTOverTCP(ctx, "_dnsaddr.example.com", []string{server})
	if err != nil {
		t.Fatalf("lookupTXTOverTCP failed: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("lookupTXTOverTCP() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("lookupTXTOverTCP()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// stubResolver is a minimal madns.BasicResolver test double.
type stubResolver struct {
	txt    []string
	txtErr error
}

func (s *stubResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return nil, errors.New("not implemented")
}

func (s *stubResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return s.txt, s.txtErr
}

func TestTCPFallbackResolver_FallsBackWhenUDPEmpty(t *testing.T) {
	want := []string{"dnsaddr=/ip4/5.6.7.8/tcp/4501/p2p/bar"}
	server := startFakeDNSOverTCP(t, want)

	r := &tcpFallbackResolver{def: &stubResolver{}, servers: []string{server}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := r.LookupTXT(ctx, "_dnsaddr.example.com")
	if err != nil {
		t.Fatalf("LookupTXT failed: %v", err)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("LookupTXT() = %v, want %v", got, want)
	}
}

func TestTCPFallbackResolver_UsesUDPResultWhenNonEmpty(t *testing.T) {
	want := []string{"dnsaddr=/ip4/9.9.9.9/tcp/4501/p2p/baz"}
	// An unroutable TEST-NET-3 address (RFC 5737): if the wrapper incorrectly
	// attempted the TCP fallback despite a good UDP result, this would hang
	// until the context deadline instead of returning immediately.
	r := &tcpFallbackResolver{def: &stubResolver{txt: want}, servers: []string{"203.0.113.1:53"}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	got, err := r.LookupTXT(ctx, "_dnsaddr.example.com")
	if err != nil {
		t.Fatalf("LookupTXT failed: %v", err)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("LookupTXT() = %v, want %v", got, want)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("LookupTXT() took %s; a good UDP result must short-circuit the TCP fallback", elapsed)
	}
}

func TestTCPFallbackResolver_NoServersReturnsOriginalResult(t *testing.T) {
	// Point at a nonexistent resolv.conf so systemNameservers() finds no
	// servers deterministically, instead of the LookupTXT change (reading
	// /etc/resolv.conf on demand) hitting this machine's real DNS config.
	oldPath := resolvConfPath
	resolvConfPath = t.TempDir() + "/does-not-exist"
	t.Cleanup(func() { resolvConfPath = oldPath })

	origErr := errors.New("boom")
	r := &tcpFallbackResolver{def: &stubResolver{txtErr: origErr}}

	_, err := r.LookupTXT(context.Background(), "_dnsaddr.example.com")
	if !errors.Is(err, origErr) {
		t.Errorf("LookupTXT() error = %v, want %v", err, origErr)
	}
}
