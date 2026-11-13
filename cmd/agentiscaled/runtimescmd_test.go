package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimesList(t *testing.T) {
	home := t.TempDir()
	bin := t.TempDir()
	codex := filepath.Join(bin, "codex")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\n: > \"$HOME/codex-ran\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AGENTISCALED_TEST_MAIN", "1")
	for _, tc := range []struct {
		name, codexHome, claudeHome, wantCodex, wantClaude string
	}{
		{"defaults", "", "", "~/.codex", "~/.claude"},
		{"overrides", filepath.Join(home, "codex config"), filepath.Join(home, "claude config"), "~/codex config", "~/claude config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", tc.codexHome)
			t.Setenv("CLAUDE_CONFIG_DIR", tc.claudeHome)
			out, err := exec.Command(os.Args[0], "runtimes", "list").CombinedOutput()
			if err != nil {
				t.Fatalf("runtimes list: %v\n%s", err, out)
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(lines) != 3 || strings.Join(strings.Fields(lines[0]), " ") != "RUNTIME COMMAND EXECUTABLE CONFIG DIR" {
				t.Fatalf("unexpected table:\n%s", out)
			}
			if got := strings.Join(strings.Fields(lines[1]), " "); got != "claude claude not found on PATH "+tc.wantClaude {
				t.Errorf("Claude row: %s", lines[1])
			}
			if got := strings.Join(strings.Fields(lines[2]), " "); got != "codex codex "+codex+" "+tc.wantCodex {
				t.Errorf("Codex row: %s", lines[2])
			}
		})
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Errorf("listing should leave the home directory empty, got %v (%v)", entries, err)
	}
}
