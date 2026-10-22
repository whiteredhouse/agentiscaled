package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A login shell whose startup files print noise still gives its environment.
func TestLoginShellEnv(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "sh")
	// Stands in for a shell whose startup files export a token and print a
	// banner, then runs the command it is given.
	script := "#!/bin/sh\necho 'Welcome back'\nexport GITHUB_TOKEN=from-shell\nshift 3\nexec /bin/sh -c \"$1\"\n"
	if err := os.WriteFile(shell, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
	env, err := loginShellEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !contains(env, "GITHUB_TOKEN=from-shell") {
		t.Errorf("environment lacks the shell's token: %q", env)
	}
	for _, kv := range env {
		if strings.Contains(kv, "Welcome") {
			t.Errorf("startup output leaked into %q", kv)
		}
	}

	// A shell that hangs is given up on.
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := loginShellTimeout
	loginShellTimeout = 200 * time.Millisecond
	t.Cleanup(func() { loginShellTimeout = old })
	start := time.Now()
	if _, err := loginShellEnv(context.Background()); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("hanging shell: %v after %s", err, time.Since(start))
	}
}

// The service keeps its own variables, gains the shell's others, and
// agent.env has the last word.
func TestOwnerEnv(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "sh")
	script := "#!/bin/sh\nexport GITHUB_TOKEN=from-shell AGENTISCALE_TEST_KEEP=from-shell AGENTISCALE_TEST_FILE=from-shell\nshift 3\nexec /bin/sh -c \"$1\"\n"
	if err := os.WriteFile(shell, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
	t.Setenv("GITHUB_TOKEN", "")
	os.Unsetenv("GITHUB_TOKEN")
	t.Setenv("AGENTISCALE_TEST_KEEP", "from-service")
	t.Setenv("AGENTISCALE_TEST_FILE", "from-service")
	envFile := filepath.Join(dir, agentEnvFile)
	if err := os.WriteFile(envFile, []byte("# tokens for MCP servers\n\nAGENTISCALE_TEST_FILE=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ownerEnv(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"GITHUB_TOKEN": "from-shell", "AGENTISCALE_TEST_KEEP": "from-service", "AGENTISCALE_TEST_FILE": "from-file"} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	if err := os.Chmod(envFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ownerEnv(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("readable agent.env: %v", err)
	}
	if err := os.WriteFile(envFile, []byte("not a variable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ownerEnv(context.Background(), dir); err == nil {
		t.Error("a malformed agent.env was accepted")
	}
}
