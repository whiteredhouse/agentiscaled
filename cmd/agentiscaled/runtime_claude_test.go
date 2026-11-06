package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
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

func TestClaudeRuntimeUsageLimit(t *testing.T) {
	dir := t.TempDir()
	command, _ := fakeClaude(t, dir, "You have hit your limit, resets 3pm", true)
	_, err := (&claudeRuntime{command: command, config: filepath.Join(dir, "c")}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x"})
	var limit *usageLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("err = %v, want a usage limit", err)
	}
	command, _ = fakeClaude(t, dir, "something else broke", true)
	_, err = (&claudeRuntime{command: command, config: filepath.Join(dir, "c")}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x"})
	if err == nil || errors.As(err, &limit) {
		t.Fatalf("err = %v, want a failure that is not a usage limit", err)
	}
}

type stubRuntime struct {
	out   string
	err   error
	calls int
}

func (s *stubRuntime) Describe(card *a2a.AgentCard) {
	card.Description = "stub " + s.out
	card.Skills = []a2a.AgentSkill{{ID: "task", Tags: []string{"stub"}}}
}

func (s *stubRuntime) Run(context.Context, string, taskRequest) (string, error) {
	s.calls++
	return s.out, s.err
}

func TestFallbackRunsOnlyOnUsageLimit(t *testing.T) {
	limited := &stubRuntime{err: &usageLimitError{errors.New("usage limit")}}
	next := &stubRuntime{out: "from fallback"}
	f := &fallbackRuntime{names: []string{"codex", "claude"}, runtimes: []runtime{limited, next}}
	if out, err := f.Run(context.Background(), "", taskRequest{}); err != nil || out != "from fallback" {
		t.Fatalf("run: %q, %v", out, err)
	}
	failing := &stubRuntime{err: errors.New("broke")}
	next.calls = 0
	f = &fallbackRuntime{names: []string{"codex", "claude"}, runtimes: []runtime{failing, next}}
	if _, err := f.Run(context.Background(), "", taskRequest{}); err == nil || next.calls != 0 {
		t.Fatalf("a failure that is no usage limit went to the fallback: %v, %d calls", err, next.calls)
	}
	ok := &stubRuntime{out: "first"}
	next.calls = 0
	f = &fallbackRuntime{names: []string{"codex", "claude"}, runtimes: []runtime{ok, next}}
	if out, _ := f.Run(context.Background(), "", taskRequest{}); out != "first" || next.calls != 0 {
		t.Fatalf("run: %q, fallback calls %d", out, next.calls)
	}
	var card a2a.AgentCard
	f.Describe(&card)
	if card.Description != "stub first" || strings.Join(card.Skills[0].Tags, ",") != "stub,claude" {
		t.Errorf("card %q tags %v", card.Description, card.Skills[0].Tags)
	}
}

func TestUsageLimited(t *testing.T) {
	for _, s := range []string{"ERROR: You've hit your usage limit. Try again at 3pm", "Claude AI usage limit reached|1760000000", "You've hit your limit · resets 2pm", "429 rate_limit_error"} {
		if !usageLimited(s) {
			t.Errorf("%q is a usage limit", s)
		}
	}
	if usageLimited("permission denied") {
		t.Error("permission denied is no usage limit")
	}
}

func TestCheckFallback(t *testing.T) {
	c := &config{agent: "codex", agentFallback: "codex"}
	if err := checkFallback(c, false); err == nil || !strings.Contains(err.Error(), "agent's own runtime") {
		t.Errorf("same runtime: %v", err)
	}
	c.agentFallback = "nope"
	if err := checkFallback(c, false); err == nil || !strings.Contains(err.Error(), "unknown runtime") {
		t.Errorf("unknown runtime: %v", err)
	}
	c.agentFallback = ""
	if err := checkFallback(c, false); err != nil {
		t.Error(err)
	}
	if got := (&config{agent: "codex", agentFallback: "claude"}).agentRuntimes(); strings.Join(got, ",") != "codex,claude" {
		t.Errorf("agentRuntimes = %v", got)
	}
}
