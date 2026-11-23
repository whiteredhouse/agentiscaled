package caller

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/whiteredhouse/agentiscaled/internal/cli"
	"github.com/whiteredhouse/agentiscaled/internal/nodestate"
	"github.com/whiteredhouse/agentiscaled/skills"
)

const skillUsage = `usage: agentiscaled skill [print]
       agentiscaled skill install [-dir DIR] [RUNTIME...]

The agentiscale skill teaches an agent when and how to find and call the other
agents of your network. print (the default) writes its SKILL.md to standard
output. install writes it where each RUNTIME (claude, codex; both when none
is named) loads skills:

  claude  $CLAUDE_CONFIG_DIR/skills/agentiscale, by default ~/.claude/skills/agentiscale
  codex   ~/.agents/skills/agentiscale

With -dir, it installs into that project instead: DIR/.claude/skills/agentiscale
and DIR/.agents/skills/agentiscale. Running install again updates the skill to
this agentiscaled's version.

Flags:
`

// RunSkill is `agentiscaled skill`: it prints the agentiscale skill, or
// installs it for agent runtimes.
func RunSkill(args []string, out, errOut io.Writer) error {
	sub := "print"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("skill", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprint(errOut, skillUsage)
		fs.PrintDefaults()
	}
	dir := fs.String("dir", "", "Install into this project directory instead of your user skills")
	pos, err := cli.ParseInterleaved(fs, args)
	if err != nil {
		return err
	}
	switch sub {
	case "print":
		if len(pos) != 0 || *dir != "" {
			fs.Usage()
			return errors.New("skill print takes no arguments")
		}
		_, err := out.Write(skills.Agentiscale)
		return err
	case "install":
		return installSkill(pos, *dir, out)
	default:
		fs.Usage()
		return fmt.Errorf("unknown skill command %q: use print or install", sub)
	}
}

// skillRuntimes are the runtimes whose skills directory skill install
// knows; an agent of the acp runtime keeps its skills its own way.
var skillRuntimes = []string{"claude", "codex"}

// installSkill writes the skill for each runtime named, all when none is.
func installSkill(names []string, project string, out io.Writer) error {
	if len(names) == 0 {
		names = skillRuntimes
	}
	var dirs []string
	for _, name := range names {
		if !slices.Contains(skillRuntimes, name) {
			return fmt.Errorf("unknown runtime %q: use %s", name, strings.Join(skillRuntimes, " or "))
		}
		dir, err := skillDir(name, project)
		if err != nil {
			return fmt.Errorf("find %s's skills directory: %w", name, err)
		}
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	for _, dir := range dirs {
		done, err := installSkillFile(dir)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %s\n", done, cli.HomeRelative(filepath.Join(dir, "SKILL.md")))
	}
	return nil
}

// skillDir is where runtime loads the agentiscale skill from: the owner's
// skills, or with project those of that directory.
func skillDir(runtime, project string) (string, error) {
	var root string
	switch {
	case project != "":
		abs, err := filepath.Abs(project)
		if err != nil {
			return "", err
		}
		if fi, err := os.Stat(abs); err != nil {
			return "", err
		} else if !fi.IsDir() {
			return "", fmt.Errorf("%s is not a directory", project)
		}
		root = filepath.Join(abs, map[string]string{"claude": ".claude", "codex": ".agents"}[runtime])
	case runtime == "claude":
		dir, err := nodestate.RuntimeConfigDir("CLAUDE_CONFIG_DIR", ".claude")
		if err != nil {
			return "", err
		}
		root = dir
	case runtime == "codex":
		// Codex loads a person's skills from ~/.agents/skills, whatever
		// CODEX_HOME is.
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".agents")
	default:
		return "", fmt.Errorf("no skills directory known for %s", runtime)
	}
	return filepath.Join(root, "skills", skills.AgentiscaleName), nil
}

// installSkillFile writes SKILL.md into dir, replacing it in one step, and says
// what it did.
func installSkillFile(dir string) (string, error) {
	path := filepath.Join(dir, "SKILL.md")
	old, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(old, skills.Agentiscale):
		return "unchanged", nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".SKILL.md.*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(skills.Agentiscale); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	if old != nil {
		return "updated", nil
	}
	return "installed", nil
}
