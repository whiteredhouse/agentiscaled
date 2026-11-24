package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("AGENTISCALED_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func TestSingularCommandGroups(t *testing.T) {
	t.Setenv("AGENTISCALED_TEST_MAIN", "1")
	t.Setenv("PATH", t.TempDir())
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "agentiscaled.json"), []byte(`{"clients":"none"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		singular, plural string
		args             []string
		want             string
	}{
		{"agent", "agents", []string{"list", "-data-dir", dataDir}, "agentiscaled agent add"},
		{"client", "clients", []string{"list", "-data-dir", dataDir}, "codex"},
		{"runtime", "runtimes", []string{"list"}, "RUNTIME"},
	} {
		t.Run(tc.singular, func(t *testing.T) {
			out, err := exec.Command(os.Args[0], append([]string{tc.singular}, tc.args...)...).CombinedOutput()
			if err != nil || !strings.Contains(string(out), tc.want) {
				t.Errorf("%s %v: %v\n%s", tc.singular, tc.args, err, out)
			}
			out, err = exec.Command(os.Args[0], append([]string{tc.plural}, tc.args...)...).CombinedOutput()
			if err == nil || !strings.Contains(string(out), `unknown command "`+tc.plural+`"`) {
				t.Errorf("expected %s to be rejected as an unknown command, got %v\n%s", tc.plural, err, out)
			}
		})
	}
}

// The owner's controls moved from task to agent: the old spellings fail and
// name the new command, without touching the node.
func TestTaskControlsMovedToAgent(t *testing.T) {
	t.Setenv("AGENTISCALED_TEST_MAIN", "1")
	dataDir := t.TempDir()
	for _, command := range []string{"stop", "resume", "status"} {
		out, err := exec.Command(os.Args[0], "task", command, "-data-dir", dataDir).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "task "+command+" is now 'agentiscaled agent "+command+"'") {
			t.Errorf("task %s: %v\n%s", command, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "tasks-stopped")); !os.IsNotExist(err) {
		t.Fatalf("task stop wrote the stop marker: %v", err)
	}
}
