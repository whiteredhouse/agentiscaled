package node

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestAgentControlAliases(t *testing.T) {
	dir := t.TempDir()
	var out, warnings bytes.Buffer
	agent := func(command string) string {
		t.Helper()
		out.Reset()
		if err := RunAgent(t.Context(), []string{command, "-data-dir", dir}, &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	task := func(command string) string {
		t.Helper()
		out.Reset()
		warnings.Reset()
		if err := RunLegacyTaskControl([]string{command, "-data-dir", dir}, &out, &warnings); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(warnings.String(), "deprecated") || !strings.Contains(warnings.String(), "agentiscaled agent "+command) {
			t.Fatalf("task %s warning: %q", command, &warnings)
		}
		return out.String()
	}

	if got := agent("status"); got != "Task execution enabled\n" {
		t.Fatalf("agent status on an offline node: %q", got)
	}
	if got := task("stop"); got != "task execution stopped: the running task is cancelled and new tasks are refused\n" {
		t.Fatalf("task stop: %q", got)
	}
	b, err := os.ReadFile(stopFilePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := agent("status"); got != "Task execution stopped since "+string(b) {
		t.Fatalf("agent status after task stop: %q", got)
	}
	agent("resume")
	if got := task("status"); got != "running\n" {
		t.Fatalf("task status after agent resume: %q", got)
	}
	agent("stop")
	if got := task("status"); !strings.HasPrefix(got, "stopped since ") {
		t.Fatalf("task status after agent stop: %q", got)
	}
	if got := task("resume"); got != "task execution resumed\n" {
		t.Fatalf("task resume: %q", got)
	}
	if got := agent("status"); got != "Task execution enabled\n" {
		t.Fatalf("agent status after task resume: %q", got)
	}
}

func TestAgentControlsRejectAgentName(t *testing.T) {
	dir := t.TempDir()
	for _, command := range []string{"stop", "resume", "status"} {
		err := RunAgent(t.Context(), []string{command, "-data-dir", dir, "helper"}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "all agents") {
			t.Errorf("agent %s helper: %v", command, err)
		}
	}
	if _, err := os.Stat(stopFilePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("unexpected stop marker: %v", err)
	}
}

func TestAgentStatusMarkerReadError(t *testing.T) {
	dir := t.TempDir()
	// A directory in place of the marker produces a read error even as root.
	if err := os.Mkdir(stopFilePath(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RunAgent(t.Context(), []string{"status", "-data-dir", dir}, &out); err == nil || out.Len() > 0 {
		t.Fatalf("agent status for unreadable marker: %v, stdout %q", err, &out)
	}
	if err := RunLegacyTaskControl([]string{"status", "-data-dir", dir}, &out, io.Discard); err == nil || out.Len() > 0 {
		t.Fatalf("task status for unreadable marker: %v, stdout %q", err, &out)
	}
}
