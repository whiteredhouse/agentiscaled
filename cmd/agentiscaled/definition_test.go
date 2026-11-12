package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const claudeAgentFile = `---
name: security-reviewer
description: "Use for security review: auth, input handling and secrets."
tools: Read, Grep, Bash, mcp__docs__search
model: sonnet
---

# Security reviewer

You review code for "security" bugs.
`

const codexAgentFile = `name = "docs-writer"
description = "Use to write and edit user docs."
model = "gpt-5-codex"
model_reasoning_effort = "high"
sandbox_mode = "read-only"
developer_instructions = """
# Docs writer

You write docs. Quote \"exactly\".
"""
`

func writeDefinition(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, rel)
	write(t, path, content)
	return path
}

func TestLoadDefinition(t *testing.T) {
	root := t.TempDir()
	d, err := loadDefinition(writeDefinition(t, root, ".claude/agents/security-reviewer.md", claudeAgentFile))
	if err != nil {
		t.Fatal(err)
	}
	if d.Runtime != "claude" || d.Name != "security-reviewer" || d.Description != "Use for security review: auth, input handling and secrets." ||
		d.Model != "sonnet" || !slices.Equal(d.Tools, []string{"Read", "Grep", "Bash", "mcp__docs__search"}) ||
		d.Instructions != "# Security reviewer\n\nYou review code for \"security\" bugs." {
		t.Fatalf("claude definition: %+v", d)
	}
	d, err = loadDefinition(writeDefinition(t, root, ".codex/agents/docs-writer.toml", codexAgentFile))
	if err != nil {
		t.Fatal(err)
	}
	if d.Runtime != "codex" || d.Name != "docs-writer" || d.Model != "gpt-5-codex" || d.ReasoningEffort != "high" || !d.ReadOnly ||
		d.Instructions != "# Docs writer\n\nYou write docs. Quote \"exactly\"." {
		t.Fatalf("codex definition: %+v", d)
	}

	for name, content := range map[string]string{
		"no-front.md":   "# Just a doc\n",
		"unclosed.md":   "---\nname: x\n",
		"empty.md":      "---\nname: x\n---\n\n",
		"empty.toml":    `name = "x"`,
		"broken.toml":   `name = `,
		"other.yaml":    "name: x",
		"inherit-ok.md": "---\nname: x\nmodel: inherit\n---\nDo x.",
	} {
		d, err := loadDefinition(writeDefinition(t, root, name, content))
		if name == "inherit-ok.md" {
			if err != nil || d.Model != "" || d.Tools != nil {
				t.Errorf("%s: %+v, %v", name, d, err)
			}
		} else if err == nil {
			t.Errorf("%s loaded", name)
		}
	}
}

func TestDefinitionRoot(t *testing.T) {
	for path, want := range map[string]string{
		"/w/proj/.claude/agents/a.md":  "/w/proj",
		"/w/proj/.codex/agents/a.toml": "/w/proj",
		"/w/proj/agents/a.md":          "",
		"/w/proj/.claude/a.md":         "",
	} {
		if got := definitionRoot(path); got != want {
			t.Errorf("definitionRoot(%s) = %q, want %q", path, got, want)
		}
	}
}

// A definition's tools only narrow what the policy allows.
func TestClaudeTools(t *testing.T) {
	readOnly := []string{"Read", "Grep", "Glob"}
	claudeDef := &agentDefinition{Runtime: "claude", Tools: []string{"Read", "Bash", "mcp__docs__search"}}
	for _, tc := range []struct {
		d       *agentDefinition
		allowed []string
		want    string
		ok      bool
	}{
		{nil, nil, "", false},
		{nil, readOnly, "Read,Grep,Glob", true},
		{claudeDef, nil, "Read,Bash", true},
		{claudeDef, readOnly, "Read", true},
		{&agentDefinition{Runtime: "claude", Tools: []string{"Bash"}}, readOnly, "", true},
		{&agentDefinition{Runtime: "codex"}, readOnly, "Read,Grep,Glob", true},
		{&agentDefinition{Runtime: "claude"}, nil, "", false},
	} {
		got, ok := claudeTools(tc.d, tc.allowed)
		if got != tc.want || ok != tc.ok {
			t.Errorf("claudeTools(%+v, %v) = %q, %t; want %q, %t", tc.d, tc.allowed, got, ok, tc.want, tc.ok)
		}
	}
}

func TestClaudeRuntimeDefinition(t *testing.T) {
	dir := t.TempDir()
	command, record := fakeClaude(t, dir, "ok", false)
	d, err := loadDefinition(writeDefinition(t, dir, "sec.md", claudeAgentFile))
	if err != nil {
		t.Fatal(err)
	}
	rt := &claudeRuntime{command: command, config: filepath.Join(dir, "cfg"), def: d}
	if _, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	args := readRecord(t, record+".args")
	for _, want := range []string{"--append-system-prompt\n# Security reviewer\n\nYou review code for \"security\" bugs.\n", "--tools\nRead,Grep\n", "--model\nsonnet\n", "--restricted\n"} {
		if !strings.Contains(args, want) {
			t.Errorf("claude arguments %q lack %q", args, want)
		}
	}
	if strings.Count(args, "--tools") != 1 {
		t.Errorf("claude arguments %q name tools more than once", args)
	}

	// A Codex agent file brings its instructions, not its Codex model.
	d, err = loadDefinition(writeDefinition(t, dir, "docs.toml", codexAgentFile))
	if err != nil {
		t.Fatal(err)
	}
	rt = &claudeRuntime{command: command, def: d, policy: agentPolicy{OwnerConfig: true, DirInstructions: true, DirSkills: true}}
	if _, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	args = readRecord(t, record+".args")
	if !strings.Contains(args, "--append-system-prompt\n# Docs writer") || strings.Contains(args, "--model") || strings.Contains(args, "--tools") {
		t.Errorf("claude arguments with a Codex definition: %q", args)
	}
}

// fakeCodexRecorder writes a codex script that records its arguments and answers
// in the --output-last-message file.
func fakeCodexRecorder(t *testing.T, dir string) (command, record string) {
	t.Helper()
	record = filepath.Join(dir, "codex-args")
	command = filepath.Join(dir, "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = --output-last-message ]; then echo done > \"$2\"; fi\n  shift\ndone\ncat > /dev/null\n"
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return command, record
}

func TestCodexRuntimeDefinition(t *testing.T) {
	dir := t.TempDir()
	command, record := fakeCodexRecorder(t, dir)
	d, err := loadDefinition(writeDefinition(t, dir, "docs.toml", codexAgentFile))
	if err != nil {
		t.Fatal(err)
	}
	rt := &codexRuntime{command: command, def: d, policy: agentPolicy{OwnerConfig: true, DirInstructions: true, DirSkills: true}}
	if out, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x"}); err != nil || out != "done" {
		t.Fatalf("run: %q, %v", out, err)
	}
	args := strings.Split(readRecord(t, record), "\n")
	var instructions string
	for i, a := range args {
		if a == "-c" && i+1 < len(args) && strings.HasPrefix(args[i+1], "developer_instructions=") {
			instructions = args[i+1]
		}
	}
	var got struct {
		DeveloperInstructions string `toml:"developer_instructions"`
	}
	if _, err := toml.Decode(instructions, &got); err != nil || got.DeveloperInstructions != d.Instructions {
		t.Fatalf("developer_instructions %q decodes to %q, %v", instructions, got.DeveloperInstructions, err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-m gpt-5-codex", `-c model_reasoning_effort="high"`, "-s read-only"} {
		if !strings.Contains(joined, want) {
			t.Errorf("codex arguments %q lack %q", joined, want)
		}
	}

	// A Claude Code agent file brings its instructions, not its model.
	d, err = loadDefinition(writeDefinition(t, dir, "sec.md", claudeAgentFile))
	if err != nil {
		t.Fatal(err)
	}
	rt.def = d
	if _, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	joined = strings.ReplaceAll(readRecord(t, record), "\n", " ")
	if !strings.Contains(joined, "developer_instructions=") || strings.Contains(joined, "-m sonnet") || strings.Contains(joined, "-s read-only") {
		t.Errorf("codex arguments with a Claude Code definition: %q", joined)
	}
}

func TestTOMLStringRoundTrips(t *testing.T) {
	for _, s := range []string{"plain", "line\nbreak\ttab", `back\slash "quote"`, "bell\x07 del\x7f", "emoji 🧠 — dash"} {
		var got struct{ V string }
		if _, err := toml.Decode("V = "+tomlString(s), &got); err != nil || got.V != s {
			t.Errorf("tomlString(%q) = %s decodes to %q, %v", s, tomlString(s), got.V, err)
		}
	}
}

// `agent add -definition` takes the runtime, name, description and
// directory from the definition.
func TestAgentAddDefinition(t *testing.T) {
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
	project := t.TempDir()
	path := writeDefinition(t, project, ".codex/agents/docs-writer.toml", codexAgentFile)
	var out bytes.Buffer
	if err := runAgent(context.Background(), []string{"add", "-y", "-definition", path, "-command", command}, &out); err != nil {
		t.Fatalf("add: %v\n%s", err, &out)
	}
	s, _ := loadSettings(def)
	if len(s.Agents) != 1 {
		t.Fatalf("agents: %+v", s.Agents)
	}
	a := s.Agents[0]
	if a.Runtime != "codex" || a.Name != "docs-writer" || a.Description != "Use to write and edit user docs." || a.Dir != project || a.Definition != path {
		t.Fatalf("agent: %+v", a)
	}
	// Flags win over the definition.
	other := t.TempDir()
	if err := runAgent(context.Background(), []string{"add", "codex", "-y", "-definition", path, "-name", "writer", "-description", "Writes.", "-dir", other, "-command", command}, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, _ = loadSettings(def)
	if a := s.Agents[1]; a.Name != "writer" || a.Description != "Writes." || a.Dir != other || a.Definition != path {
		t.Fatalf("agent with flags: %+v", a)
	}
	if err := runAgent(context.Background(), []string{"add", "-y", "-definition", filepath.Join(project, "missing.md")}, io.Discard); err == nil {
		t.Fatal("add with a missing definition")
	}
}
