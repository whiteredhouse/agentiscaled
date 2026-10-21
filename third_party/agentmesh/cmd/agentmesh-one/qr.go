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

package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	"github.com/google/agentmesh/api"
	qrcode "github.com/skip2/go-qrcode"
)

// printEnrollQR writes the device onboarding block: the URL the device will
// enroll against, the mesh:// payload for copy-paste, and the same payload
// as a half-block QR code that the Agent Mesh mobile app (or a stock camera app,
// via the mesh:// link) scans. The server must pass the control-plane
// transport rule a device enforces, or the code would scan and then fail.
// maxUsages is how many devices the code admits; the token id lets the
// operator revoke a shared code before it is exhausted.
func printEnrollQR(w io.Writer, server, token string, ttl time.Duration, maxUsages int) error {
	uri := api.EnrollURI(server, token)
	if _, _, err := api.ParseEnrollURI(uri); err != nil {
		return err
	}
	code, err := qrcode.New(uri, qrcode.Medium)
	if err != nil {
		return fmt.Errorf("failed to encode enrollment QR: %w", err)
	}
	host := server
	if u, err := url.Parse(server); err == nil && u.Host != "" {
		host = u.Host
	}
	budget := "single use"
	if maxUsages > 1 {
		budget = fmt.Sprintf("up to %d devices", maxUsages)
	}
	_, _ = fmt.Fprintf(w, "Scan with the Agent Mesh app to enroll a device into %s\n", host)
	_, _ = fmt.Fprintf(w, "(%s, valid for %s):\n\n", budget, ttl.Round(time.Minute))
	// inverseColor=false draws light modules as blocks, so the code reads
	// black-on-white on the dark terminals scanners cope with best.
	_, _ = io.WriteString(w, code.ToSmallString(false))
	_, _ = fmt.Fprintf(w, "\n%s\n", uri)
	// Same derivation as the control plane's token id, so `token list` and
	// `token revoke` line up with what is printed here.
	_, _ = fmt.Fprintf(w, "Token ID: %s (revoke early with: agentmesh-one token revoke %[1]s)\n", tokenID(token)[:12])
	return nil
}

// tokenID mirrors the control plane: the hex SHA-256 of the plaintext.
func tokenID(token string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
}

// stdoutIsTerminal reports whether stdout is an interactive terminal, the
// only place a QR code is worth drawing by default.
func stdoutIsTerminal() bool { return isTerminal(os.Stdout) }

// stdinIsTerminal reports whether a human can answer a prompt.
func stdinIsTerminal() bool { return isTerminal(os.Stdin) }

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
