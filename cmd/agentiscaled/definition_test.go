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
	if d.WrittenFor != "claude" || d.Name != "security-reviewer" || d.Description != "Use for security review: auth, input handling and secrets." ||
		d.Model != "sonnet" || !slices.Equal(d.Tools, []string{"Read", "Grep", "Bash", "mcp__docs__search"}) ||
		d.Instructions != "# Security reviewer\n\nYou review code for \"security\" bugs." {
		t.Fatalf("claude definition: %+v", d)
	}
	d, err = loadDefinition(writeDefinition(t, root, ".codex/agents/docs-writer.toml", codexAgentFile))
	if err != nil {
		t.Fatal(err)
	}
	if d.WrittenFor != "codex" || d.Name != "docs-writer" || d.Model != "gpt-5-codex" || d.ReasoningEffort != "high" || !d.ReadOnly ||
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
	claudeDef := &agentDefinition{WrittenFor: "claude", Tools: []string{"Read", "Bash", "mcp__docs__search"}}
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
		{&agentDefinition{WrittenFor: "claude", Tools: []string{"Bash"}}, readOnly, "", true},
		{&agentDefinition{WrittenFor: "codex"}, readOnly, "Read,Grep,Glob", true},
		{&agentDefinition{WrittenFor: "claude"}, nil, "", false},
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
	if out, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x"}); err != nil || out.Text != "done" {
		t.Fatalf("run: %q, %v", out.Text, err)
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

// `agent add -definition` copies the definition into the agent's state and
// takes the name and directory from it, never the runtime.
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
	old := interactive
	interactive = func() bool { return false }
	t.Cleanup(func() { interactive = old })
	// The file's format does not pick the runtime.
	if err := runAgent(context.Background(), []string{"add", "-y", "-definition", path, "-command", command}, io.Discard); err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("add without a runtime: %v", err)
	}
	var out bytes.Buffer
	if err := runAgent(context.Background(), []string{"add", "codex", "-y", "-definition", path, "-command", command}, &out); err != nil {
		t.Fatalf("add: %v\n%s", err, &out)
	}
	if strings.Contains(out.String(), "ignores") {
		t.Errorf("a Codex agent file on codex warned:\n%s", &out)
	}
	s, _ := loadSettings(def)
	if len(s.Agents) != 1 {
		t.Fatalf("agents: %+v", s.Agents)
	}
	a := s.Agents[0]
	if a.Runtime != "codex" || a.Name != "docs-writer" || a.Description != "" || a.Dir != project || !strings.HasSuffix(a.Definition, "/agents/docs-writer/definition.toml") {
		t.Fatalf("agent: %+v", a)
	}
	// The node runs it with its own copy, out of the agent's directory, and
	// its card has the copy's description.
	copyPath := filepath.Join(def, "agents", "docs-writer", "definition.toml")
	if b, err := os.ReadFile(copyPath); err != nil || string(b) != codexAgentFile {
		t.Fatalf("copy: %q, %v", b, err)
	}
	svc, err := newAgent(&a, def)
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.card(a.Name).Description; got != "Use to write and edit user docs." {
		t.Errorf("card description %q", got)
	}
	if rt := svc.rt.(*codexRuntime); rt.def == nil || rt.def.Name != "docs-writer" {
		t.Errorf("runtime definition: %+v", rt.def)
	}
	// agent list shows the copy; a changed or deleted source changes nothing
	// until the next add.
	out.Reset()
	if err := runAgent(context.Background(), []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "DEFINITION") ||
		!strings.Contains(out.String(), "agents/docs-writer/definition.toml") || strings.Contains(out.String(), ".codex/agents") {
		t.Fatalf("list: %v\n%s", err, &out)
	}
	write(t, path, strings.Replace(codexAgentFile, "You write docs.", "You write poems.", 1))
	if d, err := loadDefinition(copyPath); err != nil || strings.Contains(d.Instructions, "poems") {
		t.Fatalf("copy after a change: %+v, %v", d, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := newAgent(&a, def); err != nil {
		t.Fatalf("agent without its source: %v", err)
	}
	// The node reads only the copy in the agent's state, whatever the
	// settings name.
	moved := a
	moved.Definition = filepath.Join(project, "elsewhere", "definition.toml")
	if _, err := newAgent(&moved, def); err != nil {
		t.Fatalf("agent with another directory in its definition: %v", err)
	}
	write(t, path, codexAgentFile)
	// Flags win over the definition.
	other := t.TempDir()
	if err := runAgent(context.Background(), []string{"add", "codex", "-y", "-definition", path, "-name", "writer", "-description", "Writes.", "-dir", other, "-command", command}, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, _ = loadSettings(def)
	if a := s.Agents[1]; a.Name != "writer" || a.Description != "Writes." || a.Dir != other || !strings.HasSuffix(a.Definition, "/agents/writer/definition.toml") {
		t.Fatalf("agent with flags: %+v", a)
	}
	// A Claude Code agent file on codex says what codex ignores.
	sec := writeDefinition(t, project, ".claude/agents/security-reviewer.md", claudeAgentFile)
	out.Reset()
	if err := runAgent(context.Background(), []string{"add", "codex", "-y", "-definition", sec, "-command", command}, &out); err != nil {
		t.Fatalf("add a Claude Code agent file: %v\n%s", err, &out)
	}
	if !strings.Contains(out.String(), "security-reviewer.md is a Claude Code agent file: codex ignores its model, tools") {
		t.Errorf("no warning:\n%s", &out)
	}
	if err := runAgent(context.Background(), []string{"add", "codex", "-y", "-definition", filepath.Join(project, "missing.md")}, io.Discard); err == nil {
		t.Fatal("add with a missing definition")
	}
	// Adding it again without a definition, or removing it, drops the copy.
	if err := runAgent(context.Background(), []string{"add", "codex", "-y", "-name", "writer", "-dir", other, "-command", command}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(def, "agents", "writer", "definition.toml")); !os.IsNotExist(err) {
		t.Errorf("copy after add without a definition: %v", err)
	}
	if err := runAgent(context.Background(), []string{"remove", "docs-writer"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Errorf("copy after remove: %v", err)
	}
}
