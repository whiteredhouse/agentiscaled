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
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The members RunJoin starts are whatever binary it is pointed at. Here that
// is this test binary again, re-entered through TestMain as a node that
// answers /readyz and the two sidecar routes the journey uses on its Unix
// socket, with its behaviour set through the environment the children
// inherit. Like the real node it binds no loopback port unless told to, and
// it refuses to be told: a fleet of a thousand on one host collided with the
// kernel's ephemeral range when every member had one.

func TestMain(m *testing.M) {
	if os.Getenv("AGENTMESH_BENCH_FAKE_NODE") == "1" {
		os.Exit(fakeNode(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeNode(args []string) int {
	var socket, tokenPath string
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--metrics-addr", "--bind-addr":
			if args[i+1] != "" {
				fmt.Fprintln(os.Stderr, "fake node: a member must not bind a loopback port:", args[i], args[i+1])
				return 4
			}
		case "--socket-path":
			socket = args[i+1]
		case "--bootstrap-token-path":
			tokenPath = args[i+1]
		}
	}
	if os.Getenv("FAKE_NODE_EXIT") != "" {
		fmt.Fprintln(os.Stderr, "fake node: refusing to start")
		return 3
	}
	if tokenPath == "" {
		fmt.Fprintln(os.Stderr, "fake node: no --bootstrap-token-path")
		return 2
	}

	readyAfter, _ := time.ParseDuration(os.Getenv("FAKE_NODE_READY_AFTER"))
	downFile := os.Getenv("FAKE_NODE_DOWN_FILE")
	providers := strings.Split(os.Getenv("FAKE_NODE_PROVIDERS"), ",")
	start := time.Now()
	ready := func() bool {
		if time.Since(start) < readyAfter {
			return false
		}
		if downFile != "" {
			if _, err := os.Stat(downFile); err == nil {
				return false
			}
		}
		return true
	}

	sidecar := http.NewServeMux()
	sidecar.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	sidecar.HandleFunc("/mesh/service/discover", func(w http.ResponseWriter, _ *http.Request) {
		var out []map[string]string
		for _, p := range providers {
			if p != "" {
				out = append(out, map[string]string{"peer_id": p})
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	sidecar.HandleFunc("/mesh/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/mesh/dead/") {
			http.Error(w, "no route to peer", http.StatusBadGateway)
			return
		}
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"fake","version":"0"}}}`)
	})

	unix, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake node:", err)
		return 1
	}
	go func() { _ = http.Serve(unix, sidecar) }() //nolint:gosec // a test double
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	<-stop
	return 0
}

func fakeFleet(t *testing.T, count int, env map[string]string) JoinOptions {
	t.Helper()
	t.Setenv("AGENTMESH_BENCH_FAKE_NODE", "1")
	if env == nil {
		env = map[string]string{}
	}
	if _, ok := env["FAKE_NODE_PROVIDERS"]; !ok {
		env["FAKE_NODE_PROVIDERS"] = "alive"
	}
	for _, k := range []string{"FAKE_NODE_EXIT", "FAKE_NODE_READY_AFTER", "FAKE_NODE_DOWN_FILE", "FAKE_NODE_PROVIDERS"} {
		t.Setenv(k, env[k])
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("mesh-bt-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// t.TempDir carries the test's name, which for a subtest is long enough
	// to push a member's socket past the Unix path limit.
	dir, err := os.MkdirTemp("", "agentmesh-join-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return JoinOptions{
		NodeBin:            os.Args[0],
		Count:              count,
		Dir:                filepath.Join(dir, "fleet"),
		ControlPlane:       "http://127.0.0.1:1",
		BootstrapTokenPath: token,
		Service:            "everything",
	}
}

func TestRunJoinTimesEachStageAndRetriesStaleProviders(t *testing.T) {
	// A provider record that names a peer which does not answer is the
	// normal state of a mesh just after a rollout. The journey has to get
	// past it and the report has to say that it happened.
	opts := fakeFleet(t, 3, map[string]string{
		"FAKE_NODE_READY_AFTER": "200ms",
		"FAKE_NODE_PROVIDERS":   "dead,alive",
	})

	report, err := RunJoin(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunJoin: %v", err)
	}
	if report.Ready != 3 || report.Completed != 3 || report.Failed != 0 {
		t.Fatalf("ready/completed/failed = %d/%d/%d, want 3/3/0 (errors %v)", report.Ready, report.Completed, report.Failed, report.Errors)
	}
	if report.ReadyMs.Count != 3 || report.ReadyMs.Min < 200 {
		t.Errorf("ready_ms = %+v, want 3 samples none under the 200ms the node took", report.ReadyMs)
	}
	if report.JourneyMs.Count != 3 || report.FirstCallMs.Count != 3 {
		t.Errorf("journey/first-call counts = %d/%d, want 3/3", report.JourneyMs.Count, report.FirstCallMs.Count)
	}
	if report.CallAttempts != 6 {
		t.Errorf("call_attempts = %d, want 6: the dead provider tried once per member before the live one", report.CallAttempts)
	}
	if len(report.CallErrors) != 1 {
		t.Errorf("call_errors = %v, want the dead provider's refusal once per member", report.CallErrors)
	}
	for msg, n := range report.CallErrors {
		if n != 3 || !strings.Contains(msg, "HTTP 502") {
			t.Errorf("call_errors = %v, want 3 x HTTP 502", report.CallErrors)
		}
	}
	for _, m := range report.Members {
		if m.Provider != "alive" || m.Attempts != 2 || m.Error != "" {
			t.Errorf("member %d = %+v, want provider alive after 2 attempts", m.Index, m)
		}
		if _, err := os.Stat(filepath.Join(m.Dir, "node.log")); err != nil {
			t.Errorf("member %d kept no log: %v", m.Index, err)
		}
	}
	if report.Hold != nil {
		t.Errorf("hold reported without --hold")
	}
}

func TestRunJoinReportsMembersLeftBehindByStage(t *testing.T) {
	// A fleet that mostly failed must not read as a fast one: a member that
	// never got there is counted, by the stage it stopped at, and never
	// contributes a latency.
	t.Run("process exits", func(t *testing.T) {
		opts := fakeFleet(t, 2, map[string]string{"FAKE_NODE_EXIT": "1"})
		report, err := RunJoin(context.Background(), opts)
		if err != nil {
			t.Fatalf("RunJoin: %v", err)
		}
		if report.Failed != 2 || report.Errors[stageExited] != 2 {
			t.Fatalf("failed = %d, errors = %v, want 2 under %q", report.Failed, report.Errors, stageExited)
		}
		if report.ReadyMs.Count != 0 {
			t.Errorf("a member that exited contributed a readiness latency")
		}
		if !strings.Contains(report.Members[0].Error, "exit status 3") {
			t.Errorf("member error = %q, want the exit status", report.Members[0].Error)
		}
	})
	t.Run("never ready", func(t *testing.T) {
		opts := fakeFleet(t, 2, map[string]string{"FAKE_NODE_READY_AFTER": "1h"})
		opts.ReadyTimeout = 300 * time.Millisecond
		report, err := RunJoin(context.Background(), opts)
		if err != nil {
			t.Fatalf("RunJoin: %v", err)
		}
		if report.Failed != 2 || report.Errors[stageReady] != 2 {
			t.Fatalf("failed = %d, errors = %v, want 2 under %q", report.Failed, report.Errors, stageReady)
		}
	})
	t.Run("no provider answers", func(t *testing.T) {
		opts := fakeFleet(t, 1, map[string]string{"FAKE_NODE_PROVIDERS": "dead"})
		opts.CallTimeout = 100 * time.Millisecond
		report, err := RunJoin(context.Background(), opts)
		if err != nil {
			t.Fatalf("RunJoin: %v", err)
		}
		if report.Ready != 1 || report.Completed != 0 || report.Errors[stageCall] != 1 {
			t.Fatalf("ready/completed = %d/%d, errors = %v, want 1/0 under %q", report.Ready, report.Completed, report.Errors, stageCall)
		}
		if report.ReadyMs.Count != 1 || report.JourneyMs.Count != 0 {
			t.Errorf("ready/journey counts = %d/%d: readiness was reached, the journey was not", report.ReadyMs.Count, report.JourneyMs.Count)
		}
	})
}

func TestRunJoinHoldTurnsFlapsIntoOutageWindows(t *testing.T) {
	// What a resident fleet is for: a router that goes away for a while
	// shows up as one outage per member with a length, and a fleet that is
	// whole again at the end says so.
	downFile := filepath.Join(t.TempDir(), "router-down")
	opts := fakeFleet(t, 2, map[string]string{
		"FAKE_NODE_DOWN_FILE": downFile,
	})
	opts.Hold = 2 * time.Second
	opts.SampleInterval = 100 * time.Millisecond
	var residentCompleted int
	var residentHadHold bool
	opts.OnResident = func(r *JoinReport) {
		residentCompleted, residentHadHold = r.Completed, r.Hold != nil
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = os.WriteFile(downFile, nil, 0o600)
			time.Sleep(450 * time.Millisecond)
			_ = os.Remove(downFile)
		}()
	}

	report, err := RunJoin(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunJoin: %v", err)
	}
	if report.Completed != 2 {
		t.Fatalf("completed = %d, want 2 (errors %v)", report.Completed, report.Errors)
	}
	if residentCompleted != 2 || residentHadHold {
		t.Errorf("OnResident saw completed=%d hold=%v, want the journey report before the hold", residentCompleted, residentHadHold)
	}
	h := report.Hold
	if h == nil {
		t.Fatal("no hold report")
	}
	if h.Resident != 2 || h.MinReady != 0 {
		t.Errorf("resident/min_ready = %d/%d, want 2/0", h.Resident, h.MinReady)
	}
	if h.Flaps != 2 || h.Flapped != 2 || h.OutageMs.Count != 2 {
		t.Errorf("flaps/flapped/outages = %d/%d/%d, want 2/2/2", h.Flaps, h.Flapped, h.OutageMs.Count)
	}
	if h.OutageMs.Min < 300 || h.OutageMs.Max > 900 {
		t.Errorf("outage_ms = %+v, want windows around the 400ms the node was away", h.OutageMs)
	}
	if h.NotReadyAtEnd != 0 || h.Exited != 0 {
		t.Errorf("not_ready_at_end/exited = %d/%d, want 0/0", h.NotReadyAtEnd, h.Exited)
	}
	if h.Samples < 10 || len(h.Series) != h.Samples {
		t.Errorf("samples = %d, series = %d, want at least 10 and equal", h.Samples, len(h.Series))
	}
}

func TestRunJoinCancelEndsTheHoldWithAReport(t *testing.T) {
	// Ctrl-C during a long hold is how most holds end. The report is the
	// point of the run and must survive it.
	opts := fakeFleet(t, 1, nil)
	opts.Hold = time.Hour
	opts.SampleInterval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts.OnResident = func(*JoinReport) {
		go func() {
			time.Sleep(250 * time.Millisecond)
			cancel()
		}()
	}
	done := make(chan struct{})
	var report *JoinReport
	var err error
	go func() {
		report, err = RunJoin(ctx, opts)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("RunJoin did not return after cancel")
	}
	if err != nil {
		t.Fatalf("RunJoin: %v", err)
	}
	if report.Hold == nil || report.Hold.Samples == 0 {
		t.Fatalf("hold = %+v, want samples from before the cancel", report.Hold)
	}
	if report.Hold.Seconds > 60 {
		t.Errorf("hold ran %.0fs, want it ended by the cancel", report.Hold.Seconds)
	}
}

func TestRunJoinRefusesADirectoryThatHoldsState(t *testing.T) {
	opts := fakeFleet(t, 1, nil)
	if err := os.MkdirAll(filepath.Join(opts.Dir, "member-0000"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := RunJoin(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("err = %v, want a refusal to reuse member state", err)
	}
}

func TestRunJoinRampSpacesTheStarts(t *testing.T) {
	opts := fakeFleet(t, 3, nil)
	opts.Ramp = 5
	report, err := RunJoin(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunJoin: %v", err)
	}
	if report.Elapsed < 0.4 {
		t.Errorf("elapsed = %.2fs, want at least the 0.4s two gaps at 5/s take", report.Elapsed)
	}
	if report.Completed != 3 {
		t.Errorf("completed = %d, want 3 (errors %v)", report.Completed, report.Errors)
	}
}
