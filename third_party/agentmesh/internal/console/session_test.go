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

package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleSessionDoesNotReturnTheToken is the regression test for the endpoint
// that undid its own defence: the session credential is stored HttpOnly so an
// XSS in the console cannot exfiltrate mesh admin rights, and /auth/session read
// that cookie back and handed the raw value to any same-origin fetch. The SPA
// never needs it, since the reverse proxy injects the cookie as Authorization.
func TestHandleSessionDoesNotReturnTheToken(t *testing.T) {
	const secret = "super-secret-mesh-admin-token"
	srv := &Server{}

	t.Run("no session", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.HandleSession(rec, httptest.NewRequest(http.MethodGet, "/auth/session", nil))

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("active session", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/auth/session", nil)
		req.AddCookie(&http.Cookie{Name: "agentmesh_session", Value: secret})

		rec := httptest.NewRecorder()
		srv.HandleSession(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		body := rec.Body.String()
		if strings.Contains(body, secret) {
			t.Errorf("response body leaks the session token: %s", body)
		}
		if !strings.Contains(body, "authenticated") {
			t.Errorf("response body = %s, want it to report session state", body)
		}
	})
}
