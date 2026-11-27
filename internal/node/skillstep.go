package node

import (
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/whiteredhouse/agentiscaled/internal/caller"
	"github.com/whiteredhouse/agentiscaled/internal/cli"
	"github.com/whiteredhouse/agentiscaled/skills"
)

// skillTarget is a runtime's user skills directory that `up` may keep the
// agentiscale skill in.
type skillTarget struct {
	runtime   string // claude or codex
	dir       string
	installed bool
	// cli is whether the runtime's command is on PATH.
	cli bool
}

// skillRuntimeNames name the skill's runtimes as the owner knows them.
var skillRuntimeNames = map[string]string{"claude": "Claude Code", "codex": "Codex"}

// skillTargets finds the skill's targets; tests replace it.
var skillTargets = findSkillTargets

// findSkillTargets are the user skills directories of the skill's runtimes
// that already hold the skill or whose CLI is installed.
func findSkillTargets() []skillTarget {
	var targets []skillTarget
	for _, rt := range caller.SkillRuntimes() {
		dir, err := caller.UserSkillDir(rt)
		if err != nil {
			continue
		}
		_, err = exec.LookPath(runtimes[rt].defaultCommand)
		t := skillTarget{runtime: rt, dir: dir, installed: caller.SkillInstalled(dir), cli: err == nil}
		if t.installed || t.cli {
			targets = append(targets, t)
		}
	}
	return targets
}

// skillStep is what `up` does with the agentiscale skill: install it into
// every target when the owner asked for it this time, and otherwise keep
// the copies already installed current with this agentiscaled.
type skillStep struct {
	targets []skillTarget
	install bool
	// leave is -skill=false: install nothing and update nothing.
	leave bool
}

// missing are the runtimes of the targets without the skill.
func (s *skillStep) missing() []string {
	var names []string
	for _, t := range s.targets {
		if !t.installed {
			names = append(names, skillRuntimeNames[t.runtime])
		}
	}
	return names
}

// offerSkill asks whether to install the skill for the runtimes without it,
// yes by default.
func (w *wizard) offerSkill(runtimes []string) (bool, error) {
	they := "they know"
	if len(runtimes) == 1 {
		they = "it knows"
	}
	yes, err := w.yesNo("\nInstall the agentiscale skill for "+strings.Join(runtimes, " and ")+", so that "+they+" when and how to call your other agents?", true)
	return yes, ignoreEOF(err)
}

// run installs or updates the skill and prints what it did. A failure
// leaves the node as it is and says how to install the skill by hand.
func (s *skillStep) run(out io.Writer) {
	if s.leave {
		return
	}
	targets := s.targets
	if s.install && !slices.ContainsFunc(targets, func(t skillTarget) bool { return t.cli }) {
		// Asked for with neither CLI found: install for both, as
		// `skill install` does, whichever already has a copy.
		for _, rt := range caller.SkillRuntimes() {
			if slices.ContainsFunc(targets, func(t skillTarget) bool { return t.runtime == rt }) {
				continue
			}
			if dir, err := caller.UserSkillDir(rt); err == nil {
				targets = append(targets, skillTarget{runtime: rt, dir: dir})
			}
		}
	}
	printed, have := false, false
	for _, t := range targets {
		// Look again now: the sign-in and setup since the targets were
		// found take time, in which another installer may have written
		// a copy.
		installed := caller.SkillInstalled(t.dir)
		if !installed && !s.install {
			continue
		}
		if !printed {
			fmt.Fprintln(out, "\nThe agentiscale skill:")
			printed = true
		}
		name, shown := skillRuntimeNames[t.runtime], cli.HomeRelative(t.dir)
		if link := skillLink(t.dir); link != "" {
			// Something else manages the skill there, such as a skill
			// installer or the owner's dotfiles: writing would follow
			// the link, or replace it with a file.
			have = true
			fmt.Fprintf(out, "  %s: %s is a link to %s, which something else manages; left it as it is.\n", name, cli.HomeRelative(link), cli.HomeRelative(skillLinkTarget(link)))
			continue
		}
		if installed && !caller.SkillManaged(t.dir) {
			// A copy agentiscaled did not install, such as one a skill
			// installer keeps, is left to whoever installed it.
			have = true
			fmt.Fprintf(out, "  %s: %s was not installed by agentiscaled; left it as it is. 'agentiscaled skill install %s' replaces it with this version, which up then keeps current.\n", name, shown, t.runtime)
			continue
		}
		did, err := caller.InstallSkillDir(t.dir)
		if err != nil {
			fmt.Fprintf(out, "  %s: %v. Install it yourself with 'agentiscaled skill install %s'.\n", skillRuntimeNames[t.runtime], err, t.runtime)
			continue
		}
		have = true
		fmt.Fprintf(out, "  %s: %s %s\n", skillRuntimeNames[t.runtime], did, cli.HomeRelative(filepath.Join(t.dir, "SKILL.md")))
	}
	if !have && len(s.missing()) > 0 {
		fmt.Fprintln(out, "\nTeach "+strings.Join(s.missing(), " and ")+" to call your other agents with 'agentiscaled skill install'.")
	}
}

// skillLink is the first symbolic link among the skill's directory and the
// skill's files in it, or "" when there is none. Links above the directory,
// such as a skills directory in the owner's dotfiles, do not matter: the
// marker in the directory says who installed the copy, wherever it is.
func skillLink(dir string) string {
	paths := []string{dir}
	_ = iofs.WalkDir(skills.AgentiscaleFiles, ".", func(name string, d iofs.DirEntry, err error) error {
		if err == nil && name != "." {
			paths = append(paths, filepath.Join(dir, filepath.FromSlash(name)))
		}
		return err
	})
	for _, p := range paths {
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return p
		}
	}
	return ""
}

// skillLinkTarget is where a link leads, or the link itself when that
// cannot be read.
func skillLinkTarget(link string) string {
	if target, err := filepath.EvalSymlinks(link); err == nil {
		return target
	}
	return link
}
