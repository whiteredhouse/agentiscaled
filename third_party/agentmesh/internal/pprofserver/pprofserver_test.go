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

package pprofserver

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The listener answers the profile index and a heap profile.
func TestServeAnswersProfilesOnItsOwnListener(t *testing.T) {
	srv, err := Serve("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	resp, err := http.Get("http://" + srv.Addr + "/debug/pprof/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "heap") {
		t.Fatalf("index: %d %q", resp.StatusCode, body)
	}
	resp, err = http.Get("http://" + srv.Addr + "/debug/pprof/heap?debug=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "heap profile") {
		t.Fatalf("heap: %d %.80q", resp.StatusCode, body)
	}
}
