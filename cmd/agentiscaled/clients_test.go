package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withDefaultDataDir points the default data directory into a test directory.
func withDefaultDataDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return defaultDataDir()
}

func TestClientEntryName(t *testing.T) {
	def := withDefaultDataDir(t)
	for dir, want := range map[string]string{
		def:                                   "agentiscale",
		"/Users/a/Library/agentiscale-review": "agentiscale-review",
		"/Users/a/agentiscale-trial":          "agentiscale-trial",
		"/srv/My Node":                        "agentiscale-my-node",
		"/srv/agentiscale":                    "agentiscale-node",
		"/srv/...":                            "agentiscale-node",
	} {
		if got := clientEntryName(dir); got != want {
			t.Errorf("clientEntryName(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestServesNode(t *testing.T) {
	def := withDefaultDataDir(t)
	other := "/srv/agentiscale-review"
	for _, tc := range []struct {
		e    mcpEntry
		dir  string
		want bool
	}{
		{mcpEntry{"/opt/homebrew/bin/agentiscaled", []string{"mcp"}}, def, true},
		{mcpEntry{"/opt/homebrew/bin/agentiscaled", []string{"mcp"}}, other, false},
		{mcpEntry{"/x/agentiscaled", []string{"mcp", "-data-dir", other}}, other, true},
		{mcpEntry{"/x/agentiscaled", []string{"mcp", "--data-dir=" + other}}, other, true},
		{mcpEntry{"/x/agentiscaled", []string{"mcp", "-data-dir", other}}, def, false},
		{mcpEntry{"/x/agentiscaled", []string{"run"}}, def, false},
		{mcpEntry{"/x/node", []string{"mcp"}}, def, false},
	} {
		if got := servesNode(tc.e, tc.dir); got != tc.want {
			t.Errorf("servesNode(%v, %q) = %v, want %v", tc.e, tc.dir, got, tc.want)
		}
	}
}

func TestSelectClientsLeavesAServingNode(t *testing.T) {
	if got := selectClients(clientsAuto, true); len(got) != 0 {
		t.Errorf("auto on a node serving an agent connects %d clients", len(got))
	}
	if got := selectClients("codex", true); len(got) != 1 || got[0].name() != "Codex CLI" {
		t.Errorf("-clients codex = %v", got)
	}
	if got := selectClients("none", false); len(got) != 0 {
		t.Errorf("none connects %d clients", len(got))
	}
	if err := checkClients("claude-desktop, codex"); err != nil {
		t.Error(err)
	}
	if err := checkClients("cursor"); err == nil {
		t.Error("an unknown client is accepted")
	}
}

func testClaudeDesktop(t *testing.T) *claudeDesktop {
	dir := filepath.Join(t.TempDir(), "Claude")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &claudeDesktop{path: filepath.Join(dir, "claude_desktop_config.json")}
}

func connect(t *testing.T, c agentClient, dataDir string) string {
	t.Helper()
	var out bytes.Buffer
	connectClients(context.Background(), &out, []agentClient{c}, "/opt/homebrew/bin/agentiscaled", dataDir)
	return out.String()
}

func TestClaudeDesktopKeepsTheOwnersConfig(t *testing.T) {
	withDefaultDataDir(t)
	c := testClaudeDesktop(t)
	before := `{
    "preferences": {"theme": "dark"},
    "mcpServers": {
        "zed": {"command": "zed", "args": ["--mcp"], "env": {"A": "1"}},
        "files": {"command": "npx", "args": ["files"]}
    },
    "globalShortcut": ""
}
`
	if err := os.WriteFile(c.path, []byte(before), 0o640); err != nil {
		t.Fatal(err)
	}
	dataDir := "/srv/agentiscale-review"
	out := connect(t, c, dataDir)
	if !strings.Contains(out, `added "agentiscale-review"`) || !strings.Contains(out, "Quit Claude Desktop") {
		t.Fatalf("output:\n%s", out)
	}
	after, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	top, err := parseObject(after)
	if err != nil {
		t.Fatalf("%v\n%s", err, after)
	}
	var keys []string
	for _, f := range top {
		keys = append(keys, f.key)
	}
	if strings.Join(keys, ",") != "preferences,mcpServers,globalShortcut" {
		t.Errorf("top-level keys %v", keys)
	}
	raw, _ := lookup(top, "mcpServers")
	servers, _ := parseObject(raw)
	keys = nil
	for _, f := range servers {
		keys = append(keys, f.key)
	}
	if strings.Join(keys, ",") != "zed,files,agentiscale-review" {
		t.Errorf("servers %v", keys)
	}
	zed, _ := lookup(servers, "zed")
	if compact(t, zed) != `{"command":"zed","args":["--mcp"],"env":{"A":"1"}}` {
		t.Errorf("zed entry changed: %s", zed)
	}
	ours, _ := lookup(servers, "agentiscale-review")
	if compact(t, ours) != `{"command":"/opt/homebrew/bin/agentiscaled","args":["mcp","-data-dir","/srv/agentiscale-review"]}` {
		t.Errorf("our entry: %s", ours)
	}
	if fi, _ := os.Stat(c.path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	backups, _ := filepath.Glob(c.path + ".agentiscaled-*.bak")
	if len(backups) != 1 {
		t.Fatalf("backups %v", backups)
	}
	if b, _ := os.ReadFile(backups[0]); string(b) != before {
		t.Errorf("backup holds %q", b)
	}

	// A second `up` finds the entry and writes nothing.
	out = connect(t, c, dataDir)
	if !strings.Contains(out, `already connected to this node as "agentiscale-review"`) {
		t.Errorf("second run:\n%s", out)
	}
	if again, _ := os.ReadFile(c.path); !bytes.Equal(again, after) {
		t.Error("second run changed the file")
	}
}

func compact(t *testing.T, raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestClaudeDesktopNeverReplacesAnEntry(t *testing.T) {
	def := withDefaultDataDir(t)
	for name, before := range map[string]string{
		"name taken by another node": `{"mcpServers":{"agentiscale":{"command":"/x/agentiscaled","args":["mcp","-data-dir","/srv/other"]}}}`,
		"name taken by another tool": `{"mcpServers":{"agentiscale":{"url":"http://127.0.0.1:8080/mcp"}}}`,
		"node under another name":    `{"mcpServers":{"mesh":{"command":"/dev/build/agentiscaled","args":["mcp"]}}}`,
		"not JSON":                   `{"mcpServers": {`,
		"servers not an object":      `{"mcpServers": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := testClaudeDesktop(t)
			if err := os.WriteFile(c.path, []byte(before), 0o600); err != nil {
				t.Fatal(err)
			}
			out := connect(t, c, def)
			if b, _ := os.ReadFile(c.path); string(b) != before {
				t.Errorf("file changed to %s", b)
			}
			if name != "node under another name" && !strings.Contains(out, `"mcpServers"`) {
				t.Errorf("no step to do by hand:\n%s", out)
			}
			if backups, _ := filepath.Glob(c.path + ".*.bak"); len(backups) != 0 {
				t.Errorf("backups %v", backups)
			}
		})
	}
}

func TestClaudeDesktopFirstConfig(t *testing.T) {
	def := withDefaultDataDir(t)
	c := testClaudeDesktop(t)
	out := connect(t, c, def)
	if strings.Contains(out, "the file before") {
		t.Errorf("backup of a missing file:\n%s", out)
	}
	b, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "mcpServers": {
    "agentiscale": {
      "command": "/opt/homebrew/bin/agentiscaled",
      "args": [
        "mcp"
      ]
    }
  }
}
`
	if string(b) != want {
		t.Errorf("got\n%s\nwant\n%s", b, want)
	}

	missing := &claudeDesktop{path: filepath.Join(t.TempDir(), "Claude", "claude_desktop_config.json")}
	if out := connect(t, missing, def); !strings.Contains(out, "not installed") {
		t.Errorf("no Claude Desktop:\n%s", out)
	}
}

// fakeCodex is a stand-in codex that answers `mcp list --json` from a file and
// appends each `mcp add` to another.
func fakeCodex(t *testing.T, list string) (*codexClient, string) {
	dir := t.TempDir()
	listFile, log := filepath.Join(dir, "list.json"), filepath.Join(dir, "added")
	if err := os.WriteFile(listFile, []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$2\" in\nlist) cat " + shellQuote(listFile) + ";;\nadd) shift 2; echo \"$*\" >> " + shellQuote(log) + ";;\nesac\n"
	cmd := filepath.Join(dir, "codex")
	if err := os.WriteFile(cmd, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return &codexClient{command: cmd, home: home}, log
}

func TestCodexAddsOnlyAMissingEntry(t *testing.T) {
	def := withDefaultDataDir(t)
	c, log := fakeCodex(t, `[{"name":"other","transport":{"type":"stdio","command":"foo","args":[]}}]`)
	if err := os.WriteFile(filepath.Join(c.home, "config.toml"), []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := connect(t, c, def)
	if b, _ := os.ReadFile(log); string(b) != "agentiscale -- /opt/homebrew/bin/agentiscaled mcp\n" {
		t.Errorf("codex mcp add got %q\n%s", b, out)
	}
	if backups, _ := filepath.Glob(filepath.Join(c.home, "config.toml.agentiscaled-*.bak")); len(backups) != 1 {
		t.Errorf("backups %v", backups)
	}

	for list, want := range map[string]string{
		`[{"name":"agentiscale","transport":{"type":"stdio","command":"/x/agentiscaled","args":["mcp","-data-dir","/srv/b"]}}]`: `has another server named "agentiscale"`,
		`[{"name":"mesh","transport":{"type":"stdio","command":"/x/agentiscaled","args":["mcp"]}}]`:                             `already connected to this node as "mesh"`,
		`not json`: "could not read its configuration",
	} {
		c, log := fakeCodex(t, list)
		out := connect(t, c, def)
		if _, err := os.Stat(log); err == nil {
			t.Errorf("list %s: codex mcp add ran", list)
		}
		if !strings.Contains(out, want) {
			t.Errorf("list %s:\n%s", list, out)
		}
	}
}

// TestCodexWithTheRealCLI adds a node to a real Codex CLI's configuration in
// a scratch CODEX_HOME:
// AGENTISCALE_TEST_CODEX=$(command -v codex) go test ./cmd/agentiscaled -run TestCodexWithTheRealCLI -v
func TestCodexWithTheRealCLI(t *testing.T) {
	codex := os.Getenv("AGENTISCALE_TEST_CODEX")
	if codex == "" {
		t.Skip("set AGENTISCALE_TEST_CODEX to a codex binary")
	}
	withDefaultDataDir(t)
	home := filepath.Join(os.Getenv("HOME"), ".codex")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	before := "# my settings\nmodel = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"foo\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	c := &codexClient{command: codex, home: home}
	dataDir := "/srv/agentiscale-review"
	out := connect(t, c, dataDir)
	if !strings.Contains(out, `added "agentiscale-review"`) {
		t.Fatalf("output:\n%s", out)
	}
	have, err := c.entries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if e := have["agentiscale-review"]; !servesNode(e, dataDir) || e.Command != "/opt/homebrew/bin/agentiscaled" {
		t.Errorf("entry %+v", e)
	}
	b, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if !strings.HasPrefix(string(b), before) {
		t.Errorf("config.toml lost the owner's settings:\n%s", b)
	}
	if out := connect(t, c, dataDir); !strings.Contains(out, "already connected") {
		t.Errorf("second run:\n%s", out)
	}
}
