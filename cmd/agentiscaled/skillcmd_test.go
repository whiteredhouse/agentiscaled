package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/whiteredhouse/agentiscaled/skills"
)

func TestSkillPrint(t *testing.T) {
	for _, args := range [][]string{nil, {"print"}} {
		var out bytes.Buffer
		if err := runSkill(args, &out, io.Discard); err != nil {
			t.Fatalf("skill %v: %v", args, err)
		}
		if !bytes.Equal(out.Bytes(), skills.Agentiscale) {
			t.Errorf("skill %v printed something else than SKILL.md", args)
		}
	}
	if err := runSkill([]string{"print", "extra"}, io.Discard, io.Discard); err == nil {
		t.Error("skill print with an argument should fail")
	}
	if err := runSkill([]string{"remove"}, io.Discard, io.Discard); err == nil {
		t.Error("an unknown skill command should fail")
	}
}

func TestSkillFrontmatter(t *testing.T) {
	text := string(skills.Agentiscale)
	front, _, ok := strings.Cut(strings.TrimPrefix(text, "---\n"), "\n---\n")
	if !strings.HasPrefix(text, "---\n") || !ok {
		t.Fatal("SKILL.md has no frontmatter")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(front, "\n") {
		k, v, _ := strings.Cut(line, ": ")
		fields[k] = v
	}
	if fields["name"] != skills.AgentiscaleName {
		t.Errorf("name %q, want %q (the directory's name)", fields["name"], skills.AgentiscaleName)
	}
	if d := fields["description"]; d == "" || len(d) > 1024 || strings.ContainsAny(d, "<>") {
		t.Errorf("description must be 1 to 1024 characters without angle brackets, got %q", d)
	}
}

// TestSkillNamesRealTools keeps the skill in step with the tools and flags it
// tells agents to use.
func TestSkillNamesRealTools(t *testing.T) {
	text := string(skills.Agentiscale)
	server := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	addA2ATools(server, &a2aTasks{})
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "client"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"list_agents", networkServerName, "list_local_services", "discover_remote_services"}
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	for _, name := range names {
		if !strings.Contains(text, "`"+name+"`") {
			t.Errorf("SKILL.md does not name %s", name)
		}
	}
	for _, instructions := range []string{a2aInstructions, networkInstructions} {
		if !strings.Contains(instructions, "list_agents") {
			t.Error("MCP instructions must name the skill's agent discovery tool")
		}
	}

	// The flags of task send, as its -h prints them.
	runTaskSendFlags := map[string]bool{}
	var help bytes.Buffer
	_ = runTaskSend(context.Background(), []string{"-h"}, strings.NewReader(""), io.Discard, &help)
	for _, m := range regexp.MustCompile(`(?m)^  -([a-z-]+)`).FindAllStringSubmatch(help.String(), -1) {
		runTaskSendFlags[m[1]] = true
	}
	if len(runTaskSendFlags) == 0 {
		t.Fatalf("no flags in task send -h:\n%s", help.String())
	}
	for _, m := range regexp.MustCompile("`(?:agentiscaled task send )?-([a-z-]+)").FindAllStringSubmatch(text, -1) {
		if !runTaskSendFlags[m[1]] {
			t.Errorf("SKILL.md names task send flag -%s, which does not exist", m[1])
		}
	}
}

func TestSkillInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	claude := filepath.Join(home, ".claude", "skills", "agentiscale", "SKILL.md")
	codex := filepath.Join(home, ".agents", "skills", "agentiscale", "SKILL.md")

	install := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := runSkill(append([]string{"install"}, args...), &out, io.Discard); err != nil {
			t.Fatalf("skill install %v: %v", args, err)
		}
		return out.String()
	}
	check := func(path string) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(b, skills.Agentiscale) {
			t.Errorf("%s does not hold the skill (%v)", path, err)
		}
	}

	if got, want := install(), "installed ~/.claude/skills/agentiscale/SKILL.md\ninstalled ~/.agents/skills/agentiscale/SKILL.md\n"; got != want {
		t.Errorf("first install said\n%s\nwant\n%s", got, want)
	}
	check(claude)
	check(codex)
	if got := install("codex"); got != "unchanged ~/.agents/skills/agentiscale/SKILL.md\n" {
		t.Errorf("second install said %q", got)
	}
	if err := os.WriteFile(claude, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := install("claude"); got != "updated ~/.claude/skills/agentiscale/SKILL.md\n" {
		t.Errorf("install over an older copy said %q", got)
	}
	check(claude)
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(claude), ".SKILL.md.*")); len(leftovers) != 0 {
		t.Errorf("temporary files left: %v", leftovers)
	}

	// CLAUDE_CONFIG_DIR moves Claude Code's skills.
	config := filepath.Join(home, "claude config")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	install("claude")
	check(filepath.Join(config, "skills", "agentiscale", "SKILL.md"))

	// -dir installs into a project, before or after the runtimes.
	project := t.TempDir()
	install("-dir", project)
	check(filepath.Join(project, ".claude", "skills", "agentiscale", "SKILL.md"))
	check(filepath.Join(project, ".agents", "skills", "agentiscale", "SKILL.md"))
	other := t.TempDir()
	install("codex", "-dir", other)
	check(filepath.Join(other, ".agents", "skills", "agentiscale", "SKILL.md"))
	if _, err := os.Stat(filepath.Join(other, ".claude")); !os.IsNotExist(err) {
		t.Errorf("install codex wrote Claude Code's skill too (%v)", err)
	}

	for _, args := range [][]string{{"bogus"}, {"-dir", filepath.Join(home, "missing")}} {
		if err := runSkill(append([]string{"install"}, args...), io.Discard, io.Discard); err == nil {
			t.Errorf("skill install %v should fail", args)
		}
	}
}
