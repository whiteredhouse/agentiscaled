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
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Codespaces publishes the target on the https URL GitHub Codespaces assigns
// to a forwarded port, https://<CODESPACE_NAME>-<port>.<forwarding domain>.
// GitHub runs the forwarder, so nothing is started here and the URL is known
// at once. A forwarded port is private to the codespace owner until they
// make it public, and no API inside the codespace can do that; Open returns
// immediately and a background probe reports whether the URL answers from
// the internet, and what to click if it does not.
type Codespaces struct {
	// Getenv reads the platform variables; nil uses os.Getenv.
	Getenv func(string) string
	// Transport performs the probe requests; nil uses the default.
	Transport http.RoundTripper
	// ProbeTimeout bounds how long the probe waits for a first answer;
	// defaults to 90s.
	ProbeTimeout time.Duration
	// ProbeInterval is the pause between probe requests; defaults to 2s.
	ProbeInterval time.Duration
}

const (
	codespaceNameEnv   = "CODESPACE_NAME"
	codespaceDomainEnv = "GITHUB_CODESPACES_PORT_FORWARDING_DOMAIN"
)

// Name implements Provider.
func (c *Codespaces) Name() string { return "codespaces" }

// Open implements Provider.
func (c *Codespaces) Open(ctx context.Context, target string) (Tunnel, error) {
	getenv := c.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	name, domain := getenv(codespaceNameEnv), getenv(codespaceDomainEnv)
	if name == "" || domain == "" {
		return nil, fmt.Errorf("not running in GitHub Codespaces (%s and %s are unset); behind another proxy pass --external-url", codespaceNameEnv, codespaceDomainEnv)
	}
	rawTarget := target
	if !strings.Contains(rawTarget, "://") {
		rawTarget = "http://" + rawTarget
	}
	t, err := url.Parse(rawTarget)
	if err != nil || t.Port() == "" {
		return nil, fmt.Errorf("tunnel target %q has no port", target)
	}
	port := t.Port()
	public := fmt.Sprintf("https://%s-%s.%s", name, port, domain)

	probeCtx, cancel := context.WithCancel(context.Background())
	tun := &static{url: public, cancel: cancel, done: make(chan struct{})}
	go c.watch(probeCtx, public, name, port)
	return tun, nil
}

// watch tells the operator how the public URL answers: at once when the
// port is public, and with the visibility hint when GitHub answers in place
// of the mesh. After the hint it keeps waiting, so flipping the port in the
// PORTS panel is confirmed in the log.
func (c *Codespaces) watch(ctx context.Context, public, name, port string) {
	timeout := c.ProbeTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	switch c.probe(ctx, public, time.Now().Add(timeout), true) {
	case probeReachable:
		logger.Infof("%s answers from the internet; devices can enroll", public)
	case probePrivate:
		logger.Warnf("GitHub answers for %s: port %s is private, so only your own browser can open it. "+
			"To let devices enroll, make it public: PORTS tab -> right-click %s -> Port Visibility -> Public "+
			"(or `gh codespace ports visibility %s:public -c %s`)", public, port, port, port, name)
		if c.probe(ctx, public, time.Time{}, false) == probeReachable {
			logger.Infof("%s answers from the internet; devices can enroll", public)
		}
	case probeTimeout:
		logger.Warnf("%s did not answer within %s; check the PORTS tab lists port %s and forwards it", public, timeout, port)
	}
}

type probeOutcome int

const (
	probeReachable probeOutcome = iota
	probePrivate
	probeTimeout
	probeCancelled
)

// probe requests /healthz on base until the mesh answers 200 through the
// proxy, deadline passes (zero means never) or ctx ends. A redirect or an
// authentication status is GitHub's login gate, reported as probePrivate
// when stopOnPrivate is set and otherwise waited out like any other answer.
func (c *Codespaces) probe(ctx context.Context, base string, deadline time.Time, stopOnPrivate bool) probeOutcome {
	interval := c.ProbeInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	client := &http.Client{
		Transport: c.Transport,
		Timeout:   5 * time.Second,
		// The redirect target is GitHub's login page; following it would
		// report the gate as a healthy answer.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		if err != nil {
			return probeTimeout
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusOK:
				return probeReachable
			case stopOnPrivate && isLoginGate(resp.StatusCode):
				return probePrivate
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return probeTimeout
		}
		select {
		case <-ctx.Done():
			return probeCancelled
		case <-time.After(interval):
		}
	}
}

// isLoginGate reports whether status is what GitHub's proxy returns for a
// private port to a client without a session: a redirect to the login page
// or an authentication failure. Gateway errors mean the mesh is not
// listening yet and are not a verdict on visibility.
func isLoginGate(status int) bool {
	return (status >= 300 && status < 400) || status == http.StatusUnauthorized || status == http.StatusForbidden
}

// static is a Tunnel whose forwarder is run by the platform: it never
// fails on its own and lives until Close.
type static struct {
	url    string
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (s *static) URL() string           { return s.url }
func (s *static) Done() <-chan struct{} { return s.done }
func (s *static) Err() error            { return nil }

func (s *static) Close() error {
	s.once.Do(func() {
		s.cancel()
		close(s.done)
	})
	return nil
}
