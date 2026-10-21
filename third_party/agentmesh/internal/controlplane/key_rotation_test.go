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

package controlplane

import (
	"context"
	"testing"
	"time"
)

func TestKeyRotationCheckInterval(t *testing.T) {
	for _, tc := range []struct {
		interval, want time.Duration
	}{
		{24 * time.Hour, time.Minute},
		{time.Hour, time.Minute},
		{5 * time.Minute, 30 * time.Second},
		{2 * time.Second, 200 * time.Millisecond},
	} {
		if got := keyRotationCheckInterval(tc.interval); got != tc.want {
			t.Errorf("keyRotationCheckInterval(%s) = %s, want %s", tc.interval, got, tc.want)
		}
	}
}

// The deadline is the store's, not the process's: a rotation that was due
// while no replica was up happens on the first check after a start, and a
// replica that restarts before the deadline does not push it back.
func TestRotateKeysIfDueFollowsTheStoredDeadline(t *testing.T) {
	store := newMetricsTestStore(t)
	ctx := context.Background()
	interval := 24 * time.Hour
	issuer, _ := startCustomMockOIDC(t)
	srv, err := NewServer(Options{
		DriverName: "sqlite", DataSourceName: "unused", OIDCIssuer: issuer,
		KeyRotationInterval: interval,
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	if err := srv.Init(); err != nil {
		t.Fatal(err)
	}
	keys := func() int {
		t.Helper()
		ks, err := store.GetAllValidKeys(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(ks)
	}
	if got := keys(); got != 1 {
		t.Fatalf("after Init the keyring holds %d keys, want 1", got)
	}

	// Init scheduled the first rotation an interval from now; the loop it
	// started checks every minute, these checks are direct.
	next, err := store.NextKeyRotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.IsZero() || time.Until(next) < interval-time.Minute {
		t.Fatalf("first rotation scheduled at %s, want about %s from now", next, interval)
	}

	srv.rotateKeysIfDue(time.Now())
	if got := keys(); got != 1 {
		t.Fatalf("a check before the deadline rotated: %d keys", got)
	}

	// Five days later, as after a week of restarts that never reached a tick.
	late := next.Add(5 * 24 * time.Hour)
	srv.rotateKeysIfDue(late)
	if got := keys(); got != 2 {
		t.Fatalf("a check past the deadline did not rotate: %d keys, want 2", got)
	}
	next, err = store.NextKeyRotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Equal(late.Add(interval).Truncate(time.Millisecond)) {
		t.Fatalf("deadline after the late rotation = %s, want %s", next, late.Add(interval))
	}
	srv.rotateKeysIfDue(late.Add(time.Minute))
	if got := keys(); got != 2 {
		t.Fatalf("a check right after a rotation rotated again: %d keys", got)
	}
}
