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

package api

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ============================================================================
// Device Enrollment URI
// ============================================================================
//
// A control plane hands a new device everything it needs to enroll in one
// scannable string:
//
//	mesh://enroll?server=<control-plane-url>&token=<bootstrap-token>
//
// The token is an ordinary bootstrap token (POST /enroll spends it), so the
// URI grants exactly what the token grants: one enrollment, in the token's
// role, until it expires, against any control plane deployment — agentmesh-one or
// a full agentmesh-control-plane. The server URL is the same base URL a node
// passes to `agentmesh-node join`, and the same transport rule applies
// (ValidateControlPlaneTransport): https, or plaintext http only to a
// loopback host, because whoever answers that URL becomes the device's trust
// root. Clients (the mobile app, CLIs) parse the URI with ParseEnrollURI and
// must reject anything else; there is deliberately no second form to keep
// the scanner-to-enrollment path free of guesswork.

const (
	// EnrollURIScheme is the URI scheme of a device enrollment payload.
	EnrollURIScheme = "mesh"
	// EnrollURIHost is the fixed host component; it names the action.
	EnrollURIHost = "enroll"
)

// EnrollURI builds the device enrollment URI for the given control plane
// base URL and bootstrap token.
func EnrollURI(server, token string) string {
	q := url.Values{}
	q.Set("server", server)
	q.Set("token", token)
	u := url.URL{Scheme: EnrollURIScheme, Host: EnrollURIHost, RawQuery: q.Encode()}
	return u.String()
}

// ParseEnrollURI extracts the control plane URL and bootstrap token from a
// device enrollment URI. The server must be an absolute URL that a device
// may trust as its control plane: https, or http to a loopback host.
func ParseEnrollURI(raw string) (server, token string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("invalid enrollment URI: %w", err)
	}
	if u.Scheme != EnrollURIScheme || u.Host != EnrollURIHost {
		return "", "", fmt.Errorf("invalid enrollment URI: expected %s://%s?server=<url>&token=<token>", EnrollURIScheme, EnrollURIHost)
	}
	q := u.Query()
	server, token = q.Get("server"), q.Get("token")
	if token == "" {
		return "", "", fmt.Errorf("invalid enrollment URI: missing token")
	}
	if su, err := url.Parse(server); err != nil || su.Host == "" {
		return "", "", fmt.Errorf("invalid enrollment URI: server must be an absolute http(s) URL")
	}
	if err := ValidateControlPlaneTransport(server, false); err != nil {
		if errors.Is(err, ErrInsecureControlPlaneURL) {
			// Devices have no --insecure-control-plane escape hatch.
			return "", "", fmt.Errorf("invalid enrollment URI: server %q must use https:// (plaintext http:// is accepted for loopback hosts only)", server)
		}
		return "", "", fmt.Errorf("invalid enrollment URI: %w", err)
	}
	return server, token, nil
}
