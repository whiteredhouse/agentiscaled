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

package tlsinspect

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// buildECHConfigList constructs a valid RFC 9180 / draft-ietf-tls-esni-18
// ECHConfigList for crypto/tls so a standard Go tls.Client emits a real
// Encrypted Client Hello (0xfe0d) extension with outerSNI as its cleartext
// public_name and encrypts the inner ServerName.
func buildECHConfigList(t *testing.T, outerSNI string) []byte {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pub := priv.PublicKey().Bytes()

	var contents []byte
	contents = append(contents, 0x01)       // config_id = 1
	contents = append(contents, 0x00, 0x20) // kem_id = DHKEM(X25519, HKDF-SHA256)
	contents = binary.BigEndian.AppendUint16(contents, uint16(len(pub)))
	contents = append(contents, pub...)
	contents = binary.BigEndian.AppendUint16(contents, 4) // cipher_suites length
	contents = append(contents, 0x00, 0x01, 0x00, 0x01)   // HKDF-SHA256 + AES-128-GCM
	contents = append(contents, 0x00)                     // maximum_name_length
	contents = append(contents, byte(len(outerSNI)))      // public_name length
	contents = append(contents, []byte(outerSNI)...)      // public_name (outer cleartext SNI)
	contents = binary.BigEndian.AppendUint16(contents, 0) // extensions length = 0

	var cfg []byte
	cfg = binary.BigEndian.AppendUint16(cfg, ExtEncryptedClientHello) // version = 0xfe0d
	cfg = binary.BigEndian.AppendUint16(cfg, uint16(len(contents)))
	cfg = append(cfg, contents...)

	var list []byte
	list = binary.BigEndian.AppendUint16(list, uint16(len(cfg)))
	list = append(list, cfg...)
	return list
}

// interceptingTransport returns an *http.Transport whose DialContext pipes the
// client connection through VerifyClientHello(expectedDest) before forwarding
// the peeked ClientHello and remaining stream to the target httptest.Server.
func interceptingTransport(targetAddr string, expectedDest string, tlsCfg *tls.Config, onInspect func(sni string, err error)) *http.Transport {
	return &http.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			clientConn, proxyConn := net.Pipe()
			go func() {
				rawRecord, gotSNI, err := VerifyClientHello(proxyConn, expectedDest)
				if onInspect != nil {
					onInspect(gotSNI, err)
				}
				if err != nil {
					_ = proxyConn.Close()
					return
				}
				upstream, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp", targetAddr)
				if dialErr != nil {
					_ = proxyConn.Close()
					return
				}
				if _, writeErr := upstream.Write(rawRecord); writeErr != nil {
					_ = upstream.Close()
					_ = proxyConn.Close()
					return
				}
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					_, _ = io.Copy(upstream, proxyConn)
					_ = upstream.Close()
				}()
				go func() {
					defer wg.Done()
					_, _ = io.Copy(proxyConn, upstream)
					_ = proxyConn.Close()
				}()
				wg.Wait()
			}()
			return clientConn, nil
		},
	}
}

func TestVerifyClientHelloWithTLSServer(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	baseTLSConfig := func() *tls.Config {
		cfg := ts.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		// httptest.NewTLSServer issues a cert for 127.0.0.1 / example.com;
		// skip hostname verification on the client side so we can test custom SNIs
		// while still verifying the server certificate against ts.Certificate().
		cfg.InsecureSkipVerify = true
		return cfg
	}

	t.Run("matching_sni_completes_tls_handshake_and_http_request", func(t *testing.T) {
		var gotSNI string
		var inspectErr error
		tlsCfg := baseTLSConfig()
		tlsCfg.ServerName = "DB.Internal.Example.COM"

		client := &http.Client{
			Transport: interceptingTransport(ts.Listener.Addr().String(), "db.internal.example.com", tlsCfg, func(sni string, err error) {
				gotSNI = sni
				inspectErr = err
			}),
		}
		resp, err := client.Get("https://db.internal.example.com/healthz")
		if err != nil {
			t.Fatalf("client.Get failed: %v (inspectErr=%v)", err, inspectErr)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if inspectErr != nil {
			t.Fatalf("unexpected inspect error: %v", inspectErr)
		}
		if gotSNI != "db.internal.example.com" {
			t.Fatalf("gotSNI = %q, want db.internal.example.com", gotSNI)
		}
	})

	t.Run("mismatched_sni_rejected_before_upstream_handshake", func(t *testing.T) {
		var inspectErr error
		tlsCfg := baseTLSConfig()
		tlsCfg.ServerName = "evil.example.com"

		client := &http.Client{
			Transport: interceptingTransport(ts.Listener.Addr().String(), "db.internal.example.com", tlsCfg, func(_ string, err error) {
				inspectErr = err
			}),
		}
		_, err := client.Get("https://evil.example.com/healthz")
		if err == nil {
			t.Fatal("expected TLS handshake to fail on mismatched SNI")
		}
		if inspectErr == nil || !strings.Contains(inspectErr.Error(), "does not match destination") {
			t.Fatalf("expected SNI mismatch error from VerifyClientHello, got %v", inspectErr)
		}
	})

	t.Run("missing_sni_on_ip_literal_rejected", func(t *testing.T) {
		var inspectErr error
		tlsCfg := baseTLSConfig()
		tlsCfg.ServerName = ""

		client := &http.Client{
			Transport: interceptingTransport(ts.Listener.Addr().String(), "db.internal.example.com", tlsCfg, func(_ string, err error) {
				inspectErr = err
			}),
		}
		// Requesting an IP literal causes Go's crypto/tls to omit the SNI extension.
		_, err := client.Get("https://127.0.0.1/healthz")
		if err == nil {
			t.Fatal("expected TLS handshake without SNI to fail")
		}
		if inspectErr == nil || !strings.Contains(inspectErr.Error(), "missing SNI") {
			t.Fatalf("expected missing SNI error from VerifyClientHello, got %v", inspectErr)
		}
	})

	t.Run("encrypted_client_hello_ech_rejected_even_when_outer_sni_matches", func(t *testing.T) {
		var inspectErr error
		tlsCfg := baseTLSConfig()
		tlsCfg.MinVersion = tls.VersionTLS13
		// Inner secret SNI is evil.example.com, while outer cleartext public_name
		// in ECHConfigList is db.internal.example.com (matching the allowed destination).
		tlsCfg.ServerName = "evil.example.com"
		tlsCfg.EncryptedClientHelloConfigList = buildECHConfigList(t, "db.internal.example.com")

		client := &http.Client{
			Transport: interceptingTransport(ts.Listener.Addr().String(), "db.internal.example.com", tlsCfg, func(_ string, err error) {
				inspectErr = err
			}),
		}
		_, err := client.Get("https://evil.example.com/healthz")
		if err == nil {
			t.Fatal("expected TLS handshake with ECH to be rejected")
		}
		if inspectErr == nil || !strings.Contains(inspectErr.Error(), "Encrypted Client Hello") {
			t.Fatalf("expected ECH rejection error from VerifyClientHello, got %v", inspectErr)
		}
	})

	t.Run("plain_http_non_handshake_record_rejected", func(t *testing.T) {
		var inspectErr error
		client := &http.Client{
			Transport: interceptingTransport(ts.Listener.Addr().String(), "db.internal.example.com", nil, func(_ string, err error) {
				inspectErr = err
			}),
		}
		_, err := client.Get("http://db.internal.example.com/healthz")
		if err == nil {
			t.Fatal("expected plain HTTP request on TLS tunnel to fail")
		}
		if inspectErr == nil || !strings.Contains(inspectErr.Error(), "expected TLS Handshake record") {
			t.Fatalf("expected non-handshake record error from VerifyClientHello, got %v", inspectErr)
		}
	})
}
