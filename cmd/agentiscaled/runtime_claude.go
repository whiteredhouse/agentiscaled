package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// claudeRuntime does tasks with the Claude Code CLI in the agent's
// directory, one non-interactive run (`claude -p`) per task, under the
// selected local runtime configuration. It runs the unmodified Claude Code
// binary, signed in through Claude Code's own login; the node never reads
// or stores that sign-in.
type claudeRuntime struct {
	command string
	// config is the run's CLAUDE_CONFIG_DIR: empty with
	// policy.OwnerConfig, where Claude Code finds the owner's own as usual,
	// else the agent's own in the node's data directory, which holds its
	// own sign-in and none of the owner's skills, plugins or settings.
	config string
	policy agentPolicy
	// extra goes before the prompt; tests use it.
	extra []string
}

// claudeEnvCredentials are the variables Claude Code takes a credential
// from before its sign-in. Only the owner's configuration may use them.
var claudeEnvCredentials = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

func newClaudeRuntime(command, dataDir string, policy agentPolicy) (runtime, error) {
	if command == "" {
		command = "claude"
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("the Claude Code CLI %q was not found: %w", command, err)
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("%s --version: %w", path, err)
	}
	log.Printf("claude runtime uses %s", strings.TrimSpace(string(out)))
	config, err := claudeConfigDir(dataDir, policy)
	if err != nil {
		return nil, err
	}
	if policy.OwnerConfig {
		if !claudeSignedIn(path, dataDir, policy) && !claudeEnvSignedIn(policy) {
			return nil, fmt.Errorf("your Claude Code is not signed in: run\n  %s auth login", path)
		}
	} else {
		if err := os.MkdirAll(config, 0o700); err != nil {
			return nil, err
		}
		if !claudeSignedIn(path, dataDir, policy) {
			return nil, fmt.Errorf("the agent's Claude Code is not signed in. With -agent-owner-config=false it signs in apart from your own Claude Code, so that a remote request cannot reach your Claude Code skills, plugins or settings: run\n  CLAUDE_CONFIG_DIR=%s %s auth login", config, path)
		}
	}
	return &claudeRuntime{command: path, config: config, policy: policy}, nil
}

// claudeConfigDir is the run's CLAUDE_CONFIG_DIR: empty with
// policy.OwnerConfig, so the owner's applies as Claude Code finds it, else
// the agent's own in the node's data directory.
func claudeConfigDir(dataDir string, policy agentPolicy) (string, error) {
	if policy.OwnerConfig {
		return "", nil
	}
	return filepath.Abs(filepath.Join(dataDir, "claude"))
}

// claudeSignedIn asks Claude Code whether it is signed in, in a way the
// background service also sees: without a credential from this shell's
// environment.
func claudeSignedIn(command, dataDir string, policy agentPolicy) bool {
	if command == "" {
		command = "claude"
	}
	config, err := claudeConfigDir(dataDir, policy)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, "auth", "status", "--json")
	if policy.OwnerConfig {
		cmd.Env = withoutEnv(os.Environ(), claudeEnvCredentials)
	} else {
		cmd.Env = claudeEnv(os.Environ(), config)
	}
	// It may exit non-zero when signed out; the answer is in its output.
	out, _ := cmd.Output()
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	return json.Unmarshal(out, &status) == nil && status.LoggedIn
}

// claudeEnvSignedIn says whether the owner's Claude Code has a credential in
// the environment. It counts only with the owner's configuration: the
// agent's own never gets the node's credentials.
func claudeEnvSignedIn(policy agentPolicy) bool {
	if !policy.OwnerConfig {
		return false
	}
	for _, name := range claudeEnvCredentials {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

// claudeLogin is `claude auth login`, Claude Code's own sign-in flow: the
// owner's own with policy.OwnerConfig, else for the agent's own
// CLAUDE_CONFIG_DIR, which it creates first.
func claudeLogin(ctx context.Context, command, dataDir string, policy agentPolicy) (*exec.Cmd, string) {
	if policy.OwnerConfig {
		return exec.CommandContext(ctx, command, "auth", "login"), shellQuote(command) + " auth login"
	}
	config, err := claudeConfigDir(dataDir, policy)
	if err == nil {
		err = os.MkdirAll(config, 0o700)
	}
	if err != nil {
		config = filepath.Join(dataDir, "claude")
	}
	cmd := exec.CommandContext(ctx, command, "auth", "login")
	cmd.Env = append(withoutEnv(os.Environ(), append([]string{"CLAUDE_CONFIG_DIR"}, claudeEnvCredentials...)), "CLAUDE_CONFIG_DIR="+config)
	return cmd, "CLAUDE_CONFIG_DIR=" + shellQuote(config) + " " + shellQuote(command) + " auth login"
}

func (r *claudeRuntime) Describe(card *a2a.AgentCard) {
	card.Description = "A Claude Code agent on this node that works in its configured directory under local runtime permissions. Send one text message with the task; it answers with the result."
	card.Skills = []a2a.AgentSkill{{
		ID:          "task",
		Name:        "Task",
		Description: "Does a task in the agent's directory within its configured permissions.",
		Tags:        []string{"claude"},
		Examples:    []string{"Implement the change described in the task", "Review the uncommitted changes", "Explain how requests are authenticated"},
	}}
}

// Run runs `claude -p` in dir:
//   - With the owner's configuration (agentPolicy.OwnerConfig, the
//     default), Claude Code runs as the owner's own would: their sign-in,
//     settings, permissions, skills, plugins and MCP servers. Nothing can
//     prompt for approval: what would prompt is denied.
//   - Otherwise CLAUDE_CONFIG_DIR is the agent's own and the run is
//     restricted (--restricted): no user, project or local settings, no MCP
//     servers, and only the read-only file tools, confined to the agent's
//     directory. No credential in the node's environment reaches it
//     (claudeEnv).
//   - The directory's CLAUDE.md files load unless the owner's policy turns
//     them off, and its skills unless the policy turns them off, which turns
//     off every skill and command of the run, since Claude Code cannot turn
//     off only the directory's.
func (r *claudeRuntime) Run(ctx context.Context, dir string, req taskRequest) (string, error) {
	args := []string{"-p",
		"--output-format", "json",
		"--no-session-persistence",
		"--permission-prompts", "none"}
	if !r.policy.OwnerConfig {
		args = append(args,
			"--restricted",
			"--strict-mcp-config",
			"--tools", "Read,Grep,Glob",
			"--permission-mode", "dontAsk")
	}
	if !r.policy.DirSkills {
		args = append(args, "--disable-slash-commands")
	}
	args = append(args, r.extra...)
	cmd := exec.CommandContext(ctx, r.command, args...)
	cmd.Dir = dir
	if r.policy.OwnerConfig {
		cmd.Env = os.Environ()
	} else {
		cmd.Env = claudeEnv(os.Environ(), r.config)
	}
	if !r.policy.DirInstructions {
		cmd.Env = append(cmd.Env, "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1")
	}
	if !r.policy.DirSkills {
		cmd.Env = append(cmd.Env, "CLAUDE_CODE_DISABLE_BUNDLED_SKILLS=1")
	}
	cmd.Stdin = strings.NewReader(taskPrompt(req))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	// The result, a failure included, is in the JSON on stdout, whatever
	// the exit status.
	var result struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		if runErr != nil {
			return "", fmt.Errorf("claude -p: %v: %s", runErr, failureLines(stderr.String(), 3))
		}
		return "", fmt.Errorf("claude -p: unreadable result: %v", err)
	}
	if result.IsError || runErr != nil {
		return "", fmt.Errorf("claude -p: %s", strings.TrimSpace(result.Result+" "+failureLines(stderr.String(), 3)))
	}
	return strings.TrimSpace(result.Result), nil
}

// claudeEnv is the run's environment: of the node's, only what a process
// needs to run and reach the network, and the agent's own CLAUDE_CONFIG_DIR,
// so that no credential of the owner's, such as ANTHROPIC_API_KEY, which
// Claude Code prefers to its sign-in, comes along. HOME stays, since on
// macOS the sign-in is in the user's keychain, under a name of the config
// directory's.
func claudeEnv(environ []string, config string) []string {
	keep := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true,
		"LANG": true, "TERM": true, "TZ": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
		"NODE_EXTRA_CA_CERTS": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	}
	env := []string{"CLAUDE_CONFIG_DIR=" + config}
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if keep[strings.ToUpper(k)] || strings.HasPrefix(k, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}

// withoutEnv is environ without the named variables.
func withoutEnv(environ, names []string) []string {
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	var env []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			env = append(env, kv)
		}
	}
	return env
}
