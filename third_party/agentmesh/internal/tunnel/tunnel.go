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

// Package tunnel publishes a local HTTP listener on a public https URL
// through a third-party connector, so devices that cannot route to the host
// (a phone on cellular, a laptop on another network) can still enroll and
// join the mesh. Providers wrap external programs, or name a forwarder the
// hosting platform already runs; the mesh only learns the resulting URL,
// which it advertises exactly like a configured external URL.
package tunnel

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Tunnel is an open public endpoint forwarding to a local target.
type Tunnel interface {
	// URL is the public https base URL that reaches the local target.
	URL() string
	// Done is closed once the tunnel has stopped forwarding, for whatever
	// reason; Err then reports why (nil after Close).
	Done() <-chan struct{}
	Err() error
	// Close tears the tunnel down.
	Close() error
}

// Provider opens tunnels through one connector implementation.
type Provider interface {
	// Name is the operator-facing identifier (e.g. "cloudflare").
	Name() string
	// Open publishes target, a local http://host:port URL, and returns once
	// the public URL is known or ctx expires.
	Open(ctx context.Context, target string) (Tunnel, error)
}

var providers = map[string]func() Provider{
	"cloudflare": func() Provider { return &Cloudflare{} },
	"codespaces": func() Provider { return &Codespaces{} },
}

// Names lists the registered providers.
func Names() []string {
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Lookup returns a fresh provider by name.
func Lookup(name string) (Provider, error) {
	ctor, ok := providers[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return nil, fmt.Errorf("unknown tunnel provider %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	return ctor(), nil
}
