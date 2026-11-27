package node

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.yaml.in/yaml/v3"
)

// skillLister is a runtime that can list the skills an agent in dir loads
// from that directory, for its card.
type skillLister interface {
	// Skills lists the skills of dir that the runtime loads, under its
	// policy. Nil means none, or that the policy turns them off.
	Skills(dir string) []a2a.AgentSkill
}

// Bounds on what the card lists, from the Agent Skills format: a name of up
// to 64 characters of a-z, 0-9 and -, and a description of up to 1024
// characters.
const (
	maxCardSkills          = 64
	maxSkillNameLen        = 64
	maxSkillDescriptionLen = 1024
	// maxSkillFrontMatter is how much of a SKILL.md is read for its front
	// matter.
	maxSkillFrontMatter = 16 << 10
)

var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// skillNaming is where a runtime takes a skill's name from.
type skillNaming int

const (
	// namedInFile takes it from the name in SKILL.md's front matter, as
	// Codex does.
	namedInFile skillNaming = iota
	// namedByFolder takes it from the skill's folder, as Claude Code does.
	namedByFolder
)

// directorySkills lists the skills in roots, which are skill directories
// (each subdirectory holding a SKILL.md is a skill), for an agent card
// tagged tag. Only a skill's name and description are read; a skill without
// a valid name or description, or one only a person can invoke
// (disable-model-invocation), is left out, as is a second skill of a name
// already listed. The list is sorted by name and holds at most
// maxCardSkills.
func directorySkills(roots []string, naming skillNaming, tag string) []a2a.AgentSkill {
	seen := map[string]bool{}
	var skills []a2a.AgentSkill
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			name, description, ok := readSkill(filepath.Join(root, e.Name(), "SKILL.md"))
			if naming == namedByFolder {
				name = e.Name()
			}
			if !ok || len(name) > maxSkillNameLen || !skillName.MatchString(name) || seen[name] {
				continue
			}
			seen[name] = true
			skills = append(skills, a2a.AgentSkill{ID: name, Name: name, Description: description, Tags: []string{tag}})
		}
	}
	slices.SortFunc(skills, func(a, b a2a.AgentSkill) int { return strings.Compare(a.ID, b.ID) })
	if len(skills) > maxCardSkills {
		skills = skills[:maxCardSkills]
	}
	return skills
}

// readSkill reads the name and description in the front matter of the
// SKILL.md at path.
func readSkill(path string) (name, description string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", "", false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSkillFrontMatter))
	if err != nil {
		return "", "", false
	}
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	b = bytes.TrimPrefix(b, []byte("\ufeff"))
	rest, found := bytes.CutPrefix(b, []byte("---\n"))
	if !found {
		return "", "", false
	}
	front, _, found := bytes.Cut(rest, []byte("\n---"))
	if !found {
		return "", "", false
	}
	var m struct {
		Name                   string `yaml:"name"`
		Description            string `yaml:"description"`
		DisableModelInvocation bool   `yaml:"disable-model-invocation"`
	}
	if yaml.Unmarshal(front, &m) != nil || m.DisableModelInvocation {
		return "", "", false
	}
	name = strings.TrimSpace(m.Name)
	description = strings.Join(strings.Fields(m.Description), " ")
	if description == "" || len([]rune(description)) > maxSkillDescriptionLen {
		return "", "", false
	}
	return name, description, true
}

// upToRepository is dir and each directory above it up to the root of the
// Git repository dir is in, nearest first. Outside a repository it is dir
// only, or with aboveOutside each directory above it short of the user's
// home directory and the root, whose skills are the user's or the machine's
// rather than the directory's.
func upToRepository(dir string, aboveOutside bool) []string {
	dir = filepath.Clean(dir)
	dirs := []string{dir}
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return dirs
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
		dirs = append(dirs, d)
	}
	if !aboveOutside {
		return dirs[:1]
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		home = filepath.Clean(home)
	}
	// dirs ends with the root; each directory from the home directory up
	// is left out, and dir itself is always kept.
	for i, d := range dirs {
		if i > 0 && (d == home || filepath.Dir(d) == d) {
			return dirs[:i]
		}
	}
	return dirs
}
