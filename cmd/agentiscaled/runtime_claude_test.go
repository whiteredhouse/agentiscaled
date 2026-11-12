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
	if err != nil || out.Text != "No findings" {
		t.Fatalf("run: %q, %v", out.Text, err)
	}
	args := " " + strings.ReplaceAll(readRecord(t, record+".args"), "\n", " ")
	for _, want := range []string{" -p ", " --output-format json ", " --json-schema " + turnSchema + " ", " --session-id ", " --permission-prompts none ",
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

// fakeClaudeCLI writes a claude that signs in, prints help, and records the
// arguments of a run.
func fakeClaudeCLI(t *testing.T, help string) (command, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "args")
	command = filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo '9.9.9 (Claude Code)';;\n--help) printf '%s\\n' '" + help + "';;\n" +
		"auth) echo '{\"loggedIn\":true}';;\n*) printf '%s\\n' \"$@\" > " + record + "; cat > /dev/null; echo '{\"result\":\"ok\",\"is_error\":false}';;\nesac\n"
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return command, record
}

// A Claude Code without --permission-prompts runs without it; the
// isolated configuration needs --restricted.
func TestClaudeRuntimeOlderCLI(t *testing.T) {
	owner := agentPolicy{OwnerConfig: true, DirInstructions: true, DirSkills: true}
	command, record := fakeClaudeCLI(t, "  --restricted  --tools")
	rt, err := newClaudeRuntime(command, t.TempDir(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Run(context.Background(), t.TempDir(), taskRequest{Sender: testSender, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if args := readRecord(t, record); strings.Contains(args, "--permission-prompts") || !strings.Contains(args, "-p\n") {
		t.Errorf("arguments for a CLI without --permission-prompts: %q", args)
	}

	command, record = fakeClaudeCLI(t, "  --restricted  --permission-prompts <target>")
	if rt, err = newClaudeRuntime(command, t.TempDir(), owner); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Run(context.Background(), t.TempDir(), taskRequest{Sender: testSender, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if args := readRecord(t, record); !strings.Contains(args, "--permission-prompts\nnone\n") {
		t.Errorf("arguments for a current CLI: %q", args)
	}

	command, _ = fakeClaudeCLI(t, "  --permission-prompts <target>")
	if _, err := newClaudeRuntime(command, t.TempDir(), agentPolicy{}); err == nil || !strings.Contains(err.Error(), "update") {
		t.Errorf("isolated runtime without --restricted: %v", err)
	}
}

// A turn that asks a question names its session, and the answer resumes it.
func TestClaudeRuntimeAsksAndResumes(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "rec")
	command := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + ".args\ncat > " + record + ".prompt\n" +
		"echo '{\"type\":\"result\",\"is_error\":false,\"result\":\"\",\"structured_output\":{\"status\":\"needs_input\",\"text\":\"Which file?\"}}'\n"
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rt := &claudeRuntime{command: command, policy: agentPolicy{OwnerConfig: true}}
	first, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "Review a file"})
	if err != nil || !first.Question || first.Text != "Which file?" {
		t.Fatalf("first turn: %+v %v", first, err)
	}
	if args := readRecord(t, record+".args"); !strings.Contains(args, "--session-id\n"+first.Session+"\n") || strings.Contains(args, "--resume") {
		t.Fatalf("first turn arguments: %q", args)
	}
	if _, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "main.go", Session: first.Session}); err != nil {
		t.Fatal(err)
	}
	if args := readRecord(t, record+".args"); !strings.Contains(args, "--resume\n"+first.Session+"\n") || strings.Contains(args, "--session-id") {
		t.Fatalf("answer's arguments: %q", args)
	}
	if p := readRecord(t, record+".prompt"); !strings.Contains(p, "<answer>\nmain.go\n</answer>") || !strings.Contains(p, "node "+testSender) {
		t.Fatalf("answer's prompt: %q", p)
	}
	// A session the node did not make is not passed on.
	if _, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x", Session: "--dangerously-skip-permissions"}); err == nil {
		t.Fatal("ran with a session that is not a session ID")
	}
}
