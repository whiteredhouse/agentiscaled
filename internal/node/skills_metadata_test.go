package node

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/whiteredhouse/agentiscaled/internal/caller"
)

// The skills share the agent's metadata with its runtime and drain: with
// the largest drain and the most and longest names a card can list, the
// metadata still fits, and the skills are all counted.
func TestServiceMetadataFitsTheSkills(t *testing.T) {
	var names []string
	for i := range maxCardSkills {
		names = append(names, fmt.Sprintf("%02d-%s", i, strings.Repeat("s", maxSkillNameLen-3)))
	}
	for _, drain := range []*drainMark{nil, {Reason: strings.Repeat("\"", caller.MaxDrainReasonBytes), Since: time.Now(), By: caller.DrainSetOnNode, Drained: true}} {
		md := agentActivity{Runtime: strings.Repeat("r", 32), Drain: drain, Skills: names}.serviceMetadata()
		if err := api.ValidateServiceMetadata(md); err != nil {
			t.Fatalf("drain %v: %v", drain != nil, err)
		}
		s := caller.SkillsFromMetadata(md)
		if s == nil || s.Total != maxCardSkills || len(s.Names) == 0 || !slices.Equal(s.Names, names[:len(s.Names)]) {
			t.Fatalf("drain %v: skills %+v", drain != nil, s)
		}
		if drain != nil && caller.DrainFromMetadata(md) == nil {
			t.Fatal("the skills pushed out the drain")
		}
	}
	if md := (agentActivity{Runtime: "codex"}).serviceMetadata(); md[caller.SkillsMetadataKey] != "" {
		t.Fatalf("no skills: %v", md)
	}
}

// The node looks at its agents' skills again while it runs, and publishes
// them when they change.
func TestReportActivityPublishesTheSkills(t *testing.T) {
	defer func(a, s time.Duration) { activityEvery, skillsEvery = a, s }(activityEvery, skillsEvery)
	activityEvery, skillsEvery = 5*time.Millisecond, 20*time.Millisecond
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "agent")
	writeSkillFile(t, filepath.Join(dir, ".agents", "skills"), "review", "---\nname: review\ndescription: Reviews a change.\n---\n")
	svc, err := newAgentService(dir, &codexRuntime{policy: agentPolicy{DirSkills: true}}, t.TempDir(), "helper")
	if err != nil {
		t.Fatal(err)
	}
	published := make(chan map[string]string, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reportActivity(ctx, []*agentService{svc}, func(string, *api.ServiceStatus) {}, func(_ string, md map[string]string) { published <- md })
	await := func(want ...string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case md := <-published:
				if s := caller.SkillsFromMetadata(md); s != nil && slices.Equal(s.Names, want) && s.Total == len(want) {
					return
				}
			case <-deadline:
				t.Fatalf("skills %v never published", want)
			}
		}
	}
	await("review")
	writeSkillFile(t, filepath.Join(dir, ".agents", "skills"), "deploy", "---\nname: deploy\ndescription: Deploys it.\n---\n")
	await("deploy", "review")
}
