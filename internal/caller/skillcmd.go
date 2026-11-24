package caller

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/whiteredhouse/agentiscaled/internal/cli"
	"github.com/whiteredhouse/agentiscaled/internal/nodestate"
	"github.com/whiteredhouse/agentiscaled/skills"
)

const skillUsage = `usage: agentiscaled skill [print [FILE]]
       agentiscaled skill install [-dir DIR] [RUNTIME...]

The agentiscale skill teaches an agent when and how to find and call the other
agents of your network. print (the default) writes its SKILL.md to standard
output, or with FILE one of the references it links to, such as
references/cli.md. install writes all its files where each RUNTIME (claude,
codex; both when none is named) loads skills:

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
		if len(pos) > 1 || *dir != "" {
			fs.Usage()
			return errors.New("skill print takes at most one file")
		}
		if len(pos) == 0 {
			_, err := out.Write(skills.Agentiscale)
			return err
		}
		b, err := iofs.ReadFile(skills.AgentiscaleFiles, path.Clean(pos[0]))
		if err != nil {
			return fmt.Errorf("the agentiscale skill has no file %s", pos[0])
		}
		_, err = out.Write(b)
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
		done, err := installSkillFiles(dir)
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

// installSkillFiles writes every file of the skill into dir, each replaced in
// one step, and says what it did: installed when dir had no SKILL.md,
// unchanged when every file was already current, updated otherwise.
func installSkillFiles(dir string) (string, error) {
	switch _, err := os.Stat(filepath.Join(dir, "SKILL.md")); {
	case errors.Is(err, os.ErrNotExist):
		if err := writeSkillFiles(dir); err != nil {
			return "", err
		}
		return "installed", nil
	case err != nil:
		return "", err
	}
	changed := false
	err := iofs.WalkDir(skills.AgentiscaleFiles, ".", func(name string, d iofs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		want, err := iofs.ReadFile(skills.AgentiscaleFiles, name)
		if err != nil {
			return err
		}
		have, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		switch {
		case err == nil && bytes.Equal(have, want):
		case err == nil || errors.Is(err, os.ErrNotExist):
			changed = true
		default:
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !changed {
		return "unchanged", nil
	}
	if err := writeSkillFiles(dir); err != nil {
		return "", err
	}
	return "updated", nil
}

// writeSkillFiles writes every file of the skill under dir. SKILL.md goes
// last, so that a SKILL.md never links to a reference not yet written.
func writeSkillFiles(dir string) error {
	var names []string
	err := iofs.WalkDir(skills.AgentiscaleFiles, ".", func(name string, d iofs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && name != "SKILL.md" {
			names = append(names, name)
		}
		return err
	})
	if err != nil {
		return err
	}
	for _, name := range append(names, "SKILL.md") {
		b, err := iofs.ReadFile(skills.AgentiscaleFiles, name)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(filepath.Join(dir, filepath.FromSlash(name)), b); err != nil {
			return err
		}
	}
	return nil
}

// writeFileAtomic writes b to path through a temporary file in its
// directory, so that a reader sees the old file or the new one.
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
