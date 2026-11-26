package node

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
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
	t.Parallel()
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
	t.Parallel()
	h := newAgentHarness(t, testSender)
	h.svc.answerWait = 50 * time.Millisecond
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
	t.Parallel()
	_, _, tasks, dir := localAgentsFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{}, "helper", "second")
	resp, err := tasks.Local.Get("http://agentiscaled/agents")
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
	}, func(string, map[string]string) {})
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
	t.Parallel()
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
		if got := activityText(activity, true, name, now, nil); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	drain := &drainMark{Reason: "reboot", Since: now.Add(-3 * time.Minute)}
	for name, want := range map[string]string{
		"idle": "drained (3m): reboot",
		"both": "busy (5s), waiting for 2 answers (2h5m), draining (3m): reboot",
	} {
		if got := activityText(activity, true, name, now, drain); got != want {
			t.Errorf("drained %s: %q, want %q", name, got, want)
		}
	}
	// The running node's drain wins over the file's.
	served := map[string]agentActivity{"idle": {Name: "idle", State: activityIdle, Drain: &drainMark{}}}
	if got := activityText(served, true, "idle", now, nil); got != "drained" {
		t.Errorf("drained by the node: %q", got)
	}
	if got := activityText(nil, false, "idle", now, drain); got != "node not running, drained (3m): reboot" {
		t.Errorf("no node, drained: %q", got)
	}
	if got := activityText(nil, false, "idle", now, nil); got != "node not running" {
		t.Errorf("no node: %q", got)
	}
	if got := activityText(nil, true, "idle", now, nil); got != "-" {
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
		if err := RunAgent(context.Background(), []string{"list", "-data-dir", dir}, &out); err != nil {
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

// A saved name the node refuses is shown percent-encoded and labelled
// wherever saved names are printed, so that it cannot be copied into a
// command as it is; an agent saved without a name has the one the node
// gives it.
func TestSavedAgentNamesShownSafely(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	bad := "bot/$(id)\n'x'"
	encoded := "bot%2F%24%28id%29%0A%27x%27"
	writeSettings(t, dir, settings{Control: "https://control.test", Agents: []agentConfig{
		{Runtime: "codex", Dir: t.TempDir()},
		{Runtime: "acp", Command: "/opt/My Apps/my-agent", Dir: t.TempDir()},
		{Runtime: "claude", Name: "release.bot.", Dir: t.TempDir()},
		{Runtime: "claude", Name: bad, Dir: t.TempDir()},
		// A taken name, and a refused name that is the encoded form itself.
		{Runtime: "claude", Name: "botx2fx24x28idx29x0ax27xx27", Dir: t.TempDir()},
		{Runtime: "claude", Name: encoded, Dir: t.TempDir()},
	}})
	got, err := runAgentOut(dir, "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	want := []struct{ prefix, state string }{
		{"codex ", ""}, {"my-agent ", ""}, {"release.bot. ", ""}, {encoded + " ", "invalid"},
		{"botx2fx24x28idx29x0ax27xx27 ", ""}, {"bot%252F%2524%2528id%2529%250A%2527x%2527 ", "invalid"},
	}
	if len(lines) != len(want)+1 || strings.Contains(got, "$(id)") {
		t.Fatalf("agent list:\n%s", got)
	}
	for i, w := range want {
		if line := lines[i+1]; !strings.HasPrefix(line, w.prefix) || strings.Contains(line, "invalid name; the node does not start: fix or delete it in "+filepath.Join(dir, settingsFile)) != (w.state != "") {
			t.Fatalf("agent list line %d %q, want %q:\n%s", i+1, line, w.prefix, got)
		}
	}
	var status bytes.Buffer
	if err := RunStatus(context.Background(), []string{"-data-dir", dir}, &status); err != nil {
		t.Fatal(err)
	}
	if st := status.String(); !strings.Contains(st, "Agent:    codex (codex)") || !strings.Contains(st, "Agent:    my-agent (acp)") ||
		!strings.Contains(st, "Agent:    "+encoded+" (claude)") || !strings.Contains(st, "invalid name; the node does not start") || strings.Contains(st, "$(id)") {
		t.Fatalf("status:\n%s", st)
	}
	// Draining every agent leaves out the refused ones, which serve nothing.
	if out, _ := runAgentOut(dir, "drain"); strings.Contains(out, "$(id)") || strings.Contains(out, "%2F") {
		t.Fatalf("drain all: %q", out)
	}
	// Only a name the node takes names an agent to remove: a refused one is
	// fixed in the settings file, and its encoded form names nothing.
	if _, err := runAgentOut(dir, "remove", encoded); err == nil {
		t.Fatal("remove took the encoded form of a refused name")
	}
	if _, err := runAgentOut(dir, "remove", bad); err == nil {
		t.Fatal("remove took a refused name")
	}
	if out, err := runAgentOut(dir, "remove", "botx2fx24x28idx29x0ax27xx27"); err != nil || !strings.Contains(out, "no longer offers botx2fx24x28idx29x0ax27xx27;") {
		t.Fatalf("remove: %q %v", out, err)
	}
	s, err := loadSettings(dir)
	if err != nil || len(s.Agents) != 5 || !slices.ContainsFunc(s.Agents, func(a agentConfig) bool { return a.Name == bad }) {
		t.Fatalf("after remove: %+v %v", s.Agents, err)
	}
}

// runAgentOut runs an agent command on dir and gives its output.
func runAgentOut(dir string, args ...string) (string, error) {
	var out bytes.Buffer
	err := RunAgent(context.Background(), append(args, "-data-dir", dir), &out)
	return out.String(), err
}
