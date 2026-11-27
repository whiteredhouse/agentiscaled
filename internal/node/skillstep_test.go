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

// staleSkill puts an old copy of the skill in dir, as an earlier
// agentiscaled installed it.
func staleSkill(t *testing.T, dir string) {
	t.Helper()
	staleSkillAs(t, dir, "old")
}

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
			staleSkill(t, codexDir)
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
	t.Run("no CLI found, one copy", func(t *testing.T) {
		claudeDir, codexDir := withSkillHome(t)
		staleSkill(t, codexDir)
		s := skillStep{targets: skillTargets(), install: true}
		s.run(io.Discard)
		if !currentSkill(t, claudeDir) || !currentSkill(t, codexDir) {
			t.Error("an existing copy must not keep -skill from installing for both")
		}
	})
}

// up never writes through a link: a linked skill is managed elsewhere.
func TestSkillStepLeavesLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(t *testing.T, codexDir, managed string)
	}{
		{"directory", func(t *testing.T, codexDir, managed string) {
			if err := os.MkdirAll(filepath.Dir(codexDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(managed, codexDir); err != nil {
				t.Fatal(err)
			}
		}},
		{"file", func(t *testing.T, codexDir, managed string) {
			if err := os.MkdirAll(codexDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(skillFile(managed), skillFile(codexDir)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, codexDir := withSkillHome(t, "codex")
			managed := t.TempDir()
			write(t, skillFile(managed), "managed")
			tc.link(t, codexDir, managed)
			for _, install := range []bool{false, true} {
				s := skillStep{targets: skillTargets(), install: install}
				var out bytes.Buffer
				s.run(&out)
				if b, _ := os.ReadFile(skillFile(managed)); string(b) != "managed" {
					t.Fatalf("install=%v wrote through the link:\n%s", install, &out)
				}
				if fi, err := os.Lstat(skillFile(codexDir)); tc.name == "file" && (err != nil || fi.Mode()&os.ModeSymlink == 0) {
					t.Fatalf("install=%v replaced the linked file", install)
				}
				if !strings.Contains(out.String(), "left it as it is") {
					t.Errorf("install=%v did not say it left the link: %q", install, &out)
				}
			}
		})
	}
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
		staleSkill(t, codexDir)
		out := upWithInput(t, dir, "")
		if currentSkill(t, claudeDir) || !currentSkill(t, codexDir) {
			t.Errorf("without a terminal up should only update the installed copy:\n%s", out)
		}
		staleSkill(t, codexDir)
		upWithInput(t, dir, "", "-skill=false")
		if b, _ := os.ReadFile(skillFile(codexDir)); string(b) != "old" {
			t.Error("-skill=false changed the installed copy")
		}
		// -skill=false is for that run only: a later up in a terminal
		// still asks.
		if s, _ := loadSettings(dir); s.NoSkill {
			t.Error("-skill=false saved a no")
		}
		if out := upWithInput(t, dir, "n\nn\n"); !strings.Contains(out, "Install the agentiscale skill for Claude Code, so that it knows") {
			t.Errorf("no question after -skill=false:\n%s", out)
		}
	})
}

// up updates only copies agentiscaled installed, and never writes through a
// link anywhere on the way to them.
func TestSkillStepLeavesWhatItDidNotInstall(t *testing.T) {
	untouched := func(t *testing.T, s skillStep, files ...string) string {
		t.Helper()
		var out bytes.Buffer
		s.run(&out)
		for _, f := range files {
			if b, _ := os.ReadFile(f); string(b) != "managed" {
				t.Errorf("install=%v changed %s:\n%s", s.install, f, &out)
			}
		}
		return out.String()
	}
	t.Run("copy without the marker", func(t *testing.T) {
		_, codexDir := withSkillHome(t, "codex")
		write(t, skillFile(codexDir), "managed")
		for _, install := range []bool{false, true} {
			out := untouched(t, skillStep{targets: skillTargets(), install: install}, skillFile(codexDir))
			if !strings.Contains(out, "was not installed by agentiscaled; left it as it is") {
				t.Errorf("did not say it left the copy: %q", out)
			}
		}
	})
	t.Run("linked skills directory", func(t *testing.T) {
		// A copy someone else installed is left alone wherever it is;
		// one agentiscaled installed is its to update, also through a
		// link above it, such as to the owner's dotfiles.
		_, codexDir := withSkillHome(t, "codex")
		managed := t.TempDir()
		write(t, skillFile(filepath.Join(managed, "agentiscale")), "managed")
		if err := os.MkdirAll(filepath.Dir(filepath.Dir(codexDir)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(managed, filepath.Dir(codexDir)); err != nil {
			t.Fatal(err)
		}
		for _, install := range []bool{false, true} {
			untouched(t, skillStep{targets: skillTargets(), install: install}, skillFile(filepath.Join(managed, "agentiscale")))
		}
		staleSkill(t, filepath.Join(managed, "agentiscale"))
		(&skillStep{targets: skillTargets()}).run(io.Discard)
		if !currentSkill(t, filepath.Join(managed, "agentiscale")) {
			t.Error("a marked copy behind a linked skills directory was not updated")
		}
	})
	t.Run("copy written after the targets were found", func(t *testing.T) {
		_, codexDir := withSkillHome(t, "codex")
		s := skillStep{targets: skillTargets(), install: true}
		write(t, skillFile(codexDir), "managed")
		out := untouched(t, s, skillFile(codexDir))
		if !strings.Contains(out, "was not installed by agentiscaled") {
			t.Errorf("did not say it left the copy: %q", out)
		}
		if _, err := os.Stat(filepath.Join(codexDir, ".agentiscaled")); err == nil {
			t.Error("marked another installer's copy as its own")
		}
	})
	t.Run("one copy shared by a link", func(t *testing.T) {
		// As skill installers lay it out: the copy in ~/.agents/skills,
		// linked from ~/.claude/skills.
		claudeDir, codexDir := withSkillHome(t, "claude", "codex")
		write(t, skillFile(codexDir), "managed")
		if err := os.MkdirAll(filepath.Dir(claudeDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(codexDir, claudeDir); err != nil {
			t.Fatal(err)
		}
		for _, install := range []bool{false, true} {
			untouched(t, skillStep{targets: skillTargets(), install: install}, skillFile(codexDir))
		}
	})
}

// staleSkillAs puts a copy that agentiscaled installed in dir, with the
// given SKILL.md.
func staleSkillAs(t *testing.T, dir, text string) {
	t.Helper()
	write(t, skillFile(dir), text)
	write(t, filepath.Join(dir, ".agentiscaled"), "")
}
