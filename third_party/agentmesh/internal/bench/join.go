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

package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/agentmesh/api"
)

// A density run says how many agents one member can carry. This asks the
// other question: what the Nth member experiences while N-1 are already
// there. The journey measured is the one a new user takes: start a node with
// nothing but a bootstrap token, wait until it holds an authenticated router
// connection, find a service someone else advertised and make one call to it
// through the mesh. Each step is timed per member, so a burst of joiners that
// leaves a few behind is reported as a few left behind rather than as a
// slightly worse mean.
//
// The members are real agentmesh-node processes, one per member, started from this
// process so that every timestamp comes from one clock. They all share this
// host's source address, which is what a classroom, an office or a CI runner
// pool looks like from the router.

// JoinOptions describe one fleet to take through the join journey.
type JoinOptions struct {
	// NodeBin is the agentmesh-node binary; one process of it is started per member.
	NodeBin string

	// Count is how many members to start.
	Count int

	// Dir is the directory under which each member gets its own state,
	// socket and log. It must be empty: a member directory that already holds
	// an identity would resume rather than join.
	Dir string

	// ControlPlane is the URL every member enrolls with.
	ControlPlane string

	// BootstrapTokenPath is the file holding the bootstrap token the members
	// enroll with. It needs at least Count usages left. Empty means the
	// credential comes from NodeArgs instead, such as a --jwt-path.
	BootstrapTokenPath string

	// Service is the MCP service each member looks up and calls once it is
	// ready. Empty ends the journey at readiness.
	Service string

	// Ramp is how many members to start per second. Zero starts them all at
	// once.
	Ramp float64

	// ReadyTimeout bounds enrollment plus router authentication for one
	// member.
	ReadyTimeout time.Duration

	// CallTimeout bounds discovery plus the first successful call for one
	// member, counted from when it became ready.
	CallTimeout time.Duration

	// Hold keeps the fleet resident after the last journey has ended and
	// samples every member's readiness until it elapses or the context is
	// cancelled. Zero tears the fleet down as soon as the journeys end.
	Hold time.Duration

	// SampleInterval is how often each resident member is read during Hold.
	SampleInterval time.Duration

	// NodeArgs are appended to every member's command line as given.
	NodeArgs []string

	// OnResident, if set, is called with the report once the journeys have
	// ended and the hold has begun. The report is complete except for Hold,
	// so a caller can publish the join numbers before a long hold ends.
	OnResident func(*JoinReport)

	// Log receives one line per event: a member's journey ending, a resident
	// member losing or regaining its router. Nil discards them.
	Log io.Writer
}

// JoinReport is the outcome of one fleet's journey.
type JoinReport struct {
	Count   int     `json:"count"`
	Ramp    float64 `json:"ramp_per_second"`
	Service string  `json:"service,omitempty"`

	// Ready counts members that reached /readyz 200. Completed counts those
	// that finished the whole journey, which is readiness alone when there
	// is no Service. Failed is the rest.
	Ready     int `json:"ready"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`

	// Elapsed runs from the first member starting to the last journey ending.
	Elapsed float64 `json:"elapsed_seconds"`

	// ReadyMs is process start to /readyz 200, FirstCallMs is readiness to
	// the first successful call, and JourneyMs is their sum, each over the
	// members that got that far.
	ReadyMs     Distribution `json:"ready_ms"`
	FirstCallMs Distribution `json:"first_call_ms"`
	JourneyMs   Distribution `json:"journey_ms"`

	// CallAttempts is the number of provider calls made across the fleet.
	// More than one per completed member means provider records that named
	// a peer which did not answer; CallErrors says how each of those
	// attempts failed, including on members that then succeeded elsewhere.
	CallAttempts int            `json:"call_attempts"`
	CallErrors   map[string]int `json:"call_errors,omitempty"`

	// Errors counts failures by stage, so a fleet that mostly failed cannot
	// be mistaken for a fast one.
	Errors map[string]int `json:"errors,omitempty"`

	Hold *HoldReport `json:"hold,omitempty"`

	Members []MemberResult `json:"members"`
}

// MemberResult is one member's journey.
type MemberResult struct {
	Index       int     `json:"index"`
	Dir         string  `json:"dir"`
	ReadyMs     float64 `json:"ready_ms,omitempty"`
	FirstCallMs float64 `json:"first_call_ms,omitempty"`
	Provider    string  `json:"provider,omitempty"`
	Attempts    int     `json:"call_attempts,omitempty"`
	Error       string  `json:"error,omitempty"`
}

// HoldReport describes a resident fleet over time.
type HoldReport struct {
	Seconds float64 `json:"seconds"`
	Samples int     `json:"samples"`

	// Resident is how many members were ready at the first sample and
	// MinReady the fewest at any sample.
	Resident int `json:"resident"`
	MinReady int `json:"min_ready"`

	// Flaps counts ready-to-not-ready transitions across the fleet, Flapped
	// the members that had at least one, NotReadyAtEnd those still down when
	// the hold ended and Exited the processes that ended by themselves.
	Flaps         int `json:"flaps"`
	Flapped       int `json:"members_flapped"`
	NotReadyAtEnd int `json:"members_not_ready_at_end"`
	Exited        int `json:"members_exited"`

	// OutageMs is the length of every not-ready window that ended during
	// the hold. A window still open at the end is in NotReadyAtEnd instead.
	OutageMs Distribution `json:"outage_ms"`

	// Series is the ready count at each sample, for lining up against the
	// events an operator caused while the fleet was resident.
	Series []ReadySample `json:"ready_series"`
}

// ReadySample is the fleet's ready count at one instant.
type ReadySample struct {
	At    time.Time `json:"at"`
	Ready int       `json:"ready"`
}

// Journey stages, used as the keys of JoinReport.Errors.
const (
	stageStart    = "start failed"
	stageReady    = "not ready in time"
	stageExited   = "process exited"
	stageCall     = "no provider answered in time"
	stageCanceled = "cancelled"
)

const (
	// Readiness is polled from readyPollMin, backing off to readyPollMax: a
	// member that is up in a few hundred milliseconds is measured as such,
	// and one that takes a minute is not asked a thousand times.
	readyPollMin      = 50 * time.Millisecond
	readyPollMax      = 500 * time.Millisecond
	readyTimeout      = 2 * time.Second
	callRetryInterval = 5 * time.Second
	requestTimeout    = 30 * time.Second
	teardownGrace     = 10 * time.Second
	sampleConcurrency = 64
	// maxBodyProbe bounds how much of a response is read looking for the
	// MCP server's name; an initialize answer is a few hundred bytes.
	maxBodyProbe = 64 << 10
)

// mcpInitialize is the first request of an MCP session, enough for the
// server to name itself.
const mcpInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agentmesh-bench","version":"0"}}}`

type member struct {
	index  int
	dir    string
	socket string
	// client speaks to the member over its Unix socket: readiness, discovery
	// and the first call. A member binds no loopback port, so a fleet of any
	// size cannot collide with the kernel's ephemeral range.
	client  *http.Client
	logPath string
	cmd     *exec.Cmd
	logFile *os.File
	started time.Time
	exited  chan struct{}
	exitErr error

	readyAt    time.Time
	calledAt   time.Time
	provider   string
	attempts   int
	callErrors map[string]int
	stage      string
	detail     string
}

func (m *member) fail(stage, detail string) {
	m.stage, m.detail = stage, detail
}

func (m *member) hasExited() bool {
	if m.exited == nil {
		return true
	}
	select {
	case <-m.exited:
		return true
	default:
		return false
	}
}

func (m *member) result() MemberResult {
	r := MemberResult{Index: m.index, Dir: m.dir, Provider: m.provider, Attempts: m.attempts}
	if !m.readyAt.IsZero() {
		r.ReadyMs = millis(m.readyAt.Sub(m.started))
	}
	if !m.calledAt.IsZero() {
		r.FirstCallMs = millis(m.calledAt.Sub(m.readyAt))
	}
	if m.stage != "" {
		r.Error = m.stage + ": " + m.detail
	}
	return r
}

// RunJoin starts the fleet, takes every member through the journey, holds
// the fleet if asked, tears it down and reports. Cancelling the context ends
// whatever phase is running; the report still describes what happened.
func RunJoin(ctx context.Context, opts JoinOptions) (*JoinReport, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	opts.setDefaults()
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("bench: %w", err)
	}

	members := make([]*member, 0, opts.Count)
	defer func() { teardown(members) }()

	first := time.Now()
	var wg sync.WaitGroup
	for i := range opts.Count {
		if i > 0 && opts.Ramp > 0 {
			select {
			case <-time.After(time.Duration(float64(time.Second) / opts.Ramp)):
			case <-ctx.Done():
			}
		}
		m := newMember(opts, i)
		members = append(members, m)
		if ctx.Err() != nil {
			m.fail(stageCanceled, "before start")
			continue
		}
		if err := m.start(opts); err != nil {
			m.fail(stageStart, err.Error())
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			journey(ctx, opts, m)
		}()
	}
	wg.Wait()

	report := summariseJoin(opts, members, time.Since(first))
	if opts.Hold > 0 && ctx.Err() == nil {
		if opts.OnResident != nil {
			opts.OnResident(report)
		}
		opts.logf("fleet resident: %d of %d ready; holding for %s", report.Ready, report.Count, opts.Hold)
		report.Hold = hold(ctx, opts, members)
	}
	return report, nil
}

func (o *JoinOptions) validate() error {
	switch {
	case o.NodeBin == "":
		return errors.New("bench: no agentmesh-node binary")
	case o.Count <= 0:
		return errors.New("bench: no members to start")
	case o.Dir == "":
		return errors.New("bench: no directory for member state")
	case o.ControlPlane == "":
		return errors.New("bench: no control plane")
	case o.Ramp < 0:
		return errors.New("bench: ramp must not be negative")
	}
	entries, err := os.ReadDir(o.Dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("bench: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("bench: %s is not empty; a member directory that already holds an identity would resume rather than join", o.Dir)
	}
	// A Unix socket path is limited to 108 bytes on Linux; the member's own
	// directory and socket name take some of that.
	if len(filepath.Join(o.Dir, "member-0000", "node.sock")) > 100 {
		return fmt.Errorf("bench: %s is too deep for a member's socket path", o.Dir)
	}
	return nil
}

func (o *JoinOptions) setDefaults() {
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = 180 * time.Second
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = 120 * time.Second
	}
	if o.SampleInterval <= 0 {
		o.SampleInterval = 10 * time.Second
	}
}

func (o *JoinOptions) logf(format string, args ...any) {
	if o.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(o.Log, time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", args...)
}

func newMember(opts JoinOptions, i int) *member {
	dir := filepath.Join(opts.Dir, fmt.Sprintf("member-%04d", i))
	socket := filepath.Join(dir, "node.sock")
	return &member{
		index:   i,
		dir:     dir,
		socket:  socket,
		client:  unixClient(socket),
		logPath: filepath.Join(dir, "node.log"),
	}
}

// start launches the member's agentmesh-node. Its p2p listeners take ephemeral
// ports: the fixed defaults would collide across members, and a member is
// reached through the router's relay regardless of where it listens.
func (m *member) start(opts JoinOptions) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	logFile, err := os.Create(m.logPath)
	if err != nil {
		return err
	}
	args := []string{
		"run",
		"--control-plane", opts.ControlPlane,
		"--config", filepath.Join(m.dir, "agentmesh-node.yaml"),
		"--data-dir", m.dir,
		"--socket-path", m.socket,
		"--bind-addr=",
		"--listen", "/ip4/0.0.0.0/tcp/0",
		"--listen", "/ip4/0.0.0.0/udp/0/quic-v1",
	}
	if opts.BootstrapTokenPath != "" {
		args = append(args, "--bootstrap-token-path", opts.BootstrapTokenPath)
	}
	args = append(args, opts.NodeArgs...)

	cmd := exec.Command(opts.NodeBin, args...) // #nosec G204 -- the operator names the binary and its arguments
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	m.cmd = cmd
	m.logFile = logFile
	m.exited = make(chan struct{})
	m.started = time.Now()
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		m.cmd = nil
		m.exited = nil
		return err
	}
	go func() {
		m.exitErr = cmd.Wait()
		close(m.exited)
	}()
	return nil
}

func journey(ctx context.Context, opts JoinOptions, m *member) {
	if err := m.waitReady(ctx, opts.ReadyTimeout); err != nil {
		opts.logf("member %04d failed: %s: %s", m.index, m.stage, m.detail)
		return
	}
	if opts.Service == "" {
		opts.logf("member %04d ready in %.1fs", m.index, m.readyAt.Sub(m.started).Seconds())
		return
	}
	if err := m.firstCall(ctx, opts); err != nil {
		opts.logf("member %04d ready in %.1fs, then failed: %s: %s", m.index, m.readyAt.Sub(m.started).Seconds(), m.stage, m.detail)
		return
	}
	opts.logf("member %04d ready in %.1fs, called %s via %s after %d attempt(s) in %.1fs",
		m.index, m.readyAt.Sub(m.started).Seconds(), opts.Service, m.provider, m.attempts, m.calledAt.Sub(m.readyAt).Seconds())
}

// waitReady polls the member's /readyz, which answers 200 only once the node
// holds an authenticated connection to a router.
func (m *member) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	interval := readyPollMin
	for {
		if m.isReady(ctx) {
			m.readyAt = time.Now()
			return nil
		}
		if time.Now().After(deadline) {
			m.fail(stageReady, fmt.Sprintf("no /readyz 200 within %s (see %s)", timeout, m.logPath))
			return errors.New(m.stage)
		}
		select {
		case <-ctx.Done():
			m.fail(stageCanceled, "while waiting for readiness")
			return ctx.Err()
		case <-m.exited:
			m.fail(stageExited, fmt.Sprintf("%v (see %s)", m.exitErr, m.logPath))
			return errors.New(m.stage)
		case <-time.After(interval):
		}
		interval = min(interval*3/2, readyPollMax)
	}
}

func (m *member) isReady(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/readyz", nil)
	if err != nil {
		return false
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// firstCall finds the service and speaks MCP to one of its providers through
// the mesh. Every provider is tried and the whole thing retried until the
// deadline: a provider record can name a peer a rollout just replaced, and a
// member that joined seconds ago has a thin routing table.
func (m *member) firstCall(ctx context.Context, opts JoinOptions) error {
	client := m.client
	defer client.CloseIdleConnections()
	deadline := m.readyAt.Add(opts.CallTimeout)
	lastErr := "none discovered"
	for {
		providers, err := discover(ctx, client, opts.Service)
		if err != nil {
			lastErr = err.Error()
		}
		for _, p := range providers {
			m.attempts++
			if err := initialize(ctx, client, p, opts.Service); err != nil {
				lastErr = p + ": " + err.Error()
				if m.callErrors == nil {
					m.callErrors = map[string]int{}
				}
				m.callErrors[err.Error()]++
				continue
			}
			m.calledAt = time.Now()
			m.provider = p
			return nil
		}
		if time.Now().After(deadline) {
			m.fail(stageCall, fmt.Sprintf("%s after %d attempt(s) within %s; last: %s", opts.Service, m.attempts, opts.CallTimeout, lastErr))
			return errors.New(m.stage)
		}
		select {
		case <-ctx.Done():
			m.fail(stageCanceled, "while calling "+opts.Service)
			return ctx.Err()
		case <-m.exited:
			m.fail(stageExited, fmt.Sprintf("%v (see %s)", m.exitErr, m.logPath))
			return errors.New(m.stage)
		case <-time.After(callRetryInterval):
		}
	}
}

func unixClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

func discover(ctx context.Context, client *http.Client, service string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	url := "http://localhost/mesh/service/discover?type=mcp&name=" + service + "&timeout=20s"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discover: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyProbe))
	if err != nil {
		return nil, fmt.Errorf("discover: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discover: HTTP %d: %s", resp.StatusCode, head(body))
	}
	var providers []*api.DiscoveredProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, fmt.Errorf("discover: %w", err)
	}
	peers := make([]string, 0, len(providers))
	for _, p := range providers {
		if p.GetPeerId() != "" {
			peers = append(peers, p.GetPeerId())
		}
	}
	return peers, nil
}

// initialize opens an MCP session with one provider through the mesh. The
// answer is JSON or an SSE frame; either names the server, and the body is
// read only as far as that name. A JSON-RPC error arrives as HTTP 200 and is
// reported by its message.
func initialize(ctx context.Context, client *http.Client, peer, service string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	url := "http://localhost/mesh/" + peer + "/mcp/" + service
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(mcpInitialize))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	var body []byte
	chunk := make([]byte, 4096)
	for len(body) < maxBodyProbe {
		n, err := resp.Body.Read(chunk)
		body = append(body, chunk[:n]...)
		if resp.StatusCode == http.StatusOK && bytes.Contains(body, []byte(`"serverInfo"`)) {
			return nil
		}
		if err != nil {
			break
		}
	}
	if msg := rpcErrorMessage(body); msg != "" {
		return fmt.Errorf("HTTP %d, JSON-RPC error: %s", resp.StatusCode, msg)
	}
	return fmt.Errorf("HTTP %d without serverInfo: %s", resp.StatusCode, head(body))
}

// rpcErrorMessage extracts the message of a JSON-RPC error response, or the
// empty string when body is not one.
func rpcErrorMessage(body []byte) string {
	var rpc struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(body), &rpc) != nil || rpc.Error == nil {
		return ""
	}
	return rpc.Error.Message
}

func head(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}

func summariseJoin(opts JoinOptions, members []*member, elapsed time.Duration) *JoinReport {
	report := &JoinReport{
		Count:      opts.Count,
		Ramp:       opts.Ramp,
		Service:    opts.Service,
		Elapsed:    elapsed.Seconds(),
		Errors:     map[string]int{},
		CallErrors: map[string]int{},
		Members:    make([]MemberResult, 0, len(members)),
	}
	var ready, call, journey []time.Duration
	for _, m := range members {
		report.Members = append(report.Members, m.result())
		report.CallAttempts += m.attempts
		for msg, n := range m.callErrors {
			report.CallErrors[msg] += n
		}
		if !m.readyAt.IsZero() {
			report.Ready++
			ready = append(ready, m.readyAt.Sub(m.started))
		}
		if !m.calledAt.IsZero() {
			call = append(call, m.calledAt.Sub(m.readyAt))
			journey = append(journey, m.calledAt.Sub(m.started))
		}
		switch {
		case m.stage != "":
			report.Failed++
			report.Errors[m.stage]++
		default:
			report.Completed++
		}
	}
	report.ReadyMs = summarise(ready)
	report.FirstCallMs = summarise(call)
	report.JourneyMs = summarise(journey)
	return report
}

// hold samples every member's readiness until the hold elapses or the
// context is cancelled, and turns the transitions into outage windows.
func hold(ctx context.Context, opts JoinOptions, members []*member) *HoldReport {
	type state struct {
		ready     bool
		downSince time.Time
		flapped   bool
	}
	states := make([]state, len(members))
	report := &HoldReport{}
	var outages []time.Duration

	start := time.Now()
	end := start.Add(opts.Hold)
	for {
		now := time.Now()
		ready := sampleReady(ctx, members)
		count := 0
		for i, m := range members {
			if ready[i] {
				count++
			}
			s := &states[i]
			switch {
			case report.Samples == 0:
				s.ready = ready[i]
				if !ready[i] {
					s.downSince = now
				}
			case s.ready && !ready[i]:
				s.ready = false
				s.downSince = now
				s.flapped = true
				report.Flaps++
				opts.logf("member %04d lost its router", m.index)
			case !s.ready && ready[i]:
				s.ready = true
				outages = append(outages, now.Sub(s.downSince))
				opts.logf("member %04d back after %.1fs", m.index, now.Sub(s.downSince).Seconds())
			}
		}
		report.Series = append(report.Series, ReadySample{At: now, Ready: count})
		if report.Samples == 0 {
			report.Resident = count
			report.MinReady = count
		} else if count < report.MinReady {
			report.MinReady = count
		}
		report.Samples++

		if now.After(end) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(opts.SampleInterval):
		}
		if ctx.Err() != nil {
			break
		}
	}

	report.Seconds = time.Since(start).Seconds()
	report.OutageMs = summarise(outages)
	for i, m := range members {
		if states[i].flapped {
			report.Flapped++
		}
		if !states[i].ready {
			report.NotReadyAtEnd++
		}
		if m.cmd != nil && m.hasExited() {
			report.Exited++
		}
	}
	return report
}

// sampleReady reads every member's /readyz, a bounded number at a time.
func sampleReady(ctx context.Context, members []*member) []bool {
	ready := make([]bool, len(members))
	sem := make(chan struct{}, sampleConcurrency)
	var wg sync.WaitGroup
	for i, m := range members {
		if m.cmd == nil || m.hasExited() {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			ready[i] = m.isReady(ctx)
		}()
	}
	wg.Wait()
	return ready
}

// teardown stops every member: a SIGTERM, then a SIGKILL for whatever is
// still running when the grace period ends.
func teardown(members []*member) {
	for _, m := range members {
		if m.cmd != nil && !m.hasExited() {
			_ = m.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	deadline := time.Now().Add(teardownGrace)
	for _, m := range members {
		if m.cmd == nil {
			continue
		}
		select {
		case <-m.exited:
		case <-time.After(time.Until(deadline)):
			_ = m.cmd.Process.Kill()
			<-m.exited
		}
		_ = m.logFile.Close()
	}
}

func millis(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e6
}
