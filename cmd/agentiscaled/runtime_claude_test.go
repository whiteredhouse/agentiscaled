package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaude writes a claude script that records its arguments, environment
// and prompt next to itself and prints result as its JSON result.
func fakeClaude(t *testing.T, dir, result string, isError bool) (command, record string) {
	t.Helper()
	record = filepath.Join(dir, "rec")
	command = filepath.Join(dir, "claude")
	errFlag := "false"
	if isError {
		errFlag = "true"
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + ".args\nenv > " + record + ".env\npwd > " + record + ".pwd\ncat > " + record + ".prompt\n" +
		"printf '{\"type\":\"result\",\"is_error\":" + errFlag + ",\"result\":\"%s\"}\\n' '" + result + "'\n"
	if isError {
		script += "exit 1\n"
	}
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return command, record
}

func readRecord(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClaudeRuntimeIsolatedIsRestrictedReadOnly(t *testing.T) {
	dir := t.TempDir()
	command, record := fakeClaude(t, dir, "No findings", false)
	t.Setenv("ANTHROPIC_API_KEY", "owner-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "owner-token")
	config := filepath.Join(dir, "agent-claude")
	out, err := (&claudeRuntime{command: command, config: config}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "Review main.go"})
	if err != nil || out != "No findings" {
		t.Fatalf("run: %q, %v", out, err)
	}
	args := " " + strings.ReplaceAll(readRecord(t, record+".args"), "\n", " ")
	for _, want := range []string{" -p ", " --output-format json ", " --no-session-persistence ", " --permission-prompts none ",
		" --restricted ", " --strict-mcp-config ", " --tools Read,Grep,Glob ", " --permission-mode dontAsk ", " --disable-slash-commands "} {
		if !strings.Contains(args, want) {
			t.Errorf("claude arguments %q lack %q", args, want)
		}
	}
	env := readRecord(t, record+".env")
	for _, want := range []string{"CLAUDE_CONFIG_DIR=" + config + "\n", "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1\n", "CLAUDE_CODE_DISABLE_BUNDLED_SKILLS=1\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("environment lacks %q", want)
		}
	}
	for _, avoid := range []string{"owner-key", "owner-token"} {
		if strings.Contains(env, avoid) {
			t.Errorf("the owner's credential %q reached the isolated run", avoid)
		}
	}
	if pwd := strings.TrimSpace(readRecord(t, record+".pwd")); pwd != dir {
		if real, _ := filepath.EvalSymlinks(dir); pwd != real {
			t.Errorf("ran in %s, want %s", pwd, dir)
		}
	}
	p := readRecord(t, record+".prompt")
	for _, want := range []string{"from node " + testSender, "<request>\nReview main.go\n</request>"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt on stdin %q lacks %q", p, want)
		}
	}
}

// With the owner's configuration, Claude Code runs as theirs would, with
// their environment, and the directory's guidance loads by default.
func TestClaudeRuntimeOwnerConfig(t *testing.T) {
	dir := t.TempDir()
	command, record := fakeClaude(t, dir, "done", false)
	t.Setenv("ANTHROPIC_API_KEY", "owner-key")
	rt := &claudeRuntime{command: command, policy: agentPolicy{OwnerConfig: true, DirInstructions: true, DirSkills: true}}
	if _, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	args := " " + strings.ReplaceAll(readRecord(t, record+".args"), "\n", " ")
	for _, avoid := range []string{" --restricted ", " --tools ", " --disable-slash-commands "} {
		if strings.Contains(args, avoid) {
			t.Errorf("claude arguments %q have %q", args, avoid)
		}
	}
	env := readRecord(t, record+".env")
	if !strings.Contains(env, "ANTHROPIC_API_KEY=owner-key") {
		t.Error("the owner's environment did not reach the run")
	}
	if strings.Contains(env, "CLAUDE_CODE_DISABLE_CLAUDE_MDS") || strings.Contains(env, "CLAUDE_CONFIG_DIR=") {
		t.Errorf("owner run environment changed: %s", env)
	}
}

func TestClaudeRuntimeFailure(t *testing.T) {
	dir := t.TempDir()
	command, _ := fakeClaude(t, dir, "something broke", true)
	_, err := (&claudeRuntime{command: command, config: filepath.Join(dir, "c")}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "something broke") {
		t.Fatalf("err = %v, want the result's failure", err)
	}
}
