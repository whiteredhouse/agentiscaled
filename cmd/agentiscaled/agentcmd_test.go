package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentsListCommandAndAlias(t *testing.T) {
	t.Setenv("AGENTISCALED_TEST_MAIN", "1")
	dataDir := t.TempDir()
	for _, group := range []string{"agents", "agent"} {
		t.Run(group, func(t *testing.T) {
			out, err := exec.Command(os.Args[0], group, "list", "-data-dir", dataDir).CombinedOutput()
			if err != nil {
				t.Fatalf("%s list: %v\n%s", group, err, out)
			}
			if !strings.Contains(string(out), "offers no agent") || !strings.Contains(string(out), "agentiscaled agents add") {
				t.Errorf("expected empty agents list with the canonical add command, got %q", out)
			}
		})
	}
}

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

func TestUpHasNoAgentFlags(t *testing.T) {
	withDefaultDataDir(t)
	for _, flag := range []string{"-agent=codex", "-agent-dir=/tmp", "-agent-owner-config=false"} {
		err := runUp(context.Background(), []string{"-y", "-foreground", flag}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "not defined") {
			t.Errorf("up %s: %v", flag, err)
		}
	}
}

// `agents add` puts each agent on this machine's node.
// `agents remove` takes it off that node.
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
	if err := os.MkdirAll(def, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := seedNode(t, def, mesh.srv.URL, mesh.key)
	writeSettings(t, def, settings{Control: "https://control.test", Listen: "/ip4/127.0.0.1/tcp/0", LogLevel: "info", Clients: "none", Foreground: true, PeerID: pid, MeshURL: mesh.srv.URL})
	ctx := context.Background()
	work := t.TempDir()
	var out bytes.Buffer
	if err := runAgent(ctx, []string{"add", "codex", "-y", "-name", "helper", "-dir", work, "-command", command}, &out); err != nil {
		t.Fatalf("add: %v\n%s", err, &out)
	}
	s, _ := loadSettings(def)
	if len(s.Agents) != 1 || s.Agents[0].Runtime != "codex" || s.Agents[0].Name != "helper" || s.Agents[0].Dir != work || !s.Foreground || s.Clients != "none" || s.PeerID != pid {
		t.Fatalf("first agent: %+v", s)
	}
	// Adding the same name again changes it in place.
	if err := runAgent(ctx, []string{"add", "codex", "-y", "-name", "helper", "-dir", work, "-command", command, "-dir-skills=false"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if s, _ = loadSettings(def); len(s.Agents) != 1 || !s.Agents[0].IgnoreDirSkills {
		t.Fatalf("changed agent: %+v", s)
	}
	// Another name joins it on the same node, which keeps its identity.
	second := t.TempDir()
	if err := runAgent(ctx, []string{"add", "codex", "-y", "-name", "second", "-dir", second, "-command", command}, &out); err != nil {
		t.Fatalf("second add: %v\n%s", err, &out)
	}
	if s, _ = loadSettings(def); len(s.Agents) != 2 || s.Agents[0].Name != "helper" || s.Agents[1].Name != "second" || s.Agents[1].Dir != second || s.PeerID != pid {
		t.Fatalf("second agent: %+v", s)
	}
	if extra, _ := filepath.Glob(filepath.Join(filepath.Dir(def), "agentiscale-*")); len(extra) != 0 {
		t.Fatalf("add set up another node: %v", extra)
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
	if err := runAgent(ctx, []string{"remove", "third"}, io.Discard); err == nil || !strings.Contains(err.Error(), "no agent named") {
		t.Fatalf("remove an unknown agent: %v", err)
	}
	if err := runAgent(ctx, []string{"remove", "second"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if s, _ = loadSettings(def); len(s.Agents) != 1 || s.Agents[0].Name != "helper" {
		t.Fatalf("after removing second: %+v", s)
	}
	if err := runAgent(ctx, []string{"remove"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if s, _ = loadSettings(def); len(s.Agents) != 0 || s.PeerID == "" {
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
