package node

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
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
	svc.refreshSkills()
	published := make(chan map[string]string, 64)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := runReporters(ctx, []*agentService{svc}, func(string, *api.ServiceStatus) {}, func(_ string, md map[string]string) {
		select {
		case published <- md:
		default:
		}
	})
	// The reporters stop before the intervals are put back.
	defer func() { cancel(); stopped() }()
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

// blockingSkills is a runtime whose skill scan waits until it is let go, as
// one on a slow file system would.
type blockingSkills struct {
	*fixture
	scanning chan struct{}
	release  chan struct{}
}

func (b blockingSkills) Skills(string) []a2a.AgentSkill {
	select {
	case b.scanning <- struct{}{}:
	default:
	}
	<-b.release
	return nil
}

// A skill scan that does not return holds up no agent's activity: the
// reports go on for every agent of the node, the slow one too.
func TestSlowSkillScanDoesNotHoldUpActivity(t *testing.T) {
	defer func(a, s time.Duration) { activityEvery, skillsEvery = a, s }(activityEvery, skillsEvery)
	activityEvery, skillsEvery = 5*time.Millisecond, 5*time.Millisecond
	slow := blockingSkills{fixture: newFixture(), scanning: make(chan struct{}, 1), release: make(chan struct{})}
	stuck, err := newAgentService(t.TempDir(), slow, t.TempDir(), "stuck")
	if err != nil {
		t.Fatal(err)
	}
	h := newAgentHarness(t, testSender)
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	busy := map[string]bool{}
	stopped := runReporters(ctx, []*agentService{stuck, h.svc}, func(name string, st *api.ServiceStatus) {
		mu.Lock()
		busy[name] = st.GetState() == api.ServiceState_SERVICE_STATE_BUSY
		mu.Unlock()
	}, func(string, map[string]string) {})
	defer func() {
		cancel()
		close(slow.release)
		stopped()
	}()
	select {
	case <-slow.scanning:
	case <-time.After(5 * time.Second):
		t.Fatal("no skill scan")
	}
	// While the scan waits, the other agent takes a task and its status
	// says so.
	running := h.send(t, "", "review", nil)
	<-h.fix.prompts
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		reported := busy["helper"]
		mu.Unlock()
		if reported {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the other agent's busy status was held up by the scan")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.fix.release <- "done"
	h.await(t, running.ID, a2a.TaskStateCompleted)
}

// runReporters runs the skill refresh and the activity reports as the node
// does, until ctx ends; the function it returns waits for both to stop.
func runReporters(ctx context.Context, agents []*agentService, setStatus func(string, *api.ServiceStatus), setMetadata func(string, map[string]string)) func() {
	var wg sync.WaitGroup
	wg.Go(func() { refreshSkillsEvery(ctx, agents) })
	wg.Go(func() { reportActivity(ctx, agents, setStatus, setMetadata) })
	return wg.Wait
}
