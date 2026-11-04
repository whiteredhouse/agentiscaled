package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// Re-running up must not silently remove the hosted agent or reset node flags.
func TestUpPreservesHostedAgent(t *testing.T) {
	dir := withDefaultDataDir(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	mesh := newStandInMesh(t, "fine")
	pid := seedNode(t, dir, mesh.srv.URL, mesh.key)
	command := filepath.Join(t.TempDir(), "codex")
	write(t, command, "#!/bin/sh\necho codex-cli-test\n")
	if err := os.Chmod(command, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "codex", "auth.json"), "{}")
	want := settings{Control: "https://control.test", Listen: "/ip4/127.0.0.1/tcp/0", LogLevel: "debug", Agent: "codex", AgentDir: t.TempDir(), AgentCommand: command, AgentName: "helper", AgentDescription: "Helps", AgentOwnConfig: true, AgentIgnoreDirSkills: true, AgentEnv: []string{"CODEX_HOME=/saved/config"}, PeerID: pid, NetworkID: "saved-network", MeshURL: mesh.srv.URL}
	writeSettings(t, dir, want)
	var out bytes.Buffer
	for i := 0; i < 2; i++ {
		if err := runUp(context.Background(), []string{"-y", "-foreground", "-clients", "none"}, &out); err != nil {
			t.Fatal(err)
		}
		got, err := loadSettings(dir)
		if err != nil {
			t.Fatal(err)
		}
		got.Path, want.Path = "", ""
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("up changed settings:\ngot %+v\nwant %+v", got, want)
		}
	}
}

func TestDefaultClientsIncludeHostedNode(t *testing.T) {
	if len(selectClients(clientsAuto)) == 0 {
		t.Fatal("hosting an agent must not disable default local clients")
	}
	w, _ := testWizard("\n\n")
	got, err := w.chooseClients([]string{"claude-desktop", "codex"})
	if err != nil || got != "claude-desktop,codex" {
		t.Fatalf("clients = %q, %v", got, err)
	}
}

func TestWizardHostsAgentOnSelectedNode(t *testing.T) {
	dir := withDefaultDataDir(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	mesh := newStandInMesh(t, "fine")
	pid := seedNode(t, dir, mesh.srv.URL, mesh.key)
	bin := t.TempDir()
	command := filepath.Join(bin, "codex")
	write(t, command, "#!/bin/sh\necho codex-cli-test\n")
	if err := os.Chmod(command, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "owner-codex"))
	write(t, filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), "{}")
	// An old independent node must remain untouched.
	oldDir := filepath.Join(filepath.Dir(dir), "agentiscale-codex-2")
	write(t, filepath.Join(oldDir, settingsFile), "legacy node")
	workspace := t.TempDir()
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.WriteString("y\n\n\n" + workspace + "\n\n\n\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	oldStdin, oldInteractive, oldRun := os.Stdin, interactive, runAttached
	os.Stdin, interactive = input, func() bool { return true }
	runAttached = func(_ context.Context, cmd *exec.Cmd) error {
		t.Fatalf("setup launched another process: %v", cmd.Args)
		return nil
	}
	t.Cleanup(func() { os.Stdin, interactive, runAttached = oldStdin, oldInteractive, oldRun })
	var out bytes.Buffer
	if err := runUp(context.Background(), []string{"-foreground", "-clients", "none"}, &out); err != nil {
		t.Fatal(err)
	}
	got, err := loadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.PeerID != pid || got.Agent != "codex" || got.AgentDir != workspace || got.AgentName != "codex" || got.AgentOwnConfig {
		t.Fatalf("selected node settings: %+v", got)
	}
	legacy, err := os.ReadFile(filepath.Join(oldDir, settingsFile))
	if err != nil || string(legacy) != "legacy node" {
		t.Fatalf("legacy node changed: %q %v", legacy, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "agentiscale-codex")); !os.IsNotExist(err) {
		t.Fatalf("created a second node: %v", err)
	}
}

func TestUpPreservesDefaultOwnerConfig(t *testing.T) {
	dir := withDefaultDataDir(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	mesh := newStandInMesh(t, "fine")
	pid := seedNode(t, dir, mesh.srv.URL, mesh.key)
	command := filepath.Join(t.TempDir(), "codex")
	write(t, command, "#!/bin/sh\necho codex-cli-test\n")
	if err := os.Chmod(command, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(os.Getenv("HOME"), ".codex", "auth.json"), "{}")
	original := settings{Control: "https://control.test", Listen: "/ip4/127.0.0.1/tcp/0", LogLevel: "info", Agent: "codex", AgentDir: t.TempDir(), AgentCommand: command, AgentName: "helper", PeerID: pid, MeshURL: mesh.srv.URL}
	writeSettings(t, dir, original)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "different-owner"))
	if err := runUp(context.Background(), []string{"-y", "-foreground", "-clients", "none"}, io.Discard); err != nil {
		t.Fatalf("up switched away from the saved default owner configuration: %v", err)
	}
	got, err := loadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.AgentEnv) != 0 {
		t.Fatalf("captured another owner configuration: %v", got.AgentEnv)
	}
	// Explicit values still override the saved values, including false and empty.
	if err := runUp(context.Background(), []string{"-y", "-foreground", "-clients", "none", "-agent=", "-bind-addr=", "-agent-dir-instructions=false"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err = loadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "" || got.BindAddr != "" || !got.AgentIgnoreDirInstructions || got.AgentDir != original.AgentDir {
		t.Fatalf("explicit overrides: %+v", got)
	}
}
