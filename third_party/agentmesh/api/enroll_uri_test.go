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

import "testing"

func TestEnrollURIRoundTrip(t *testing.T) {
	const server, token = "https://abc-def-123.trycloudflare.com", "mesh_dev_0123456789abcdef"
	uri := EnrollURI(server, token)
	if want := "mesh://enroll?server=https%3A%2F%2Fabc-def-123.trycloudflare.com&token=mesh_dev_0123456789abcdef"; uri != want {
		t.Fatalf("EnrollURI = %q, want %q", uri, want)
	}
	gotServer, gotToken, err := ParseEnrollURI(" " + uri + "\n")
	if err != nil {
		t.Fatalf("ParseEnrollURI: %v", err)
	}
	if gotServer != server || gotToken != token {
		t.Fatalf("ParseEnrollURI = (%q, %q), want (%q, %q)", gotServer, gotToken, server, token)
	}
}

func TestParseEnrollURIAcceptsLoopbackHTTP(t *testing.T) {
	// Emulators reach the host over adb reverse, i.e. loopback.
	server, _, err := ParseEnrollURI("mesh://enroll?server=http%3A%2F%2F127.0.0.1%3A18432&token=t")
	if err != nil || server != "http://127.0.0.1:18432" {
		t.Fatalf("ParseEnrollURI loopback http = (%q, %v)", server, err)
	}
}

func TestParseEnrollURIRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"wrong scheme":      "samone://enroll?server=https%3A%2F%2Fx&token=t",
		"wrong host":        "mesh://join?server=https%3A%2F%2Fx&token=t",
		"missing token":     "mesh://enroll?server=https%3A%2F%2Fx",
		"missing server":    "mesh://enroll?token=t",
		"non-http server":   "mesh://enroll?server=ftp%3A%2F%2Fx&token=t",
		"relative server":   "mesh://enroll?server=x.example.com&token=t",
		"plaintext to LAN":  "mesh://enroll?server=http%3A%2F%2F192.168.1.50%3A18432&token=t",
		"plaintext to host": "mesh://enroll?server=http%3A%2F%2Fmesh.example.com&token=t",
		"plain token only":  "mesh_dev_0123",
	} {
		if _, _, err := ParseEnrollURI(raw); err == nil {
			t.Errorf("%s: ParseEnrollURI(%q) accepted", name, raw)
		}
	}
}
