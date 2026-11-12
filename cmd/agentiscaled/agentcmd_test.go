package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithClients(t *testing.T) {
	all := strings.Join(systemClients(), ",")
	for _, tc := range []struct {
		saved string
		names []string
		add   bool
		want  string
	}{
		{"", []string{"codex"}, true, clientsAuto},
		{clientsAuto, []string{"codex"}, true, clientsAuto},
		{"none", []string{"codex"}, true, "codex"},
		{"codex", []string{"claude-desktop"}, true, "claude-desktop,codex"},
		{"claude-desktop,codex", []string{"codex"}, false, "claude-desktop"},
		{"codex", []string{"codex"}, false, "none"},
		{"none", []string{"codex"}, false, "none"},
		{clientsAuto, systemClients(), false, "none"},
		{"none", systemClients(), true, all},
	} {
		if got := withClients(tc.saved, tc.names, tc.add); got != tc.want {
			t.Errorf("withClients(%q, %v, %t) = %q, want %q", tc.saved, tc.names, tc.add, got, tc.want)
		}
	}
}

func TestClaudeDesktopDisconnectKeepsOtherServers(t *testing.T) {
	withDefaultDataDir(t)
	c := testClaudeDesktop(t)
	before := `{"preferences": {"theme": "dark"}, "mcpServers": {"files": {"command": "npx", "args": ["files"]}}}`
	if err := os.WriteFile(c.path, []byte(before), 0o640); err != nil {
		t.Fatal(err)
	}
	dataDir := "/srv/agentiscale-review"
	connect(t, c, dataDir)
	if got := clientState(context.Background(), c, dataDir); got != `connected as "agentiscale-review"` {
		t.Fatalf("after connect: %s", got)
	}
	var out bytes.Buffer
	disconnectClients(context.Background(), &out, []agentClient{c}, dataDir)
	if !strings.Contains(out.String(), `removed "agentiscale-review"`) {
		t.Fatalf("output:\n%s", &out)
	}
	if got := clientState(context.Background(), c, dataDir); got != "not connected" {
		t.Fatalf("after disconnect: %s", got)
	}
	have, err := c.entries(context.Background())
	if err != nil || len(have) != 1 || have["files"].Command != "npx" {
		t.Fatalf("other servers: %v, %v", have, err)
	}
	b, _ := os.ReadFile(c.path)
	if !strings.Contains(string(b), `"theme": "dark"`) {
		t.Fatalf("lost the owner's settings:\n%s", b)
	}
	out.Reset()
	disconnectClients(context.Background(), &out, []agentClient{c}, dataDir)
	if !strings.Contains(out.String(), "not connected to this node") {
		t.Fatalf("second disconnect:\n%s", &out)
	}
}

func TestClientsNeedANode(t *testing.T) {
	withDefaultDataDir(t)
	if err := runClients(context.Background(), []string{"list"}, io.Discard); err == nil || !strings.Contains(err.Error(), "agentiscaled up") {
		t.Fatalf("clients without a node: %v", err)
	}
	if err := runClients(context.Background(), []string{"connect", "cursor"}, io.Discard); err == nil || !strings.Contains(err.Error(), "unknown client") {
		t.Fatalf("unknown client: %v", err)
	}
}

func TestUpRefusesAgentFlags(t *testing.T) {
	withDefaultDataDir(t)
	for _, flag := range []string{"-agent=codex", "-agent-dir=/tmp", "-agent-owner-config=false"} {
		err := runUp(context.Background(), []string{"-y", "-foreground", flag}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "agentiscaled agent add") {
			t.Errorf("up %s: %v", flag, err)
		}
	}
}

// `agent add` puts the first agent on this machine's node and a second one,
// under another name, on a node of its own; `agent remove` takes each off.
func TestAgentAddListRemove(t *testing.T) {
	def := withDefaultDataDir(t)
	mesh := newStandInMesh(t, "fine")
	command := filepath.Join(t.TempDir(), "codex")
	write(t, command, "#!/bin/sh\necho codex-cli-test\n")
	if err := os.Chmod(command, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "owner-codex"))
	write(t, filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), "{}")
	extra := extraNodeDir("second")
	for _, dir := range []string{def, extra} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		pid := seedNode(t, dir, mesh.srv.URL, mesh.key)
		// The extra node is joined already, so the test signs nothing in.
		if dir == def {
			writeSettings(t, dir, settings{Control: "https://control.test", Listen: "/ip4/127.0.0.1/tcp/0", LogLevel: "info", Clients: "none", Foreground: true, PeerID: pid, MeshURL: mesh.srv.URL})
		}
	}
	ctx := context.Background()
	work := t.TempDir()
	var out bytes.Buffer
	if err := runAgent(ctx, []string{"add", "codex", "-y", "-name", "helper", "-dir", work, "-command", command}, &out); err != nil {
		t.Fatalf("add: %v\n%s", err, &out)
	}
	s, _ := loadSettings(def)
	if s.Agent != "codex" || s.AgentName != "helper" || s.AgentDir != work || !s.Foreground || s.Clients != "none" {
		t.Fatalf("first agent: %+v", s)
	}
	// Adding the same name again changes it in place.
	if err := runAgent(ctx, []string{"add", "codex", "-y", "-name", "helper", "-dir", work, "-command", command, "-dir-skills=false"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if s, _ = loadSettings(def); s.AgentName != "helper" || !s.AgentIgnoreDirSkills {
		t.Fatalf("changed agent: %+v", s)
	}
	out.Reset()
	if err := runAgent(ctx, []string{"add", "codex", "-y", "-name", "second", "-dir", t.TempDir(), "-command", command}, &out); err != nil {
		t.Fatalf("second add: %v\n%s", err, &out)
	}
	if !strings.Contains(out.String(), "a node of its own") {
		t.Fatalf("second add output:\n%s", &out)
	}
	x, _ := loadSettings(extra)
	if x.AgentName != "second" || x.Clients != "none" || !x.Foreground || !strings.HasSuffix(x.MachineName, "-second") {
		t.Fatalf("second agent: %+v", x)
	}
	if s, _ = loadSettings(def); s.AgentName != "helper" {
		t.Fatalf("second add changed the first node: %+v", s)
	}
	// A name in use on another node is refused.
	if err := runAgent(ctx, []string{"add", "codex", "-y", "-name", "second", "-dir", work, "-command", command, "-data-dir", def}, io.Discard); err == nil {
		t.Fatal("added a second agent to a node that offers one")
	}

	out.Reset()
	if err := runAgent(ctx, []string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "helper") || !strings.Contains(out.String(), "second") {
		t.Fatalf("list:\n%s", &out)
	}
	if err := runAgent(ctx, []string{"remove"}, io.Discard); err == nil || !strings.Contains(err.Error(), "helper, second") {
		t.Fatalf("remove without a name among two: %v", err)
	}
	out.Reset()
	if err := runAgent(ctx, []string{"remove", "second"}, &out); err != nil {
		t.Fatal(err)
	}
	if x, _ = loadSettings(extra); x.Agent != "" || x.PeerID == "" {
		t.Fatalf("removed agent's node: %+v", x)
	}
	if !strings.Contains(out.String(), "console") {
		t.Fatalf("remove output:\n%s", &out)
	}
	if err := runAgent(ctx, []string{"remove"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if s, _ = loadSettings(def); s.Agent != "" || s.AgentName != "" || s.AgentIgnoreDirSkills || s.PeerID == "" {
		t.Fatalf("after remove: %+v", s)
	}
	out.Reset()
	if err := runAgent(ctx, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "offers no agent") {
		t.Fatalf("list after remove: %v\n%s", err, &out)
	}
}

func TestAgentAddNeedsARuntimeWithoutATerminal(t *testing.T) {
	withDefaultDataDir(t)
	old := interactive
	interactive = func() bool { return false }
	t.Cleanup(func() { interactive = old })
	if err := runAgent(context.Background(), []string{"add"}, io.Discard); err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("add without a runtime: %v", err)
	}
	if err := runAgent(context.Background(), []string{"add", "cursor"}, io.Discard); err == nil || !strings.Contains(err.Error(), "unknown runtime") {
		t.Fatalf("unknown runtime: %v", err)
	}
}
