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

package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestDoWithChallengeRetry covers a control plane that is busy: 429 and 503
// are retried, Retry-After sets the wait when present, other answers are
// returned at once, and a limiter that never relents hands its last answer
// back after the configured attempts.
func TestDoWithChallengeRetry(t *testing.T) {
	fast := Backoff{Initial: time.Millisecond, Max: 5 * time.Millisecond, Budget: 2 * time.Second}
	build := func(url string) func(ts int64) (*http.Request, error) {
		return func(ts int64) (*http.Request, error) {
			return http.NewRequest(http.MethodPost, url, nil)
		}
	}

	t.Run("busy then admitted", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch calls.Add(1) {
			case 1:
				http.Error(w, "busy", http.StatusTooManyRequests)
			case 2:
				http.Error(w, "busy", http.StatusServiceUnavailable)
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer srv.Close()
		resp, err := DoWithChallengeRetry(context.Background(), srv.Client(), nil, fast, build(srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || calls.Load() != 3 {
			t.Fatalf("status %d after %d calls, want 200 after 3", resp.StatusCode, calls.Load())
		}
	})

	t.Run("retry-after is honoured", func(t *testing.T) {
		var calls atomic.Int32
		var gap atomic.Int64
		var first time.Time
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch calls.Add(1) {
			case 1:
				first = time.Now()
				w.Header().Set("Retry-After", "1")
				http.Error(w, "busy", http.StatusTooManyRequests)
			default:
				gap.Store(int64(time.Since(first)))
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer srv.Close()
		resp, err := DoWithChallengeRetry(context.Background(), srv.Client(), nil, fast, build(srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		// At least the named second, at most that plus half again of jitter.
		if got := time.Duration(gap.Load()); got < time.Second || got > 1600*time.Millisecond {
			t.Fatalf("waited %s after Retry-After: 1, want 1s to 1.5s", got)
		}
	})

	t.Run("retry-after as a date, with the default clock", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", time.Now().Add(200*time.Millisecond).UTC().Format(http.TimeFormat))
				http.Error(w, "busy", http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		resp, err := DoWithChallengeRetry(context.Background(), srv.Client(), nil, fast, build(srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || calls.Load() != 2 {
			t.Fatalf("status %d after %d calls, want 200 after 2", resp.StatusCode, calls.Load())
		}
	})

	t.Run("other answers return at once", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Error(w, "no", http.StatusForbidden)
		}))
		defer srv.Close()
		resp, err := DoWithChallengeRetry(context.Background(), srv.Client(), nil, fast, build(srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || calls.Load() != 1 {
			t.Fatalf("status %d after %d calls, want 403 after 1", resp.StatusCode, calls.Load())
		}
	})

	t.Run("gives up when the budget runs out", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Retry-After", "1")
			http.Error(w, "busy", http.StatusTooManyRequests)
		}))
		defer srv.Close()
		// A budget of 2 s and a named wait of 1 s to 1.5 s: one retry fits,
		// a second would end past the budget.
		started := time.Now()
		resp, err := DoWithChallengeRetry(context.Background(), srv.Client(), nil, fast, build(srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests || calls.Load() != 2 {
			t.Fatalf("status %d after %d calls, want 429 after 2", resp.StatusCode, calls.Load())
		}
		if took := time.Since(started); took > fast.Budget {
			t.Fatalf("kept trying for %s, past the %s budget", took, fast.Budget)
		}
	})

	t.Run("cancelled while waiting", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "busy", http.StatusTooManyRequests)
		}))
		defer srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		patient := Backoff{Initial: time.Second, Max: time.Minute, Budget: time.Hour}
		if _, err := DoWithChallengeRetry(ctx, srv.Client(), nil, patient, build(srv.URL)); err != context.DeadlineExceeded {
			t.Fatalf("err = %v, want context deadline", err)
		}
	})
}

func TestRetryAfter(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		header string
		want   time.Duration
		named  bool
	}{
		{"", 0, false},
		{"3", 3 * time.Second, true},
		{"-1", 0, false},
		{"7200", time.Minute, true},
		{now().Add(5 * time.Second).Format(http.TimeFormat), 5 * time.Second, true},
		{now().Add(-5 * time.Second).Format(http.TimeFormat), 0, true},
		{"soon", 0, false},
	} {
		resp := &http.Response{Header: http.Header{}}
		if tc.header != "" {
			resp.Header.Set("Retry-After", tc.header)
		}
		got, named := retryAfter(resp, now)
		if got != tc.want || named != tc.named {
			t.Errorf("Retry-After %q: got %s/%v, want %s/%v", tc.header, got, named, tc.want, tc.named)
		}
	}
}
