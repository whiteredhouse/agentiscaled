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
	"context"
	"io"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/multiformats/go-multiaddr"
)

func TestRelayLimit(t *testing.T) {
	unlimitedDuration := math.MaxUint32 * time.Second
	for _, tc := range []struct {
		name     string
		duration time.Duration
		data     int64
		want     *relay.RelayLimit
	}{
		{"no limits", 0, 0, nil},
		{"duration only", time.Hour, 0, &relay.RelayLimit{Duration: time.Hour, Data: math.MaxInt64}},
		{"data only", 0, 4096, &relay.RelayLimit{Duration: unlimitedDuration, Data: 4096}},
		{"both", time.Minute, 4096, &relay.RelayLimit{Duration: time.Minute, Data: 4096}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := relayLimit(tc.duration, tc.data); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("relayLimit(%v, %d) = %+v, want %+v", tc.duration, tc.data, got, tc.want)
			}
		})
	}
}

func TestByteSizeSet(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    ByteSize
		wantErr bool
	}{
		{"128MiB", 128 << 20, false},
		{"1GB", 1_000_000_000, false},
		{"0", 0, false},
		{"abc", 0, true},
		{"20EiB", 0, true},
	} {
		var got ByteSize
		err := got.Set(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("Set(%q) = %d, %v; want %d, error %v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

// A real circuit: the data cap truncates, and a duration-only limit must not cut.
func TestRelayLimitOnCircuit(t *testing.T) {
	const payload = 8192
	for _, tc := range []struct {
		name     string
		limit    *relay.RelayLimit
		complete bool
	}{
		{"data cap cuts the circuit", relayLimit(0, 4096), false},
		{"duration only passes everything", relayLimit(time.Hour, 0), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			relayHost := newLoopbackHost(t, libp2p.DisableRelay())
			if _, err := relay.New(relayHost, relay.WithLimit(tc.limit)); err != nil {
				t.Fatal(err)
			}
			relayInfo := peer.AddrInfo{ID: relayHost.ID(), Addrs: relayHost.Addrs()}

			dst := newLoopbackHost(t, libp2p.EnableRelay())
			dst.SetStreamHandler("/test", func(s network.Stream) {
				_, _ = s.Write(make([]byte, payload))
				_ = s.Close()
			})
			if err := dst.Connect(ctx, relayInfo); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Reserve(ctx, dst, relayInfo); err != nil {
				t.Fatal(err)
			}

			src := newLoopbackHost(t, libp2p.EnableRelay())
			if err := src.Connect(ctx, relayInfo); err != nil {
				t.Fatal(err)
			}
			circuit, err := multiaddr.NewMultiaddr("/p2p/" + relayHost.ID().String() + "/p2p-circuit")
			if err != nil {
				t.Fatal(err)
			}
			if err := src.Connect(ctx, peer.AddrInfo{ID: dst.ID(), Addrs: []multiaddr.Multiaddr{relayHost.Addrs()[0].Encapsulate(circuit)}}); err != nil {
				t.Fatal(err)
			}

			s, err := src.NewStream(network.WithAllowLimitedConn(ctx, "test"), dst.ID(), "/test")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			got, _ := io.Copy(io.Discard, s)
			if (got == payload) != tc.complete {
				t.Errorf("read %d of %d bytes through the relay, want complete=%v", got, payload, tc.complete)
			}
		})
	}
}

func newLoopbackHost(t *testing.T, opts ...libp2p.Option) host.Host {
	t.Helper()
	h, err := libp2p.New(append(opts, libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}
