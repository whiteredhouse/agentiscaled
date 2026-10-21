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

package tunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func codespaceEnv(name, domain string) func(string) string {
	return func(key string) string {
		switch key {
		case codespaceNameEnv:
			return name
		case codespaceDomainEnv:
			return domain
		}
		return ""
	}
}

// rewriteTo sends every request to the test server standing in for GitHub's
// proxy, whatever host the request names.
type rewriteTo string

func (r rewriteTo) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(string(r))
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(req)
}

// scriptedProxy answers /healthz with the given statuses in order and the
// last one forever after, counting requests.
func scriptedProxy(t *testing.T, statuses ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("probe requested %s, want /healthz", r.URL.Path)
		}
		i := int(n.Add(1)) - 1
		if i >= len(statuses) {
			i = len(statuses) - 1
		}
		if statuses[i] >= 300 && statuses[i] < 400 {
			w.Header().Set("Location", "https://github.com/login")
		}
		w.WriteHeader(statuses[i])
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestCodespacesOpenDerivesURLFromPlatformEnv(t *testing.T) {
	c := &Codespaces{
		Getenv:       codespaceEnv("octocat-agentmesh-abc123", "app.github.dev"),
		Transport:    rewriteTo("http://127.0.0.1:0"),
		ProbeTimeout: 10 * time.Millisecond,
	}
	tun, err := c.Open(context.Background(), "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, want := tun.URL(), "https://octocat-agentmesh-abc123-8080.app.github.dev"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
	select {
	case <-tun.Done():
		t.Fatal("Done closed before Close")
	default:
	}
	if err := tun.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tun.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case <-tun.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed after Close")
	}
	if tun.Err() != nil {
		t.Fatalf("Err = %v after Close, want nil", tun.Err())
	}
}

func TestCodespacesOpenRejectsOtherPlatforms(t *testing.T) {
	for name, getenv := range map[string]func(string) string{
		"no env":      codespaceEnv("", ""),
		"name only":   codespaceEnv("octocat-agentmesh-abc123", ""),
		"domain only": codespaceEnv("", "app.github.dev"),
	} {
		t.Run(name, func(t *testing.T) {
			c := &Codespaces{Getenv: getenv}
			if _, err := c.Open(context.Background(), "http://127.0.0.1:8080"); err == nil || !strings.Contains(err.Error(), codespaceNameEnv) {
				t.Fatalf("Open error = %v, want one naming %s", err, codespaceNameEnv)
			}
		})
	}
	c := &Codespaces{Getenv: codespaceEnv("octocat-agentmesh-abc123", "app.github.dev")}
	if _, err := c.Open(context.Background(), "http://127.0.0.1"); err == nil {
		t.Fatal("Open accepted a target without a port")
	}
}

func TestCodespacesProbeWaitsForTheMeshBehindThePublicPort(t *testing.T) {
	// 502 is what the proxy returns while agentmesh-one is still binding.
	proxy, n := scriptedProxy(t, http.StatusBadGateway, http.StatusBadGateway, http.StatusOK)
	c := &Codespaces{Transport: rewriteTo(proxy.URL), ProbeInterval: time.Millisecond}
	if got := c.probe(context.Background(), "https://name-8080.app.github.dev", time.Now().Add(5*time.Second), true); got != probeReachable {
		t.Fatalf("probe = %v, want probeReachable", got)
	}
	if n.Load() != 3 {
		t.Fatalf("probe made %d requests, want 3", n.Load())
	}
}

func TestCodespacesProbeReportsGitHubLoginGateOnce(t *testing.T) {
	proxy, n := scriptedProxy(t, http.StatusFound, http.StatusUnauthorized, http.StatusOK)
	c := &Codespaces{Transport: rewriteTo(proxy.URL), ProbeInterval: time.Millisecond}
	if got := c.probe(context.Background(), "https://name-8080.app.github.dev", time.Now().Add(5*time.Second), true); got != probePrivate {
		t.Fatalf("first probe = %v, want probePrivate", got)
	}
	if n.Load() != 1 {
		t.Fatalf("private verdict took %d requests, want 1", n.Load())
	}
	// Once reported, the gate is waited out until the operator flips the port.
	if got := c.probe(context.Background(), "https://name-8080.app.github.dev", time.Time{}, false); got != probeReachable {
		t.Fatalf("second probe = %v, want probeReachable", got)
	}
	if n.Load() != 3 {
		t.Fatalf("probe made %d requests in total, want 3", n.Load())
	}
}

func TestCodespacesProbeGivesUpAtDeadlineAndOnCancel(t *testing.T) {
	proxy, _ := scriptedProxy(t, http.StatusBadGateway)
	c := &Codespaces{Transport: rewriteTo(proxy.URL), ProbeInterval: time.Millisecond}
	if got := c.probe(context.Background(), "https://name-8080.app.github.dev", time.Now().Add(20*time.Millisecond), true); got != probeTimeout {
		t.Fatalf("probe = %v, want probeTimeout", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := c.probe(ctx, "https://name-8080.app.github.dev", time.Time{}, true); got != probeCancelled {
		t.Fatalf("probe = %v, want probeCancelled", got)
	}
}

func TestLookupKnowsCodespaces(t *testing.T) {
	p, err := Lookup("codespaces")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, ok := p.(*Codespaces); !ok || p.Name() != "codespaces" {
		t.Fatalf("Lookup returned %T named %q", p, p.Name())
	}
}
