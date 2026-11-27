package node

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/whiteredhouse/agentiscaled/internal/caller"
	"github.com/whiteredhouse/agentiscaled/internal/cli"
)

// skillTarget is a runtime's user skills directory that `up` may keep the
// agentiscale skill in.
type skillTarget struct {
	runtime   string // claude or codex
	dir       string
	installed bool
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
		t := skillTarget{runtime: rt, dir: dir, installed: caller.SkillInstalled(dir)}
		if !t.installed {
			if _, err := exec.LookPath(runtimes[rt].defaultCommand); err != nil {
				continue
			}
		}
		targets = append(targets, t)
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
	yes, err := w.yesNo("\nInstall the agentiscale skill for "+strings.Join(runtimes, " and ")+", so that they know when and how to call your other agents?", true)
	return yes, ignoreEOF(err)
}

// run installs or updates the skill and prints what it did. A failure
// leaves the node as it is and says how to install the skill by hand.
func (s *skillStep) run(out io.Writer) {
	if s.leave {
		return
	}
	targets := s.targets
	if s.install && len(targets) == 0 {
		// Asked for with neither CLI found: install for both, as
		// `skill install` does.
		for _, rt := range caller.SkillRuntimes() {
			if dir, err := caller.UserSkillDir(rt); err == nil {
				targets = append(targets, skillTarget{runtime: rt, dir: dir})
			}
		}
	}
	printed, have := false, false
	for _, t := range targets {
		if !t.installed && !s.install {
			continue
		}
		if !printed {
			fmt.Fprintln(out, "\nThe agentiscale skill:")
			printed = true
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
