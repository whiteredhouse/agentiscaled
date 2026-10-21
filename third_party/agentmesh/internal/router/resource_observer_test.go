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
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

func TestPerPeerLogLimiter(t *testing.T) {
	var l perPeerLogLimiter
	now := time.Unix(1_700_000_000, 0)
	p, q := peer.ID("peer-a"), peer.ID("peer-b")
	if !l.allow(p, now) || l.allow(p, now.Add(perPeerLogInterval-time.Second)) {
		t.Fatal("first line allowed once, then held for the interval")
	}
	if !l.allow(q, now) {
		t.Fatal("another peer is not held by the first")
	}
	if !l.allow(p, now.Add(perPeerLogInterval)) {
		t.Fatal("the peer is logged again after the interval")
	}
	if _, kept := l.lastAt[q]; kept {
		t.Fatal("a peer silent for the whole interval is still remembered")
	}
}

func TestEffectiveConnsPerSourceIP(t *testing.T) {
	if got := effectiveConnsPerSourceIP(0); got != DefaultHighWaterMark/4 {
		t.Errorf("unset = %d, want a quarter of the default high watermark, %d", got, DefaultHighWaterMark/4)
	}
	if got := effectiveConnsPerSourceIP(64); got != 64 {
		t.Errorf("configured = %d, want 64", got)
	}
}
