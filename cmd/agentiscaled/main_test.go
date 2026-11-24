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
		{"task", "tasks", []string{"status", "-data-dir", dataDir}, "running"},
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
