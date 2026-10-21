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
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Backoff is how a caller waits for a control plane that is busy. A 429 or
// 503 is the control plane saying "not now": its enrollment limiter admits
// a fixed number of members per second for the whole mesh, so a fleet that
// joins at once is told to come back, and a member that gave up at the
// first such answer never joined.
type Backoff struct {
	// Initial is the first wait when the answer carries no Retry-After;
	// each further wait doubles, up to Max.
	Initial, Max time.Duration
	// Budget is how long in all the caller keeps trying. The last answer
	// before the budget runs out is returned to the caller as it came.
	// Time, not a count of attempts: a control plane that names a short
	// Retry-After is asking for many short waits, and a fleet of N members
	// behind a limiter of R a second needs N/R seconds whatever it answers.
	Budget time.Duration
}

// DefaultEnrollBackoff keeps trying for three minutes, which admits a
// fleet of about 1500 members that start at once through the control
// plane's limiter of ten a second. A larger fleet starts in waves.
var DefaultEnrollBackoff = Backoff{Initial: time.Second, Max: 15 * time.Second, Budget: 3 * time.Minute}

// DoWithChallengeRetry is DoWithChallenge for a request the control plane
// may answer with 429 or 503. Such an answer is retried after Retry-After
// when the control plane names a wait, and after the backoff's own delay
// when it does not, with jitter so a fleet told "not now" together does
// not come back together. Any other answer, and the last one within the
// budget, is returned to the caller.
func DoWithChallengeRetry(ctx context.Context, httpClient *http.Client, now func() time.Time, b Backoff, build func(ts int64) (*http.Request, error)) (*http.Response, error) {
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(b.Budget)
	delay := b.Initial
	for {
		resp, err := DoWithChallenge(httpClient, now, build)
		if err != nil {
			return nil, err
		}
		if !retryable(resp.StatusCode) {
			return resp, nil
		}
		wait, named := retryAfter(resp, now)
		if !named {
			wait = delay
			delay = min(2*delay, b.Max)
		}
		// Up to half the wait again, at random.
		wait += time.Duration(rand.Int64N(int64(wait)/2 + 1))
		if now().Add(wait).After(deadline) {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxBodyBytes))
		_ = resp.Body.Close()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

// retryAfter reads a Retry-After header in either of its forms. The wait is
// capped at a minute: a header is advice, and a control plane's clock or
// typo must not park a member for an hour.
func retryAfter(resp *http.Response, now func() time.Time) (time.Duration, bool) {
	h := resp.Header.Get("Retry-After")
	if h == "" {
		return 0, false
	}
	const cap = time.Minute
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0, false
		}
		return min(time.Duration(secs)*time.Second, cap), true
	}
	if at, err := http.ParseTime(h); err == nil {
		wait := at.Sub(now())
		if wait < 0 {
			wait = 0
		}
		return min(wait, cap), true
	}
	return 0, false
}
