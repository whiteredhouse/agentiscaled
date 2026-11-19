package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
)

// The agent is busy while a turn runs, and a task whose agent asked a
// question counts as waiting until the answer comes, also while another
// task runs.
func TestActivityFollowsTheTasks(t *testing.T) {
	h := newAgentHarness(t, testSender)
	if a := h.svc.activity(); a.Name != "helper" || a.State != activityIdle || a.Since != nil || a.Waiting != 0 {
		t.Fatalf("before any task: %+v", a)
	}
	asking := h.send(t, "", "double a number", nil)
	<-h.fix.prompts
	if a := h.svc.activity(); a.State != activityBusy || a.Since == nil || time.Since(*a.Since) > time.Minute {
		t.Fatalf("while a task runs: %+v", a)
	}
	h.fix.ask <- "Which number?"
	h.await(t, asking.ID, a2a.TaskStateInputRequired)
	waitIdle(t, h.svc)
	if a := h.svc.activity(); a.State != activityIdle || a.Since != nil || a.Waiting != 1 || a.WaitingSince == nil {
		t.Fatalf("while the question waits: %+v", a)
	}

	other := h.send(t, "", "review", nil)
	<-h.fix.prompts
	if a := h.svc.activity(); a.State != activityBusy || a.Waiting != 1 {
		t.Fatalf("busy with a question waiting: %+v", a)
	}
	h.fix.release <- "No findings"
	h.await(t, other.ID, a2a.TaskStateCompleted)
	waitIdle(t, h.svc)

	h.answer(t, asking.ID, "", "21")
	<-h.fix.prompts
	if a := h.svc.activity(); a.State != activityBusy || a.Waiting != 0 {
		t.Fatalf("once answered: %+v", a)
	}
	h.fix.release <- "42"
	h.await(t, asking.ID, a2a.TaskStateCompleted)
	waitIdle(t, h.svc)
	if a := h.svc.activity(); a.State != activityIdle || a.Waiting != 0 {
		t.Fatalf("after the tasks: %+v", a)
	}
}

// A question nobody answered in time no longer counts as waiting.
func TestActivityLeavesOutLapsedQuestions(t *testing.T) {
	defer func(d time.Duration) { answerWait = d }(answerWait)
	answerWait = 50 * time.Millisecond
	h := newAgentHarness(t, testSender)
	task := h.send(t, "", "double a number", nil)
	<-h.fix.prompts
	h.fix.ask <- "Which number?"
	h.await(t, task.ID, a2a.TaskStateInputRequired)
	waitIdle(t, h.svc)
	time.Sleep(100 * time.Millisecond)
	if a := h.svc.activity(); a.Waiting != 0 || a.WaitingSince != nil {
		t.Fatalf("a lapsed question still waits: %+v", a)
	}
}

// The owner's local agent API says what the node's agents do, and only
// on its Unix socket.
func TestLocalAgentAPIServesActivity(t *testing.T) {
	_, _, tasks, dir := localAgentsFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{}, "helper", "second")
	resp, err := tasks.local.Get("http://agentiscaled/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got localAgentsActivity
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /agents: %s %v", resp.Status, err)
	}
	if len(got.Agents) != 2 || got.Agents[0].Name != "helper" || got.Agents[1].Name != "second" || got.Agents[0].State != activityIdle {
		t.Fatalf("agents: %+v", got.Agents)
	}
	activity, running := nodeActivity(dir)
	if !running || activity["second"].State != activityIdle {
		t.Fatalf("nodeActivity: %v %+v", running, activity)
	}
}

// The node's catalog reports carry each agent's activity as a coarse
// status, looked at again while the node runs.
func TestReportActivity(t *testing.T) {
	defer func(d time.Duration) { activityEvery = d }(activityEvery)
	activityEvery = 10 * time.Millisecond
	h := newAgentHarness(t, testSender)
	statuses := make(chan *api.ServiceStatus, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reportActivity(ctx, []*agentService{h.svc}, func(name string, st *api.ServiceStatus) {
		if name != "helper" {
			t.Errorf("status for %q", name)
		}
		select {
		case statuses <- st:
		default:
		}
	})
	await := func(want api.ServiceState, waiting uint32) *api.ServiceStatus {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case st := <-statuses:
				if st.GetState() == want && st.GetInputRequired() == waiting {
					return st
				}
			case <-deadline:
				t.Fatalf("no status %s with %d waiting", want, waiting)
			}
		}
	}
	if st := await(api.ServiceState_SERVICE_STATE_IDLE, 0); st.Since != nil || st.InputRequiredSince != nil {
		t.Fatalf("idle: %v", st)
	}
	task := h.send(t, "", "double a number", nil)
	<-h.fix.prompts
	if st := await(api.ServiceState_SERVICE_STATE_BUSY, 0); st.Since == nil {
		t.Fatalf("busy: %v", st)
	}
	h.fix.ask <- "Which number?"
	h.await(t, task.ID, a2a.TaskStateInputRequired)
	if st := await(api.ServiceState_SERVICE_STATE_IDLE, 1); st.InputRequiredSince == nil {
		t.Fatalf("waiting: %v", st)
	}
}

func TestActivityText(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	activity := map[string]agentActivity{
		"idle":    {Name: "idle", State: activityIdle},
		"busy":    {Name: "busy", State: activityBusy, Since: ago(3*time.Minute + 20*time.Second)},
		"asked":   {Name: "asked", State: activityIdle, Waiting: 1, WaitingSince: ago(42 * time.Second)},
		"both":    {Name: "both", State: activityBusy, Since: ago(5 * time.Second), Waiting: 2, WaitingSince: ago(2*time.Hour + 5*time.Minute)},
		"unknown": {Name: "unknown", State: "something new"},
	}
	for name, want := range map[string]string{
		"idle":    "idle",
		"busy":    "busy (3m)",
		"asked":   "waiting for an answer (42s)",
		"both":    "busy (5s), waiting for 2 answers (2h5m)",
		"unknown": "idle",
		"added":   "not served yet",
	} {
		if got := activityText(activity, true, name, now); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if got := activityText(nil, false, "idle", now); got != "node not running" {
		t.Errorf("no node: %q", got)
	}
	if got := activityText(nil, true, "idle", now); got != "-" {
		t.Errorf("node without the activity API: %q", got)
	}
}

// agent list asks the running node, says so when none runs, and shows
// nothing it cannot know when the node is too old to say.
func TestAgentListShowsActivity(t *testing.T) {
	dir := shortTempDir(t)
	writeSettings(t, dir, settings{Control: "https://control.test", Agents: []agentConfig{
		{Runtime: "codex", Name: "helper", Dir: t.TempDir()},
		{Runtime: "claude", Name: "reviewer", Dir: t.TempDir()},
	}})
	list := func() string {
		t.Helper()
		var out bytes.Buffer
		if err := runAgent(context.Background(), []string{"list", "-data-dir", dir}, &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if out := list(); !strings.HasPrefix(out, "NAME      STATE ") || strings.Count(out, "node not running") != 2 {
		t.Fatalf("no node running:\n%s", out)
	}

	listener, err := listenAgentSocket(filepath.Join(dir, localAgentSocketName))
	if err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-2 * time.Minute)
	agents := []agentActivity{{Name: "helper", State: activityBusy, Since: &since, Waiting: 1, WaitingSince: &since}}
	var old atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		if old.Load() {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(localAgentsActivity{Agents: agents})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })
	out := list()
	if !strings.Contains(out, "busy (2m), waiting for an answer (2m)") || !strings.Contains(out, "not served yet") {
		t.Fatalf("running node:\n%s", out)
	}

	old.Store(true)
	for _, line := range strings.Split(strings.TrimSpace(list()), "\n")[1:] {
		if fields := strings.Fields(line); fields[1] != "-" {
			t.Fatalf("node without the activity API: %q", line)
		}
	}
}
