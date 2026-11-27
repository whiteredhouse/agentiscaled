package node

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiteredhouse/agentiscaled/skills"
)

// withSkillHome gives the test a home of its own, real skill targets in it,
// and a PATH with only the given agent CLIs on it.
func withSkillHome(t *testing.T, clis ...string) (claudeDir, codexDir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	bin := t.TempDir()
	for _, name := range clis {
		write(t, filepath.Join(bin, name), "#!/bin/sh\n")
		if err := os.Chmod(filepath.Join(bin, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	old := skillTargets
	skillTargets = findSkillTargets
	t.Cleanup(func() { skillTargets = old })
	return filepath.Join(home, ".claude", "skills", "agentiscale"), filepath.Join(home, ".agents", "skills", "agentiscale")
}

func skillFile(dir string) string { return filepath.Join(dir, "SKILL.md") }

func currentSkill(t *testing.T, dir string) bool {
	t.Helper()
	b, err := os.ReadFile(skillFile(dir))
	return err == nil && bytes.Equal(b, skills.Agentiscale)
}

func TestSkillTargets(t *testing.T) {
	claudeDir, codexDir := withSkillHome(t, "claude")
	got := findSkillTargets()
	if len(got) != 1 || got[0].runtime != "claude" || got[0].dir != claudeDir || got[0].installed {
		t.Fatalf("only claude on PATH: %+v", got)
	}
	// An installed copy is a target whether or not its CLI is found.
	write(t, skillFile(codexDir), "old")
	got = findSkillTargets()
	if len(got) != 2 || got[1].runtime != "codex" || !got[1].installed {
		t.Fatalf("codex skill installed: %+v", got)
	}
}

func TestSkillStep(t *testing.T) {
	for _, tc := range []struct {
		name                string
		step                skillStep
		wantClaude, wantCdx bool // the skill current there after the step
		wantOut             string
	}{
		// Without an answer, only the installed copy is updated.
		{"update", skillStep{}, false, true, "Codex: updated"},
		{"install", skillStep{install: true}, true, true, "Claude Code: installed"},
		{"leave", skillStep{leave: true}, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claudeDir, codexDir := withSkillHome(t, "claude")
			write(t, skillFile(codexDir), "old")
			s := tc.step
			s.targets = skillTargets()
			var out bytes.Buffer
			s.run(&out)
			if currentSkill(t, claudeDir) != tc.wantClaude || currentSkill(t, codexDir) != tc.wantCdx {
				t.Errorf("claude current %v, codex current %v; want %v, %v\n%s", currentSkill(t, claudeDir), currentSkill(t, codexDir), tc.wantClaude, tc.wantCdx, &out)
			}
			if !strings.Contains(out.String(), tc.wantOut) || tc.wantOut == "" && out.Len() != 0 {
				t.Errorf("output %q, want %q", &out, tc.wantOut)
			}
		})
	}
	t.Run("hint", func(t *testing.T) {
		withSkillHome(t, "codex")
		s := skillStep{targets: skillTargets()}
		var out bytes.Buffer
		s.run(&out)
		if !strings.Contains(out.String(), "Teach Codex to call your other agents with 'agentiscaled skill install'") {
			t.Errorf("no hint to install the skill: %q", &out)
		}
	})
	t.Run("no CLI found", func(t *testing.T) {
		claudeDir, codexDir := withSkillHome(t)
		s := skillStep{targets: skillTargets(), install: true}
		s.run(io.Discard)
		if !currentSkill(t, claudeDir) || !currentSkill(t, codexDir) {
			t.Error("-skill with no CLI found should install for both, as skill install does")
		}
	})
}

// upWithInput runs `up` on a node that has joined, as if in a terminal
// that answers input, and returns what it printed.
func upWithInput(t *testing.T, dir, input string, args ...string) string {
	t.Helper()
	stdin, err := os.CreateTemp(t.TempDir(), "wizard-input")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close() })
	if _, err := stdin.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	oldStdin, oldInteractive := os.Stdin, interactive
	os.Stdin, interactive = stdin, func() bool { return input != "" }
	t.Cleanup(func() { os.Stdin, interactive = oldStdin, oldInteractive })
	var out bytes.Buffer
	if err := RunUp(context.Background(), append([]string{"-foreground", "-clients", "none"}, args...), &out); err != nil {
		t.Fatalf("up %v: %v\n%s", args, err, &out)
	}
	return out.String()
}

func TestUpInstallsSkill(t *testing.T) {
	joined := func(t *testing.T) string {
		dir := defaultDataDir()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		mesh := newStandInMesh(t, "fine")
		pid := seedNode(t, dir, mesh.srv.URL, mesh.key)
		writeSettings(t, dir, settings{Control: "https://control.test", Listen: "/ip4/127.0.0.1/tcp/0", LogLevel: "info", PeerID: pid, NetworkID: "saved-network", MeshURL: mesh.srv.URL})
		return dir
	}
	t.Run("yes by default", func(t *testing.T) {
		claudeDir, codexDir := withSkillHome(t, "claude", "codex")
		dir := joined(t)
		// Enter at the skill, then no agent offered.
		out := upWithInput(t, dir, "\nn\n")
		if !strings.Contains(out, "Install the agentiscale skill for Claude Code and Codex") {
			t.Errorf("did not ask:\n%s", out)
		}
		if !currentSkill(t, claudeDir) || !currentSkill(t, codexDir) {
			t.Errorf("skill not installed:\n%s", out)
		}
		// Installed everywhere, a later up asks nothing about it.
		if out := upWithInput(t, dir, "n\n"); strings.Contains(out, "Install the agentiscale skill") {
			t.Errorf("asked again with the skill installed:\n%s", out)
		}
	})
	t.Run("no is kept", func(t *testing.T) {
		claudeDir, _ := withSkillHome(t, "claude")
		dir := joined(t)
		upWithInput(t, dir, "n\nn\n")
		if s, _ := loadSettings(dir); !s.NoSkill || currentSkill(t, claudeDir) {
			t.Fatalf("no to the skill: saved no_skill=%v, installed %v", s.NoSkill, currentSkill(t, claudeDir))
		}
		if out := upWithInput(t, dir, "n\n"); strings.Contains(out, "Install the agentiscale skill") {
			t.Errorf("asked again after a no:\n%s", out)
		}
		// -skill installs it and forgets the no.
		upWithInput(t, dir, "", "-skill")
		if s, _ := loadSettings(dir); s.NoSkill || !currentSkill(t, claudeDir) {
			t.Fatalf("-skill: saved no_skill=%v, installed %v", s.NoSkill, currentSkill(t, claudeDir))
		}
	})
	t.Run("without a terminal", func(t *testing.T) {
		claudeDir, codexDir := withSkillHome(t, "claude", "codex")
		dir := joined(t)
		write(t, skillFile(codexDir), "old")
		out := upWithInput(t, dir, "")
		if currentSkill(t, claudeDir) || !currentSkill(t, codexDir) {
			t.Errorf("without a terminal up should only update the installed copy:\n%s", out)
		}
		write(t, skillFile(codexDir), "old")
		upWithInput(t, dir, "", "-skill=false")
		if b, _ := os.ReadFile(skillFile(codexDir)); string(b) != "old" {
			t.Error("-skill=false changed the installed copy")
		}
	})
}
