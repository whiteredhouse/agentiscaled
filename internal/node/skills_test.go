package node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func writeSkillFile(t *testing.T, root, dir, content string) {
	t.Helper()
	path := filepath.Join(root, dir, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skillIDs(skills []a2a.AgentSkill) []string {
	var ids []string
	for _, s := range skills {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestCodexCardListsTheDirectorysSkills(t *testing.T) {
	outside := t.TempDir()
	repo := filepath.Join(outside, "repo")
	dir := filepath.Join(repo, "service")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Above the repository: not the directory's, so not listed.
	writeSkillFile(t, filepath.Join(outside, ".agents", "skills"), "personal", "---\nname: personal\ndescription: Not the directory's.\n---\n")
	writeSkillFile(t, filepath.Join(repo, ".agents", "skills"), "release", "---\nname: release\ndescription: >-\n  Cuts a release\n  of the service.\n---\nSteps.\n")
	writeSkillFile(t, filepath.Join(dir, ".codex", "skills"), "deploy", "---\r\nname: deploy\r\ndescription: Deploys the service.\r\n---\r\n")
	writeSkillFile(t, filepath.Join(dir, ".agents", "skills"), "review", "---\nname: review\ndescription: Reviews a change.\n---\n")
	// The nearer copy of a name wins.
	writeSkillFile(t, filepath.Join(repo, ".codex", "skills"), "review-again", "---\nname: review\ndescription: Shadowed.\n---\n")

	skills := (&codexRuntime{policy: agentPolicy{DirSkills: true}}).Skills(dir)
	if got, want := skillIDs(skills), []string{"deploy", "release", "review"}; !slices.Equal(got, want) {
		t.Fatalf("skills = %v, want %v", got, want)
	}
	for _, s := range skills {
		if s.Name != s.ID || !slices.Equal(s.Tags, []string{"codex"}) {
			t.Errorf("skill %+v", s)
		}
		if s.ID == "release" && s.Description != "Cuts a release of the service." {
			t.Errorf("release description = %q", s.Description)
		}
		if s.ID == "review" && s.Description != "Reviews a change." {
			t.Errorf("review description = %q", s.Description)
		}
	}
	if skills := (&codexRuntime{policy: agentPolicy{DirSkills: false}}).Skills(dir); skills != nil {
		t.Errorf("-dir-skills=false lists %v", skillIDs(skills))
	}
}

func TestCodexOutsideARepositoryListsOnlyItsDirectory(t *testing.T) {
	// The home directory ends the search, whatever is above the test's
	// temporary directory.
	home := t.TempDir()
	t.Setenv("HOME", home)
	parent := filepath.Join(home, "work")
	dir := filepath.Join(parent, "agent")
	writeSkillFile(t, filepath.Join(parent, ".agents", "skills"), "above", "---\nname: above\ndescription: Above.\n---\n")
	writeSkillFile(t, filepath.Join(dir, ".agents", "skills"), "here", "---\nname: here\ndescription: Here.\n---\n")
	if got := skillIDs((&codexRuntime{policy: agentPolicy{DirSkills: true}}).Skills(dir)); !slices.Equal(got, []string{"here"}) {
		t.Fatalf("skills = %v", got)
	}
}

func TestSkillsTheCardLeavesOut(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, ".claude", "skills")
	writeSkillFile(t, root, "ok", "---\nname: ok\ndescription: Fine.\n---\n")
	for dir, content := range map[string]string{
		"no-front-matter":                      "name: no-front-matter\n",
		"unclosed":                             "---\nname: unclosed\ndescription: Never closed.\n",
		"Bad_Name":                             "---\nname: bad-name\ndescription: Its folder is not a skill name.\n---\n",
		strings.Repeat("a", maxSkillNameLen+1): "---\ndescription: Its folder name is too long.\n---\n",
		"no-description":                       "---\nname: no-description\n---\n",
		"long":                                 "---\nname: long\ndescription: " + strings.Repeat("x", maxSkillDescriptionLen+1) + "\n---\n",
		"manual":                               "---\nname: manual\ndescription: Only a person invokes it.\ndisable-model-invocation: true\n---\n",
		"bad-yaml":                             "---\nname: [\n---\n",
		".hidden":                              "---\nname: hidden\ndescription: Hidden folder.\n---\n",
	} {
		writeSkillFile(t, root, dir, content)
	}
	// A SKILL.md that is a directory is no skill.
	if err := os.MkdirAll(filepath.Join(root, "odd", "SKILL.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	skills := (&claudeRuntime{policy: agentPolicy{DirSkills: true, OwnerConfig: true}}).Skills(dir)
	if got := skillIDs(skills); !slices.Equal(got, []string{"ok"}) {
		t.Fatalf("skills = %v", got)
	}
	if !slices.Equal(skills[0].Tags, []string{"claude"}) {
		t.Errorf("tags = %v", skills[0].Tags)
	}
	if skills := (&claudeRuntime{policy: agentPolicy{DirSkills: false, OwnerConfig: true}}).Skills(dir); skills != nil {
		t.Errorf("-dir-skills=false lists %v", skillIDs(skills))
	}
}

func TestCardListsAtMostMaxCardSkills(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, ".claude", "skills")
	for i := range maxCardSkills + 5 {
		name := "skill-" + strings.Repeat("a", i/26) + string(rune('a'+i%26))
		writeSkillFile(t, root, name, "---\nname: "+name+"\ndescription: One.\n---\n")
	}
	if n := len((&claudeRuntime{policy: agentPolicy{DirSkills: true, OwnerConfig: true}}).Skills(dir)); n != maxCardSkills {
		t.Fatalf("listed %d skills, want %d", n, maxCardSkills)
	}
}

// The card a caller reads lists the directory's skills as they are when it
// reads it, and an empty list when the directory has none.
func TestAgentCardShowsTheCurrentSkills(t *testing.T) {
	dir := t.TempDir()
	svc, err := newAgentService(dir, &claudeRuntime{policy: agentPolicy{DirSkills: true, OwnerConfig: true}}, t.TempDir(), "helper")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(svc.handler("helper", "secret"))
	defer server.Close()
	read := func() *a2a.AgentCard {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/.well-known/agent-card.json", nil)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("card: %s", resp.Status)
		}
		card := new(a2a.AgentCard)
		if err := json.NewDecoder(resp.Body).Decode(card); err != nil {
			t.Fatal(err)
		}
		return card
	}
	if got := read().Skills; got == nil || len(got) != 0 {
		t.Fatalf("card skills without directory skills = %v, want an empty list", skillIDs(got))
	}
	writeSkillFile(t, filepath.Join(dir, ".claude", "skills"), "summarize", "---\nname: summarize\ndescription: Summarizes a document.\n---\n")
	card := read()
	if got := skillIDs(card.Skills); !slices.Equal(got, []string{"summarize"}) {
		t.Fatalf("card skills = %v, want the directory's", got)
	}
	if card.Skills[0].Description != "Summarizes a document." {
		t.Errorf("description = %q", card.Skills[0].Description)
	}
}

func TestCodexSkillNamesComeFromTheFile(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, ".agents", "skills")
	writeSkillFile(t, root, "folder", "---\nname: from-file\ndescription: Named in the file.\n---\n")
	writeSkillFile(t, root, "unnamed", "---\ndescription: No name.\n---\n")
	writeSkillFile(t, root, "bad", "---\nname: Bad_Name\ndescription: Not a skill name.\n---\n")
	writeSkillFile(t, root, "long", "---\nname: "+strings.Repeat("a", maxSkillNameLen+1)+"\ndescription: Too long.\n---\n")
	if got := skillIDs((&codexRuntime{policy: agentPolicy{DirSkills: true}}).Skills(dir)); !slices.Equal(got, []string{"from-file"}) {
		t.Fatalf("skills = %v", got)
	}
}

// Claude Code names a skill by its folder, and loads .claude/skills from the
// directories above the agent's up to its repository's root.
func TestClaudeCardListsSkillsUpToTheRepository(t *testing.T) {
	outside := t.TempDir()
	repo := filepath.Join(outside, "repo")
	dir := filepath.Join(repo, "service")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillFile(t, filepath.Join(outside, ".claude", "skills"), "above", "---\ndescription: Above the repository.\n---\n")
	writeSkillFile(t, filepath.Join(repo, ".claude", "skills"), "release", "---\ndescription: Cuts a release.\n---\n")
	writeSkillFile(t, filepath.Join(dir, ".claude", "skills"), "deploy", "---\nname: other-name\ndescription: Deploys.\n---\n")
	got := skillIDs((&claudeRuntime{policy: agentPolicy{DirSkills: true, OwnerConfig: true}}).Skills(dir))
	if want := []string{"deploy", "release"}; !slices.Equal(got, want) {
		t.Fatalf("skills = %v, want %v", got, want)
	}
}

// Outside a repository Claude Code loads .claude/skills from every directory
// above; the card stops short of the home directory, whose skills are the
// owner's own.
func TestClaudeOutsideARepositoryStopsShortOfHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "work", "agent")
	writeSkillFile(t, filepath.Join(home, ".claude", "skills"), "mine", "---\ndescription: The owner's own.\n---\n")
	writeSkillFile(t, filepath.Join(home, "work", ".claude", "skills"), "work", "---\ndescription: The work folder's.\n---\n")
	writeSkillFile(t, filepath.Join(dir, ".claude", "skills"), "here", "---\ndescription: The agent's.\n---\n")
	got := skillIDs((&claudeRuntime{policy: agentPolicy{DirSkills: true, OwnerConfig: true}}).Skills(dir))
	if want := []string{"here", "work"}; !slices.Equal(got, want) {
		t.Fatalf("skills = %v, want %v", got, want)
	}
}

// A Claude Code run that cannot use skills lists none: a restricted run
// (-owner-config=false) loads none, and a definition can leave out the Skill
// tool.
func TestClaudeCardListsNoSkillsItCannotUse(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, filepath.Join(dir, ".claude", "skills"), "deploy", "---\ndescription: Deploys.\n---\n")
	for name, r := range map[string]*claudeRuntime{
		"restricted":       {policy: agentPolicy{DirSkills: true}},
		"tools without it": {policy: agentPolicy{DirSkills: true, OwnerConfig: true}, def: &agentDefinition{WrittenFor: "claude", Tools: []string{"Read", "Grep"}}},
		"disallowed":       {policy: agentPolicy{DirSkills: true, OwnerConfig: true}, def: &agentDefinition{WrittenFor: "claude", DisallowedTools: []string{"Skill"}}},
	} {
		if skills := r.Skills(dir); skills != nil {
			t.Errorf("%s: lists %v", name, skillIDs(skills))
		}
	}
	for name, r := range map[string]*claudeRuntime{
		"no definition":     {policy: agentPolicy{DirSkills: true, OwnerConfig: true}},
		"tools with it":     {policy: agentPolicy{DirSkills: true, OwnerConfig: true}, def: &agentDefinition{WrittenFor: "claude", Tools: []string{"Read", "Skill"}}},
		"written for codex": {policy: agentPolicy{DirSkills: true, OwnerConfig: true}, def: &agentDefinition{WrittenFor: "codex"}},
	} {
		if got := skillIDs(r.Skills(dir)); !slices.Equal(got, []string{"deploy"}) {
			t.Errorf("%s: skills = %v", name, got)
		}
	}
}

// A home directory that is a Git repository (dotfiles) is still the owner's:
// its skills are not listed, but the directories below it are, as Codex
// loads them up to the repository's root.
func TestHomeRepositorySkillsAreNotListed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "work", "agent")
	writeSkillFile(t, filepath.Join(home, ".agents", "skills"), "mine", "---\nname: mine\ndescription: The owner's own.\n---\n")
	writeSkillFile(t, filepath.Join(home, "work", ".agents", "skills"), "work", "---\nname: work\ndescription: The work folder's.\n---\n")
	writeSkillFile(t, filepath.Join(dir, ".agents", "skills"), "here", "---\nname: here\ndescription: The agent's.\n---\n")
	got := skillIDs((&codexRuntime{policy: agentPolicy{DirSkills: true}}).Skills(dir))
	if want := []string{"here", "work"}; !slices.Equal(got, want) {
		t.Fatalf("skills = %v, want %v", got, want)
	}
}
