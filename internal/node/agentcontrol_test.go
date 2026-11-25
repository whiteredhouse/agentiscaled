package node

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/whiteredhouse/agentiscaled/internal/nodestate"
)

func TestAgentControls(t *testing.T) {
	dir := t.TempDir()
	writeSettings(t, dir, settings{Control: "https://control.test", Agents: []agentConfig{
		{Runtime: "codex", Name: "helper", Dir: t.TempDir()},
		{Runtime: "claude", Name: "reviewer", Dir: t.TempDir()},
	}})
	var out bytes.Buffer
	agent := func(args ...string) string {
		t.Helper()
		out.Reset()
		if err := RunAgent(t.Context(), append([]string{args[0], "-data-dir", dir}, args[1:]...), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	stamp := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	helperFile := nodestate.AgentStopFilePath(agentStateDir(dir, "helper"))

	if got := agent("status"); got != "Task execution enabled\n" {
		t.Fatalf("agent status on an offline node: %q", got)
	}
	if got := agent("stop"); got != "stopped all agents on this node: running tasks are cancelled and new tasks are refused\n" {
		t.Fatalf("agent stop: %q", got)
	}
	if got := agent("status"); got != "All agents stopped since "+stamp(stopFilePath(dir))+"\n" {
		t.Fatalf("agent status after agent stop: %q", got)
	}

	// One agent's stop is its own: resuming the node leaves it stopped.
	if got := agent("stop", "helper"); got != "stopped helper: its running task is cancelled and new tasks are refused\n" {
		t.Fatalf("agent stop helper: %q", got)
	}
	if got := agent("status"); got != "All agents stopped since "+stamp(stopFilePath(dir))+"\nhelper stopped since "+stamp(helperFile)+"\n" {
		t.Fatalf("agent status with both stopped: %q", got)
	}
	if got := agent("resume"); got != "resumed all agents on this node\nhelper stays stopped: agentiscaled agent resume helper\n" {
		t.Fatalf("agent resume: %q", got)
	}
	if got := agent("status"); got != "helper stopped since "+stamp(helperFile)+"\n" {
		t.Fatalf("agent status with helper stopped: %q", got)
	}
	if got := agent("resume", "helper"); got != "resumed helper\n" {
		t.Fatalf("agent resume helper: %q", got)
	}
	if got := agent("status"); got != "Task execution enabled\n" {
		t.Fatalf("agent status after agent resume: %q", got)
	}

	// Resuming one agent while the node is stopped says the node stop holds.
	agent("stop")
	if got := agent("resume", "reviewer"); got != "resumed reviewer, but all agents on this node stay stopped: agentiscaled agent resume\n" {
		t.Fatalf("agent resume reviewer on a stopped node: %q", got)
	}
}

func TestAgentControlsRejectUnknownAgent(t *testing.T) {
	dir := t.TempDir()
	writeSettings(t, dir, settings{Control: "https://control.test", Agents: []agentConfig{{Runtime: "codex", Name: "helper", Dir: t.TempDir()}}})
	for _, args := range [][]string{{"stop", "other"}, {"resume", "other"}, {"status", "helper"}, {"stop", "helper", "other"}} {
		if err := RunAgent(t.Context(), append([]string{args[0], "-data-dir", dir}, args[1:]...), io.Discard); err == nil {
			t.Errorf("agent %v: accepted", args)
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
}
