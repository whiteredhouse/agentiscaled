package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"

	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWithClients(t *testing.T) {
	t.Parallel()
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
	if err := RunClients(context.Background(), []string{"list"}, io.Discard); err == nil || !strings.Contains(err.Error(), "agentiscaled up") {
		t.Fatalf("clients without a node: %v", err)
	}
	if err := RunClients(context.Background(), []string{"connect", "cursor"}, io.Discard); err == nil || !strings.Contains(err.Error(), "unknown client") {
		t.Fatalf("unknown client: %v", err)
	}
}

func TestUpHasNoAgentFlags(t *testing.T) {
	withDefaultDataDir(t)
	for _, flag := range []string{"-agent=codex", "-agent-dir=/tmp", "-agent-owner-config=false"} {
		err := RunUp(context.Background(), []string{"-y", "-foreground", flag}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "not defined") {
			t.Errorf("up %s: %v", flag, err)
		}
	}
}

// `agent add` puts each agent on this machine's node.
// `agent remove` takes it off that node.
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
	if err := RunAgent(ctx, []string{"add", "codex", "-y", "-name", "helper", "-dir", work, "-command", command}, &out); err != nil {
		t.Fatalf("add: %v\n%s", err, &out)
	}
	s, _ := loadSettings(def)
	if len(s.Agents) != 1 || s.Agents[0].Runtime != "codex" || s.Agents[0].Name != "helper" || s.Agents[0].Dir != work || !s.Foreground || s.Clients != "none" || s.PeerID != pid {
		t.Fatalf("first agent: %+v", s)
	}
	// Adding the same name again changes it in place.
	if err := RunAgent(ctx, []string{"add", "codex", "-y", "-name", "helper", "-dir", work, "-command", command, "-dir-skills=false"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if s, _ = loadSettings(def); len(s.Agents) != 1 || !s.Agents[0].IgnoreDirSkills {
		t.Fatalf("changed agent: %+v", s)
	}
	// Another name joins it on the same node, which keeps its identity.
	second := t.TempDir()
	secondArgs := []string{"add", "codex", "-y", "-name", "second", "-dir", second, "-command", command, "-owner-config=false"}
	write(t, filepath.Join(def, "codex", "auth.json"), "{}")
	secondProfile := agentProfileDir(def, "second")
	if err := RunAgent(ctx, secondArgs, io.Discard); err == nil || !strings.Contains(err.Error(), secondProfile) {
		t.Fatalf("expected sign-in instructions for %s, got %v", secondProfile, err)
	}
	write(t, filepath.Join(secondProfile, "auth.json"), "{}")
	if err := RunAgent(ctx, secondArgs, &out); err != nil {
		t.Fatalf("second add: %v\n%s", err, &out)
	}
	if s, _ = loadSettings(def); len(s.Agents) != 2 || s.Agents[0].Name != "helper" || s.Agents[1].Name != "second" || s.Agents[1].Dir != second || !s.Agents[1].OwnConfig || s.PeerID != pid {
		t.Fatalf("second agent: %+v", s)
	}
	if extra, _ := filepath.Glob(filepath.Join(filepath.Dir(def), "agentiscale-*")); len(extra) != 0 {
		t.Fatalf("add set up another node: %v", extra)
	}

	out.Reset()
	if err := RunAgent(ctx, []string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "helper") || !strings.Contains(out.String(), "second") {
		t.Fatalf("list:\n%s", &out)
	}
	if err := RunAgent(ctx, []string{"remove"}, io.Discard); err == nil || !strings.Contains(err.Error(), "helper, second") {
		t.Fatalf("remove without a name among two: %v", err)
	}
	if err := RunAgent(ctx, []string{"remove", "third"}, io.Discard); err == nil || !strings.Contains(err.Error(), "no agent named") {
		t.Fatalf("remove an unknown agent: %v", err)
	}
	if err := RunAgent(ctx, []string{"remove", "second"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if s, _ = loadSettings(def); len(s.Agents) != 1 || s.Agents[0].Name != "helper" {
		t.Fatalf("after removing second: %+v", s)
	}
	if err := RunAgent(ctx, []string{"remove"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if s, _ = loadSettings(def); len(s.Agents) != 0 || s.PeerID == "" {
		t.Fatalf("after remove: %+v", s)
	}
	out.Reset()
	if err := RunAgent(ctx, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "offers no agent") {
		t.Fatalf("list after remove: %v\n%s", err, &out)
	}
}

func TestInteractiveAgentAddHonorsExplicitPolicyFlags(t *testing.T) {
	withDefaultDataDir(t)
	bin := t.TempDir()
	const cli = `#!/bin/sh
case "$1" in
  --version) echo test-codex ;;
  login) printf '{}' > "$CODEX_HOME/auth.json" ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(cli), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("CODEX_API_KEY", "")

	const owner = "Use your own codex configuration"
	const instructions = "Load the directory's instruction files"
	const skills = "Load the directory's skills"
	for _, tc := range []struct {
		name    string
		flags   []string
		prompts []string
		answer  string
		want    agentPolicy
	}{
		{"private profile", []string{"-owner-config=false"}, []string{instructions, skills}, "\n", agentPolicy{DirInstructions: true, DirSkills: true}},
		{"owner profile", []string{"-owner-config=true", "-dir-instructions=false"}, []string{skills}, "n", agentPolicy{OwnerConfig: true}},
		{"prompt for profile", []string{"-dir-instructions=true", "-dir-skills=false"}, []string{owner}, "n", agentPolicy{DirInstructions: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, ownerProfile := t.TempDir(), t.TempDir()
			t.Setenv("CODEX_HOME", ownerProfile)
			key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
			pid := seedNode(t, dir, "https://mesh.invalid", key)
			writeSettings(t, dir, settings{PeerID: pid, Clients: "none", Foreground: true})
			upWizardInput(t, "helper\n\n"+t.TempDir()+"\n"+tc.answer+"\n")

			var out bytes.Buffer
			if err := RunAgent(t.Context(), append([]string{"add", "-data-dir", dir}, tc.flags...), &out); err != nil {
				t.Fatalf("agent add: %v\n%s", err, &out)
			}
			for _, q := range []string{owner, instructions, skills} {
				if got := strings.Contains(out.String(), q); got != slices.Contains(tc.prompts, q) {
					t.Errorf("asked %q = %t; want prompts %q", q, got, tc.prompts)
				}
			}
			saved, err := loadSettings(dir)
			if err != nil || len(saved.Agents) != 1 {
				t.Fatalf("saved agents: %v, %v", saved.Agents, err)
			}
			if got := saved.Agents[0].policy(); got != tc.want {
				t.Errorf("saved policy = %+v; want %+v", got, tc.want)
			}
			profile := filepath.Join(dir, "agents", "helper", "profile")
			if tc.want.OwnerConfig {
				profile = ownerProfile
			}
			if _, err := os.Stat(filepath.Join(profile, "auth.json")); err != nil {
				t.Errorf("expected Codex sign-in in %s: %v", profile, err)
			}
		})
	}
}

func TestAgentAddNeedsARuntimeWithoutATerminal(t *testing.T) {
	def := withDefaultDataDir(t)
	if err := os.MkdirAll(def, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, def, settings{Control: "https://control.test", PeerID: "12D3KooWJoined"})
	old := interactive
	interactive = func() bool { return false }
	t.Cleanup(func() { interactive = old })
	if err := RunAgent(context.Background(), []string{"add"}, io.Discard); err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("add without a runtime: %v", err)
	}
	if err := RunAgent(context.Background(), []string{"add", "cursor"}, io.Discard); err == nil || !strings.Contains(err.Error(), "unknown runtime") {
		t.Fatalf("unknown runtime: %v", err)
	}
}

// `agent add` offers agents from a node that has joined; joining is up's job.
func TestAgentAddNeedsAJoinedNode(t *testing.T) {
	def := withDefaultDataDir(t)
	err := RunAgent(context.Background(), []string{"add", "codex", "-y"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "has not joined") || !strings.Contains(err.Error(), "agentiscaled up") {
		t.Fatalf("add without a node: %v", err)
	}
	if _, err := os.Stat(filepath.Join(def, settingsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("add without a node wrote settings: %v", err)
	}
	if err := os.MkdirAll(def, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, def, settings{Control: "https://control.test"})
	err = RunAgent(context.Background(), []string{"add", "codex", "-y"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "has not joined") {
		t.Fatalf("add on a node with no identity: %v", err)
	}
	other := filepath.Join(t.TempDir(), "other")
	err = RunAgent(context.Background(), []string{"add", "codex", "-y", "-data-dir", other}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "agentiscaled up -data-dir") {
		t.Fatalf("add on another data dir: %v", err)
	}
}

// Settings can still name a peer after its identity is gone, as after an
// interrupted reset. `agent add` must not sign in or enroll then either.
func TestAgentAddNeverEnrollsAStaleNode(t *testing.T) {
	def := withDefaultDataDir(t)
	var signIns atomic.Int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signIns.Add(1)
		http.Error(w, "no sign-in expected", http.StatusTeapot)
	}))
	t.Cleanup(idp.Close)
	old := defaultAuth
	defaultAuth.issuer = idp.URL
	t.Cleanup(func() { defaultAuth = old })
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
	writeSettings(t, def, settings{Control: "https://control.test", Clients: "none", Foreground: true, PeerID: "12D3KooWGone"})
	err := RunAgent(context.Background(), []string{"add", "codex", "-y", "-dir", t.TempDir(), "-command", command}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "has not joined") {
		t.Fatalf("add on a node whose identity is gone: %v", err)
	}
	if n := signIns.Load(); n != 0 {
		t.Fatalf("add asked the sign-in service %d times", n)
	}
	if s, err := loadSettings(def); err != nil || len(s.Agents) != 0 {
		t.Fatalf("add saved settings: %+v, %v", s.Agents, err)
	}
}
