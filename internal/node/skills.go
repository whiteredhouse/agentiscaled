package node

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

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

// skillVisibility is what the runtime's settings let the model see of a
// skill.
type skillVisibility int

const (
	skillShown skillVisibility = iota
	// skillNameOnly shows the model the skill's name, not its
	// description.
	skillNameOnly
	// skillHidden keeps the skill from the model: turned off, or only a
	// person may start it.
	skillHidden
)

// Bounds on a scan, so that a card read stays cheap whatever the directory
// holds.
const (
	// maxSkillDepth is how deep under a skill root a nested scan looks
	// for skill folders.
	maxSkillDepth = 4
	// maxSkillScanEntries bounds the directory entries one scan reads.
	maxSkillScanEntries = 4096
)

// skillScan finds the skills a runtime loads from an agent's directory.
type skillScan struct {
	// roots are the skill directories, nearest first. Each folder in one
	// that holds a SKILL.md is a skill.
	roots []string
	// scope is the directory every skill must really be in, symlinks
	// resolved: one linked from outside it, such as one of the owner's own
	// skills, is not the directory's and is not listed.
	scope  string
	naming skillNaming
	// nested also looks for skill folders in the subfolders of a root, as
	// Codex does.
	nested bool
	tag    string
	// visibility, if set, is what the runtime's settings show of the skill
	// of name at path (the SKILL.md as found).
	visibility func(name, path string) skillVisibility
}

// list lists the scan's skills for an agent card. Only a skill's name and
// description are read; a skill without a valid name or description, one
// only a person can invoke (disable-model-invocation), one the runtime's
// settings hide, and a second skill of a name already listed are left out.
// The list is sorted by name and holds at most maxCardSkills.
func (sc skillScan) list() []a2a.AgentSkill {
	scope, err := filepath.EvalSymlinks(sc.scope)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	visited := map[string]bool{}
	budget := maxSkillScanEntries
	var skills []a2a.AgentSkill
	var scan func(dir string, depth int)
	scan = func(dir string, depth int) {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || visited[real] || budget <= 0 {
			return
		}
		visited[real] = true
		entries := readDirUpTo(dir, budget)
		budget -= len(entries)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			folder := filepath.Join(dir, e.Name())
			if info, err := os.Stat(folder); err != nil || !info.IsDir() {
				continue
			}
			path := filepath.Join(folder, "SKILL.md")
			if _, err := os.Lstat(path); err == nil {
				sc.add(&skills, seen, scope, e.Name(), path)
			}
			// Codex also finds skills nested in a skill's own folder.
			if sc.nested && depth < maxSkillDepth {
				scan(folder, depth+1)
			}
		}
	}
	for _, root := range sc.roots {
		scan(root, 0)
	}
	slices.SortFunc(skills, func(a, b a2a.AgentSkill) int { return strings.Compare(a.ID, b.ID) })
	if len(skills) > maxCardSkills {
		skills = skills[:maxCardSkills]
	}
	return skills
}

// add adds the skill whose SKILL.md is at path, in the folder named
// folder, unless it is left out (list). seen holds the names listed.
func (sc skillScan) add(skills *[]a2a.AgentSkill, seen map[string]bool, scope, folder, path string) {
	file, err := filepath.EvalSymlinks(path)
	if err != nil || !within(file, scope) {
		return
	}
	name, description, ok := readSkill(file)
	if sc.naming == namedByFolder {
		name = folder
	}
	if !ok || len(name) > maxSkillNameLen || !skillName.MatchString(name) || seen[name] {
		return
	}
	v := skillShown
	if sc.visibility != nil {
		v = sc.visibility(name, path)
	}
	switch v {
	case skillHidden:
		// A farther skill of the name may still be on (Codex turns off
		// skill files, not names it has listed).
		return
	case skillNameOnly:
		description = ""
	}
	// The nearest skill of a name that is on is the one listed.
	seen[name] = true
	*skills = append(*skills, a2a.AgentSkill{ID: name, Name: name, Description: description, Tags: []string{sc.tag}})
}

// readDirUpTo reads at most n entries of dir, sorted by name. It stops
// reading at n, so a huge directory costs no more than that.
func readDirUpTo(dir string, n int) []os.DirEntry {
	if info, err := os.Stat(dir); n <= 0 || err != nil || !info.IsDir() {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer f.Close()
	entries, _ := f.ReadDir(n)
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries
}

// maxSettingsSize bounds a runtime settings file read for its skill
// settings.
const maxSettingsSize = 1 << 20

// readRegularFile reads up to limit bytes of the regular file at path, and
// says whether it could. Anything else is never opened in a way that waits:
// a named pipe would block the open.
func readRegularFile(path string, limit int64) ([]byte, bool) {
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	// It may have changed since it was checked.
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, limit))
	return b, err == nil
}

// readSkill reads the name and description in the front matter of the
// SKILL.md at path, which must be a regular file (readRegularFile).
func readSkill(path string) (name, description string, ok bool) {
	b, read := readRegularFile(path, maxSkillFrontMatter)
	if !read {
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
// only, or with aboveOutside each directory above it. Either way it stops
// short of the user's home directory and the filesystem root, whose skills
// are the user's or the machine's rather than the directory's, even when one
// of them is the repository's root.
func upToRepository(dir string, aboveOutside bool) []string {
	home, _ := os.UserHomeDir()
	if home != "" {
		home = filepath.Clean(home)
	}
	isRepo := func(d string) bool {
		_, err := os.Lstat(filepath.Join(d, ".git"))
		return err == nil
	}
	var dirs []string
	inRepo := false
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if len(dirs) > 0 && (d == home || filepath.Dir(d) == d) {
			inRepo = isRepo(d)
			break
		}
		dirs = append(dirs, d)
		if isRepo(d) {
			return dirs
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	if inRepo || aboveOutside {
		return dirs
	}
	return dirs[:1]
}
