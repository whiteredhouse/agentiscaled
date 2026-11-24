package node

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/whiteredhouse/agentiscaled/internal/caller"
	"github.com/whiteredhouse/agentiscaled/skills"
)

// TestSkillNamesRealTools keeps the skill in step with the tools and flags it
// tells agents to use.
func TestSkillNamesRealTools(t *testing.T) {
	t.Parallel()
	// The skill's text is every file of it, references included.
	var all strings.Builder
	err := fs.WalkDir(skills.AgentiscaleFiles, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(skills.AgentiscaleFiles, name)
		all.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	text := all.String()
	server := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	caller.AddTools(server, &caller.Client{})
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
			t.Errorf("the skill does not name %s", name)
		}
	}
	// The servers' own instructions name the discovery tools the skill
	// names.
	for _, name := range []string{"list_local_services", "discover_remote_services"} {
		if !strings.Contains(caller.Instructions, name) {
			t.Errorf("the agentiscale server no longer names %s: update SKILL.md", name)
		}
	}
	if !strings.Contains(networkInstructions, "list_agents") {
		t.Error("agentiscale-network no longer names list_agents: update SKILL.md")
	}

	// The flags of task send, as its -h prints them.
	runTaskSendFlags := map[string]bool{}
	var help bytes.Buffer
	_ = caller.RunTaskSend(context.Background(), []string{"-h"}, strings.NewReader(""), io.Discard, &help)
	for _, m := range regexp.MustCompile(`(?m)^  -([a-z-]+)`).FindAllStringSubmatch(help.String(), -1) {
		runTaskSendFlags[m[1]] = true
	}
	if len(runTaskSendFlags) == 0 {
		t.Fatalf("no flags in task send -h:\n%s", help.String())
	}
	for _, m := range regexp.MustCompile("`(?:agentiscaled task send )?-([a-z-]+)").FindAllStringSubmatch(text, -1) {
		if !runTaskSendFlags[m[1]] {
			t.Errorf("the skill names task send flag -%s, which does not exist", m[1])
		}
	}
}
