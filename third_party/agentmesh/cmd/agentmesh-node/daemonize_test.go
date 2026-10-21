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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWithoutDaemonizeFlag(t *testing.T) {
	got := withoutDaemonizeFlag([]string{"run", "--daemonize", "--join", "--bind-addr", "127.0.0.1:8080"})
	want := []string{"run", "--join", "--bind-addr", "127.0.0.1:8080"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v, want %v", got, want)
	}

	got = withoutDaemonizeFlag([]string{"run", "--daemonize=true", "--log-level", "debug"})
	want = []string{"run", "--log-level", "debug"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v, want %v", got, want)
	}

	// A value that merely looks like the flag must survive.
	got = withoutDaemonizeFlag([]string{"run", "--log-level", "mode=--daemonize"})
	if len(got) != 3 {
		t.Errorf("flag values must not be stripped: got %v", got)
	}
}

func TestLocalProbeAddr(t *testing.T) {
	for bind, want := range map[string]string{
		"127.0.0.1:8080": "127.0.0.1:8080",
		"0.0.0.0:8080":   "127.0.0.1:8080",
		":8080":          "127.0.0.1:8080",
		"[::]:8080":      "127.0.0.1:8080",
		"nonsense":       "nonsense",
	} {
		if got := localProbeAddr(bind); got != want {
			t.Errorf("localProbeAddr(%q) = %q, want %q", bind, got, want)
		}
	}
}

func TestProbeReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("unexpected probe path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	if !probeReady(addr, time.Second) {
		t.Error("a listening node must probe ready")
	}
	srv.Close()
	if probeReady(addr, 200*time.Millisecond) {
		t.Error("a stopped node must not probe ready")
	}
}

func TestEnsureDaemonToken(t *testing.T) {
	t.Setenv("AGENTMESH_API_TOKEN", "")
	apiTokenPathFlag = ""
	dir := t.TempDir()

	args, path, err := ensureDaemonToken(dir)
	if err != nil {
		t.Fatalf("generating a token: %v", err)
	}
	if path != filepath.Join(dir, daemonTokenFile) {
		t.Errorf("token path: got %q", path)
	}
	if len(args) != 2 || args[0] != "--api-token-path" || args[1] != path {
		t.Errorf("child must receive the token by path, got %v", args)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the token: %v", err)
	}
	if len(first) != 64 {
		t.Errorf("token should be 32 random bytes hex-encoded, got %d chars", len(first))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("token file must not be readable by others: got %v", info.Mode().Perm())
	}

	if _, _, err := ensureDaemonToken(dir); err != nil {
		t.Fatalf("reusing a token: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-reading the token: %v", err)
	}
	if string(second) != string(first) {
		t.Error("an existing token must be reused, not rotated")
	}

	t.Setenv("AGENTMESH_API_TOKEN", "from-env")
	args, path, err = ensureDaemonToken(t.TempDir())
	if err != nil || args != nil || path != "" {
		t.Errorf("a configured AGENTMESH_API_TOKEN must be left alone: got %v, %q, %v", args, path, err)
	}
}

func TestPurgeDataDir(t *testing.T) {
	dir := t.TempDir()
	unrelated := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(unrelated, []byte("keep me"), 0600); err != nil {
		t.Fatalf("seeding unrelated file: %v", err)
	}
	for _, name := range nodeStateFiles() {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("state"), 0600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	removed, err := purgeDataDir(dir)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(removed) != len(nodeStateFiles()) {
		t.Errorf("every state file should be reported as removed, got %v", removed)
	}
	for _, name := range nodeStateFiles() {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should be gone, stat returned %v", name, err)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("purge must not touch unrelated files: %v", err)
	}

	// A second purge is a no-op rather than an error.
	removed, err = purgeDataDir(dir)
	if err != nil {
		t.Fatalf("purging a clean directory: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("a clean directory has nothing to remove, got %v", removed)
	}
}

func TestTailFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentmesh-node.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0600); err != nil {
		t.Fatalf("seeding log: %v", err)
	}
	if got := tailFile(path, 8192, 2); got != "three\nfour" {
		t.Errorf("tailFile should return the last lines, got %q", got)
	}
	if got := tailFile(path, 8192, 10); got != "one\ntwo\nthree\nfour" {
		t.Errorf("a short log should be returned whole, got %q", got)
	}
	if got := tailFile(filepath.Join(t.TempDir(), "missing.log"), 8192, 10); got != "" {
		t.Errorf("a missing log must not fail the report, got %q", got)
	}
}
