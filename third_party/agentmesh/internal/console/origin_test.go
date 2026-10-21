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

func newOriginServer(t *testing.T, externalURL string) *Server {
	t.Helper()
	// Empty /info means no OIDC, which the console tolerates; these tests are
	// about how the origin is derived, not about the provider.
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(controlPlane.Close)

	srv, err := NewServer(Config{
		ControlPlaneURL: controlPlane.URL,
		AdminToken:      "t",
		StaticDir:       t.TempDir(),
		BasePath:        "/console",
		ExternalURL:     externalURL,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// TestExternalURLDecidesOriginAndSecureCookies covers the reason the flag
// exists. Without it both the redirect_uri and the cookie Secure flag come from
// the Host and X-Forwarded-Proto headers, which the client sets: a proxy that
// terminates TLS but drops the header leaves the session cookie without Secure.
func TestExternalURLDecidesOriginAndSecureCookies(t *testing.T) {
	req := func(host, forwardedProto string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/console/auth/login", nil)
		r.Host = host
		if forwardedProto != "" {
			r.Header.Set("X-Forwarded-Proto", forwardedProto)
		}
		return r
	}

	t.Run("configured external url wins over the headers", func(t *testing.T) {
		srv := newOriginServer(t, "https://console.example")

		// An attacker-controlled Host must not reach the redirect_uri.
		got := srv.redirectURI(req("evil.example", ""))
		if want := "https://console.example/console/auth/callback"; got != want {
			t.Errorf("redirectURI = %q, want %q", got, want)
		}
		if !srv.secureCookies(req("evil.example", "")) {
			t.Error("secureCookies = false for an https external url")
		}
	})

	t.Run("a proxy that drops the header no longer downgrades the cookie", func(t *testing.T) {
		srv := newOriginServer(t, "https://console.example")
		if !srv.secureCookies(req("console.example", "")) {
			t.Error("secureCookies = false, want true: the external url says https")
		}
	})

	t.Run("without the flag the request still decides", func(t *testing.T) {
		srv := newOriginServer(t, "")

		got := srv.redirectURI(req("console.example", "https"))
		if want := "https://console.example/console/auth/callback"; got != want {
			t.Errorf("redirectURI = %q, want %q", got, want)
		}
		if srv.secureCookies(req("console.example", "")) {
			t.Error("secureCookies = true for a plain http request")
		}
	})

	t.Run("an http external url does not mark cookies secure", func(t *testing.T) {
		srv := newOriginServer(t, "http://localhost:8081")
		if srv.secureCookies(req("localhost:8081", "https")) {
			t.Error("secureCookies = true, want false: the external url says http")
		}
	})
}

// A redirect_uri must match the provider's registered value byte for byte, so a
// malformed flag has to fail at startup rather than on every login.
func TestNewServerRejectsAMalformedExternalURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"no scheme", "console.example"},
		{"unsupported scheme", "ftp://console.example"},
		{"no host", "https://"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewServer(Config{
				ControlPlaneURL: "http://cp.invalid",
				AdminToken:      "t",
				StaticDir:       t.TempDir(),
				ExternalURL:     tt.url,
			})
			if err == nil {
				t.Fatalf("NewServer accepted ExternalURL %q", tt.url)
			}
			// Checked before the control plane is contacted, so cp.invalid
			// never has to resolve.
			if !strings.Contains(err.Error(), "ExternalURL") {
				t.Errorf("error %q does not name the offending setting", err)
			}
		})
	}
}
