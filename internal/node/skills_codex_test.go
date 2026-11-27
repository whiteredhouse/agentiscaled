package node

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCodexCardMatchesCodex checks the card's skills against the skills a
// real Codex CLI gives the model in a task run, in the cases where the card
// reproduces Codex's own rules: skill roots up to the repository's root,
// nested skills, skills.config selectors and the last match winning, a
// trusted project's configuration, and a turned-off duplicate. Run with:
//
//	AGENTISCALE_TEST_CODEX=$(command -v codex) go test ./internal/node -run TestCodexCardMatchesCodex -v
func TestCodexCardMatchesCodex(t *testing.T) {
	command := os.Getenv("AGENTISCALE_TEST_CODEX")
	if command == "" {
		t.Skip("set AGENTISCALE_TEST_CODEX to a Codex CLI to run")
	}
	home := codexTempDir(t)
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_API_KEY", "test-key")
	repo := filepath.Join(home, "work", "repo")
	dir := filepath.Join(repo, "service")
	near, far := filepath.Join(dir, ".agents", "skills"), filepath.Join(repo, ".agents", "skills")

	// Each skill's description carries its marker, which the model sees in
	// Codex's list of skills when the skill is on.
	marker := func(id string) string { return "CARDCHECK[" + id + "]" }
	skill := func(root, folder, name, id, extra string) {
		write(t, filepath.Join(root, folder, "SKILL.md"), "---\nname: "+name+"\ndescription: "+marker(id)+" a skill to check the card with\n"+extra+"---\nBody.\n")
	}
	skill(near, "top", "top", "top", "")
	skill(far, "in-root", "in-root", "in-root", "")
	skill(filepath.Join(dir, ".codex", "skills"), "codex-root", "codex-root", "codex-root", "")
	skill(near, filepath.Join("group", "nested"), "nested", "nested", "")
	skill(near, filepath.Join("top", "inner"), "inner", "inner", "")
	skill(near, "off-by-path", "off-by-path", "off-by-path", "")
	skill(near, "off-by-name", "off-by-name", "off-by-name", "")
	skill(near, "folder-entry", "folder-entry", "folder-entry", "")
	skill(near, "project-off", "project-off", "project-off", "")
	skill(near, "back-on", "back-on", "back-on", "")
	skill(near, "manual", "manual", "manual", "disable-model-invocation: true\n")
	skill(near, "dup", "dup", "dup-near", "")
	skill(far, "dup", "dup", "dup-far", "")

	entry := func(key, value string, enabled bool) string {
		return fmt.Sprintf("[[skills.config]]\n%s = %q\nenabled = %t\n\n", key, value, enabled)
	}
	write(t, filepath.Join(codexHome, "config.toml"), "approval_policy = \"never\"\n\n"+
		entry("path", filepath.Join(near, "off-by-path", "SKILL.md"), false)+
		entry("name", "off-by-name", false)+
		entry("path", filepath.Join(near, "folder-entry"), false)+
		entry("path", filepath.Join(near, "back-on", "SKILL.md"), false)+
		entry("path", filepath.Join(near, "dup", "SKILL.md"), false)+
		fmt.Sprintf("[projects.%q]\ntrust_level = \"trusted\"\n", repo))
	for _, d := range []string{repo, dir} {
		write(t, filepath.Join(d, ".codex", "config.toml"),
			entry("path", filepath.Join(near, "project-off", "SKILL.md"), false)+
				entry("path", filepath.Join(near, "back-on", "SKILL.md"), true))
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	rt, err := newCodexRuntime(command, codexTempDir(t), agentPolicy{DirInstructions: true, DirSkills: true, OwnerConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	codex := rt.(*codexRuntime)
	model := &fakeModel{command: "true"}
	srv := httptest.NewServer(model)
	defer srv.Close()
	codex.extra = []string{
		"-c", `model_provider="agentiscale-test"`,
		"-c", fmt.Sprintf(`model_providers.agentiscale-test={name="agentiscale-test", base_url="%s/v1", wire_api="responses", requires_openai_auth=true}`, srv.URL),
	}
	if _, err := codex.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "Say hello"}); err != nil {
		t.Fatal(err)
	}
	requests, _, _ := model.seen()
	if !strings.Contains(requests, "Say hello") {
		t.Fatal("the request did not reach the model")
	}

	onCard := map[string]bool{}
	for _, s := range codex.Skills(dir) {
		id := s.ID
		if id == "dup" {
			id = strings.TrimSuffix(strings.TrimPrefix(strings.Fields(s.Description)[0], "CARDCHECK["), "]")
		}
		onCard[id] = true
	}
	ids := []string{"top", "in-root", "codex-root", "nested", "inner", "off-by-path", "off-by-name", "folder-entry", "project-off", "back-on", "manual", "dup-near", "dup-far"}
	var codexHas []string
	for _, id := range ids {
		inCodex := strings.Contains(requests, marker(id))
		if inCodex {
			codexHas = append(codexHas, id)
		}
		if inCodex != onCard[id] {
			t.Errorf("skill %s: Codex gives it to the model: %v, the card lists it: %v", id, inCodex, onCard[id])
		}
	}
	slices.Sort(codexHas)
	t.Logf("skills Codex gave the model: %v", codexHas)
}
