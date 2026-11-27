package node

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/agentmesh/api"
	"github.com/whiteredhouse/agentiscaled/internal/caller"
)

// A drained agent finishes the task it runs and the one that waits for an
// answer, but refuses a new task with its owner's reason, until undrained.
func TestDrainedAgentFinishesItsTasksAndRefusesNewOnes(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t, testSender)
	asked := h.send(t, "", "double a number", nil)
	<-h.fix.prompts
	h.fix.ask <- "Which number?"
	h.await(t, asked.ID, a2a.TaskStateInputRequired)
	waitIdle(t, h.svc)
	running := h.send(t, "", "review", nil)
	<-h.fix.prompts

	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := saveDrain(h.dir, "helper", drainMark{Reason: "reboot at noon", Since: since, By: caller.DrainSetOnNode}); err != nil {
		t.Fatal(err)
	}
	refused := h.send(t, "", "another", nil)
	got := h.await(t, refused.ID, a2a.TaskStateRejected)
	if text := statusText(got); text != "refused: helper is draining: reboot at noon; it takes no new tasks" || terminal(got) != "rejected" {
		t.Fatalf("refusal: %q (terminal %q)", text, terminal(got))
	}

	h.fix.release <- "No findings"
	h.await(t, running.ID, a2a.TaskStateCompleted)
	h.answer(t, asked.ID, "", "21")
	if p := <-h.fix.prompts; p.Text != "21" {
		t.Fatalf("answer reached the runtime as %+v", p)
	}
	h.fix.release <- "42"
	h.await(t, asked.ID, a2a.TaskStateCompleted)

	a := h.svc.activity()
	if a.Drain == nil || a.Drain.Reason != "reboot at noon" || !a.Drain.Since.Equal(since) {
		t.Fatalf("activity drain: %+v", a.Drain)
	}
	md := a.serviceMetadata()
	if err := api.ValidateServiceMetadata(md); err != nil {
		t.Fatalf("metadata %v: %v", md, err)
	}
	if d := caller.DrainFromMetadata(md); d == nil || *d != *a.Drain {
		t.Fatalf("metadata drain: %+v, want %+v", d, a.Drain)
	}

	if lifted, err := removeDrain(h.dir, "helper"); err != nil || !lifted {
		t.Fatalf("undrain: %v %v", lifted, err)
	}
	again := h.send(t, "", "after", nil)
	<-h.fix.prompts
	h.fix.release <- "done"
	h.await(t, again.ID, a2a.TaskStateCompleted)
	if a := h.svc.activity(); a.Drain != nil || a.serviceMetadata()[caller.DrainMetadataKey] != "" {
		t.Fatalf("undrained activity: %+v", a)
	}
}

// A drain file the node cannot read still drains the agent: the owner
// meant it to take no new tasks.
func TestUnreadableDrainStillDrains(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t, testSender)
	if err := os.WriteFile(drainPath(h.dir, "helper"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := h.await(t, h.send(t, "", "task", nil).ID, a2a.TaskStateRejected)
	if text := statusText(got); text != "refused: helper is draining; it takes no new tasks" {
		t.Fatalf("refusal: %q", text)
	}
	if md := h.svc.activity().serviceMetadata(); caller.DrainFromMetadata(md) == nil || api.ValidateServiceMetadata(md) != nil {
		t.Fatalf("metadata: %v", md)
	}
}

// A hand-edited drain file whose fields do not fit still drains the agent,
// and its mark still fits the service metadata, so that the node
// registers.
func TestHandEditedDrainFileStillFits(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t, testSender)
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name, file, reason string
		since              bool
	}{
		{"long by", `{"reason":"reboot","since":"2026-01-02T03:04:05Z","by":"` + strings.Repeat("x", 600) + `"}`, "reboot", true},
		{"by not a word", `{"reason":"reboot","by":"no de"}`, "reboot", false},
		{"long reason", `{"reason":"` + strings.Repeat("x", 600) + `","since":"2026-01-02T03:04:05Z","by":"node"}`, "", true},
		{"control in by", `{"by":"a\u0007b"}`, "", false},
		// Go reads this time and cannot write it.
		{"offset past a day", `{"reason":"reboot","since":"2026-01-02T03:04:05+24:00"}`, "reboot", false},
		{"offset", `{"reason":"reboot","since":"2026-01-02T04:04:05+01:00"}`, "reboot", true},
	} {
		if err := os.WriteFile(drainPath(h.dir, "helper"), []byte(tc.file), 0o600); err != nil {
			t.Fatal(err)
		}
		activity := h.svc.activity()
		md := activity.serviceMetadata()
		if err := api.ValidateServiceMetadata(md); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		d := caller.DrainFromMetadata(md)
		if d == nil || d.Reason != tc.reason || d.Since.Equal(since) != tc.since || (d.By != "" && d.By != caller.DrainSetOnNode) {
			t.Fatalf("%s: %q read back as %+v", tc.name, md[caller.DrainMetadataKey], d)
		}
		// The local agent API writes every agent's drain in one answer.
		if _, err := json.Marshal(caller.LocalInfo{Draining: map[caller.AgentName]caller.Drain{"helper": *activity.Drain}}); err != nil {
			t.Fatalf("%s: /info: %v", tc.name, err)
		}
	}
	got := h.await(t, h.send(t, "", "task", nil).ID, a2a.TaskStateRejected)
	if text := statusText(got); !strings.HasPrefix(text, "refused: helper is draining") {
		t.Fatalf("refusal: %q", text)
	}
}

// The catalog's metadata changes only when the drain does, so a drain
// reaches callers and the console at once, without a report a second.
func TestReportActivityPublishesTheDrain(t *testing.T) {
	defer func(d time.Duration) { activityEvery = d }(activityEvery)
	activityEvery = 5 * time.Millisecond
	h := newAgentHarness(t, testSender)
	h.svc.runtimeName = "codex"
	// Every report says what runs the agent, drained or not.
	facts := map[string]string{caller.RuntimeMetadataKey: "codex"}
	published := make(chan map[string]string, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reportActivity(ctx, []*agentService{h.svc}, func(string, *api.ServiceStatus) {}, func(name string, md map[string]string) {
		if name != "helper" {
			t.Errorf("metadata for %q", name)
		}
		published <- md
	})
	next := func() map[string]string {
		t.Helper()
		select {
		case md := <-published:
			return md
		case <-time.After(5 * time.Second):
			t.Fatal("no metadata")
		}
		return nil
	}
	if md := next(); !maps.Equal(md, facts) {
		t.Fatalf("first: %v", md)
	}
	if err := saveDrain(h.dir, "helper", drainMark{Reason: "swap definition", Since: time.Now().UTC(), By: caller.DrainSetOnNode}); err != nil {
		t.Fatal(err)
	}
	drained := next()
	if d := caller.DrainFromMetadata(drained); d == nil || d.Reason != "swap definition" || d.By != "node" {
		t.Fatalf("drain: %+v", d)
	}
	if caller.RuntimeFromMetadata(drained) != "codex" {
		t.Fatalf("drained metadata lost the runtime: %v", drained)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case md := <-published:
		t.Fatalf("published again without a change: %v", md)
	default:
	}
	if _, err := removeDrain(h.dir, "helper"); err != nil {
		t.Fatal(err)
	}
	if md := next(); !maps.Equal(md, facts) {
		t.Fatalf("undrained: %v", md)
	}
}

// A runtime that is not a valid one is left out rather than published, and
// the largest metadata still fits.
func TestServiceMetadataLeavesOutAnInvalidRuntime(t *testing.T) {
	if md := (agentActivity{Runtime: "Codex CLI"}).serviceMetadata(); len(md) != 0 {
		t.Fatalf("metadata = %v, want none", md)
	}
	if err := api.ValidateServiceMetadata(agentActivity{Runtime: "acp", Drain: &drainMark{Reason: strings.Repeat("\"", caller.MaxDrainReasonBytes)}}.serviceMetadata()); err != nil {
		t.Fatalf("largest metadata does not fit: %v", err)
	}
}

// The longest reason the command takes, made of characters JSON escapes,
// still fits in the service metadata.
func TestDrainMetadataFits(t *testing.T) {
	t.Parallel()
	for _, r := range []string{"\"", "\\", " ", "<", "é", "日"} {
		reason := strings.Repeat(r, caller.MaxDrainReasonBytes/len(r))
		if why := caller.DrainReasonError(reason); why != "" && r != " " {
			t.Fatalf("%q: %s", r, why)
		}
		d := drainMark{Reason: reason, Since: time.Now(), By: caller.DrainSetOnNode}
		md := map[string]string{caller.DrainMetadataKey: d.Metadata()}
		if err := api.ValidateServiceMetadata(md); err != nil {
			t.Fatalf("%q: %v", r, err)
		}
		if got := caller.DrainFromMetadata(md); got == nil || got.Reason != reason {
			t.Fatalf("%q: read back %+v", r, got)
		}
	}
	long := drainMark{Reason: strings.Repeat("x", caller.MaxDrainReasonBytes+1)}
	if got := caller.DrainFromMetadata(map[string]string{caller.DrainMetadataKey: long.Metadata()}); got == nil || got.Reason != "" {
		t.Fatalf("too long a reason: %+v", got)
	}
	for _, bad := range []string{"two\nlines", "tab\there", "bell\a", string([]byte{0xff})} {
		if caller.DrainReasonError(bad) == "" {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestAgentDrainCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := saveSettings(dir, settings{PeerID: "peer", Agents: []agentConfig{{Name: "codex", Runtime: "codex", Dir: dir}, {Name: "claude", Runtime: "claude", Dir: dir}}}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := RunAgent(context.Background(), append(args, "-data-dir", dir), &out)
		return out.String(), err
	}
	if _, err := run("drain", "nobody"); err == nil || !strings.Contains(err.Error(), `no agent named "nobody"`) {
		t.Fatalf("unknown agent: %v", err)
	}
	if _, err := run("drain", "codex", "-reason", "two\nlines"); err == nil || !strings.Contains(err.Error(), "-reason has a line break") {
		t.Fatalf("bad reason: %v", err)
	}
	out, err := run("drain", "codex", "-reason", " re-login ")
	if err != nil || !strings.Contains(out, "codex is drained: no node answers on this machine now") {
		t.Fatalf("drain: %q %v", out, err)
	}
	first := loadDrain(drainPath(dir, "codex"))
	if first == nil || first.Reason != "re-login" || first.By != "node" || first.Since.IsZero() {
		t.Fatalf("saved: %+v", first)
	}
	if loadDrain(drainPath(dir, "claude")) != nil {
		t.Fatal("draining one agent drained another")
	}
	// Draining again changes the reason but keeps when it began.
	if _, err := run("drain", "codex", "-reason", "reboot"); err != nil {
		t.Fatal(err)
	}
	if again := loadDrain(drainPath(dir, "codex")); again.Reason != "reboot" || !again.Since.Equal(first.Since) {
		t.Fatalf("drained again: %+v, first %+v", again, first)
	}
	// Without a name, every agent drains.
	if _, err := run("drain"); err != nil {
		t.Fatal(err)
	}
	if d := loadDrain(drainPath(dir, "claude")); d == nil || d.Reason != "" {
		t.Fatalf("claude: %+v", d)
	}
	out, err = run("undrain", "codex")
	if err != nil || out != "codex takes new tasks again.\n" || loadDrain(drainPath(dir, "codex")) != nil {
		t.Fatalf("undrain: %q %v", out, err)
	}
	out, err = run("undrain")
	if err != nil || out != "codex was not drained; it takes new tasks.\nclaude takes new tasks again.\n" {
		t.Fatalf("undrain all: %q %v", out, err)
	}
	b, _ := json.Marshal(drainMark{})
	if string(b) != "{}" {
		t.Fatalf("empty drain: %s", b)
	}
}

// agent drain -wait asks the node, which judges the drain and the tasks
// itself: the command succeeds only on the node's answer that the drain it
// saved holds and no task is left. The stand-in node here serves the real
// handlers over one real agent service whose running slot the test sets.
func TestAgentDrainWait(t *testing.T) {
	dir := shortTempDir(t)
	writeSettings(t, dir, settings{Control: "https://control.test", Agents: []agentConfig{
		{Runtime: "codex", Name: "codex", Dir: t.TempDir()},
		{Runtime: "claude", Name: "claude", Dir: t.TempDir()},
	}})
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := RunAgent(context.Background(), append(args, "-data-dir", dir), &out)
		return out.String(), err
	}
	if _, err := run("drain", "codex", "-timeout", "1s"); err == nil || !strings.Contains(err.Error(), "-timeout needs -wait") {
		t.Fatalf("timeout without wait: %v", err)
	}
	// No node answers: silence is not proof that no task runs, since a
	// starting node serves its agents before it opens the socket.
	if out, err := run("drain", "codex", "-wait"); err == nil || !strings.Contains(err.Error(), "no node answers; the drain is saved") || !strings.Contains(out, "codex is drained") {
		t.Fatalf("no node: %q %v", out, err)
	}
	if loadDrain(drainPath(dir, "codex")) == nil {
		t.Fatal("drain not saved")
	}
	if _, err := removeDrain(dir, "codex"); err != nil {
		t.Fatal(err)
	}

	// Only this command reads these; the stand-in node below holds its
	// requests for its own times, so no handler outlives the test reading them.
	for v, short := range map[*time.Duration]time.Duration{&drainWaitEvery: 10 * time.Millisecond, &drainWaitLongest: 80 * time.Millisecond} {
		prev := *v
		*v = short
		t.Cleanup(func() { *v = prev })
	}
	svc, err := newAgentService(t.TempDir(), nil, dir, "codex")
	if err != nil {
		t.Fatal(err)
	}
	setBusy := func(busy bool) {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		svc.current = nil
		if busy {
			svc.current = &agentRun{taskID: "t1", started: time.Now()}
		}
	}
	listener, err := listenAgentSocket(filepath.Join(dir, localAgentSocketName))
	if err != nil {
		t.Fatal(err)
	}
	var old, older atomic.Bool // a node without the wait question; one without drains
	var failing atomic.Int32   // the next wait questions to answer 503
	var slow atomic.Int64      // how long each wait answer takes, in nanoseconds
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		as := agentsActivity([]*agentService{svc})
		if older.Load() {
			as[0].Drain = nil
		}
		_ = json.NewEncoder(w).Encode(localAgentsActivity{Agents: as})
	})
	mux.HandleFunc("POST /agents/drain/wait", func(w http.ResponseWriter, r *http.Request) {
		if old.Load() {
			http.NotFound(w, r)
			return
		}
		time.Sleep(time.Duration(slow.Load()))
		if failing.Add(-1) >= 0 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		serveDrainWaitEvery(w, r, []*agentService{svc}, dir, 80*time.Millisecond, 5*time.Millisecond)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })

	// A busy agent: -timeout gives up, the drain kept.
	setBusy(true)
	out, err := run("drain", "codex", "-wait", "-timeout", "200ms")
	if err == nil || !strings.Contains(err.Error(), "codex still has work after 200ms; it stays drained") || !strings.Contains(out, "codex is draining") {
		t.Fatalf("timeout: %q %v", out, err)
	}
	if loadDrain(drainPath(dir, "codex")) == nil {
		t.Fatal("a timed-out wait lifted the drain")
	}
	// The wait ends once the node says the task is done.
	go func() {
		time.Sleep(150 * time.Millisecond)
		setBusy(false)
	}()
	out, err = run("drain", "codex", "-wait")
	if err != nil || !strings.Contains(out, "codex: no task left.") {
		t.Fatalf("wait: %q %v", out, err)
	}

	// Answers the node cannot give are asked again: the wait goes on until
	// the node says the agent is idle.
	setBusy(true)
	failing.Store(3)
	go func() {
		for failing.Load() > 0 {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		setBusy(false)
	}()
	out, err = run("drain", "codex", "-wait")
	if err != nil || !strings.Contains(out, "codex: no task left.") {
		t.Fatalf("wait through failed answers: %q %v", out, err)
	}
	failing.Store(0)

	// An answer that comes in after -timeout does not count as success.
	slow.Store(int64(time.Second))
	start := time.Now()
	out, err = run("drain", "codex", "-wait", "-timeout", "50ms")
	if err == nil || !strings.Contains(err.Error(), "the node did not say within 50ms") {
		t.Fatalf("late answer: %q %v", out, err)
	}
	if took := time.Since(start); took > 150*time.Millisecond {
		t.Fatalf("-timeout 50ms took %s: the question outlived it", took)
	}
	slow.Store(0)

	// A drain lifted while the command waits: the agent may take new tasks.
	setBusy(true)
	go func() {
		time.Sleep(50 * time.Millisecond)
		removeDrain(dir, "codex")
	}()
	if out, err := run("drain", "codex", "-wait", "-timeout", "5s"); err == nil || !strings.Contains(err.Error(), "codex was undrained while this command waited") {
		t.Fatalf("lifted drain: %q %v", out, err)
	}
	// And one lifted and set again between two checks: not this drain.
	go func() {
		time.Sleep(50 * time.Millisecond)
		saveDrain(dir, "codex", drainMark{Since: time.Now().UTC().Add(time.Second), By: caller.DrainSetOnNode})
	}()
	if out, err := run("drain", "codex", "-wait", "-timeout", "5s"); err == nil || !strings.Contains(err.Error(), "codex was undrained while this command waited") {
		t.Fatalf("drained again: %q %v", out, err)
	}

	// A node that holds drains but cannot be asked to wait on them.
	old.Store(true)
	if _, err := run("drain", "codex", "-wait", "-timeout", "5s"); err == nil || !strings.Contains(err.Error(), "the drain holds, but the running node is older than this command and cannot say when its tasks are done") {
		t.Fatalf("node without the wait question: %v", err)
	}
	old.Store(false)

	// A node from before drains reports no drain for an agent it serves.
	older.Store(true)
	if _, err := run("drain", "codex"); err == nil || !strings.Contains(err.Error(), "the running node is older than this command") {
		t.Fatalf("old node: %v", err)
	}
	older.Store(false)

	// A node that fails to answer at all ends the wait after
	// drainWaitSilence, also without -timeout.
	prevSilence := drainWaitSilence
	drainWaitSilence = 150 * time.Millisecond
	t.Cleanup(func() { drainWaitSilence = prevSilence })
	failing.Store(1 << 20)
	if out, err := run("drain", "codex", "-wait"); err == nil || !strings.Contains(err.Error(), "the node has not answered for 150ms; the drain is saved") {
		t.Fatalf("silent node: %q %v", out, err)
	}
	failing.Store(0)
	// Nor does an answer that arrives after drainWaitSilence count: each
	// question gets only the silence left.
	setBusy(false)
	slow.Store(int64(170 * time.Millisecond))
	if out, err := run("drain", "codex", "-wait"); err == nil || !strings.Contains(err.Error(), "the node has not answered for 150ms; the drain is saved") || strings.Contains(out, "no task left.") {
		t.Fatalf("answer after the silence: %q %v", out, err)
	}
	slow.Store(0)

	// A node that goes away while the agent works is no proof that its
	// work is done: the wait fails.
	setBusy(true)
	go func() {
		time.Sleep(50 * time.Millisecond)
		srv.Close()
	}()
	out, err = run("drain", "codex", "-wait", "-timeout", "5s")
	if err == nil || !strings.Contains(err.Error(), "the node stopped answering before it said the agent had no task left") || strings.Contains(out, "no task left.") {
		t.Fatalf("node went away: %q %v", out, err)
	}
}

// The node answers waits on every name it registers, DNS names too, and
// answers within the time the command says it can wait.
func TestDrainWaitEndpoint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	svc, err := newAgentService(t.TempDir(), nil, dir, "release.bot")
	if err != nil {
		t.Fatal(err)
	}
	since := time.Now().UTC().Truncate(time.Second)
	if err := saveDrain(dir, "release.bot", drainMark{Since: since, By: caller.DrainSetOnNode}); err != nil {
		t.Fatal(err)
	}
	ask := func(req any) (int, drainWaitAnswer, time.Duration) {
		body, _ := json.Marshal(req)
		rec := httptest.NewRecorder()
		start := time.Now()
		serveDrainWaitEvery(rec, httptest.NewRequest(http.MethodPost, "/agents/drain/wait", bytes.NewReader(body)), []*agentService{svc}, dir, 5*time.Second, 5*time.Millisecond)
		took := time.Since(start)
		var answer drainWaitAnswer
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return rec.Code, answer, took
	}
	if code, answer, _ := ask(drainWaitRequest{Agents: map[caller.AgentName]time.Time{"release.bot": since}}); code != http.StatusOK || answer.State != drainHeld {
		t.Fatalf("DNS name: %d %+v", code, answer)
	}
	if code, _, _ := ask(map[string]any{"agents": map[string]time.Time{"../x": since}}); code != http.StatusBadRequest {
		t.Fatalf("bad name: %d", code)
	}
	svc.mu.Lock()
	svc.current = &agentRun{taskID: "t1", started: time.Now()}
	svc.mu.Unlock()
	code, answer, took := ask(drainWaitRequest{Agents: map[caller.AgentName]time.Time{"release.bot": since}, WithinMS: 50})
	if code != http.StatusOK || answer.State != drainWorking || took > time.Second {
		t.Fatalf("within 50ms: %d %+v after %s", code, answer, took)
	}
}

// The restart advice for a node that ignores drains fits how it runs.
func TestDrainRestartAdvice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	foreground := func(got string) bool {
		return strings.Contains(got, "stop the 'agentiscaled run -data-dir") && !strings.Contains(got, "background service")
	}
	running := &adviceService{installedNow: true, runningNow: true}
	if got := restartAdvice(context.Background(), dir, settings{}, running); got != "restart its background service (test service)" {
		t.Fatalf("running service: %q", got)
	}
	for name, c := range map[string]struct {
		st  settings
		svc service
	}{
		"foreground setting":    {settings{Foreground: true}, running},
		"no service":            {settings{}, nil},
		"service not installed": {settings{}, &adviceService{}},
		// Stopped, while the node runs in the foreground instead.
		"service stopped": {settings{}, &adviceService{installedNow: true}},
	} {
		if got := restartAdvice(context.Background(), dir, c.st, c.svc); !foreground(got) {
			t.Errorf("%s: %q", name, got)
		}
	}
	// A service manager that is slow to answer does not outlast the
	// caller's deadline; the advice then names both ways.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	slow := &adviceService{installedNow: true, runningNow: true, slow: time.Second}
	got := restartAdvice(ctx, dir, settings{}, slow)
	if took := time.Since(start); took > 300*time.Millisecond || !strings.Contains(got, "test service) if that runs it, else stop the 'agentiscaled run") {
		t.Fatalf("slow service manager: %q after %s", got, took)
	}
	if !slow.cancelled.Load() {
		t.Fatal("the service manager's question was not cancelled with the deadline")
	}
}

// The node answers for all its agents at one moment: an agent read while
// another's lock is still taken is read then, not before.
func TestAgentsActivityIsOneMoment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := newAgentService(t.TempDir(), nil, dir, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newAgentService(t.TempDir(), nil, dir, "b")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveDrain(dir, "a", drainMark{Since: time.Now().UTC(), By: caller.DrainSetOnNode}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	got := make(chan []agentActivity, 1)
	go func() { got <- agentsActivity([]*agentService{a, b}) }()
	time.Sleep(100 * time.Millisecond)
	if _, err := removeDrain(dir, "a"); err != nil {
		b.mu.Unlock()
		t.Fatal(err)
	}
	b.mu.Unlock()
	if as := <-got; as[0].Drain != nil {
		t.Fatalf("a was read before b's moment: %+v", as[0].Drain)
	}
}

// adviceService is a background service for restartAdvice.
type adviceService struct {
	service
	installedNow, runningNow bool
	slow                     time.Duration
	cancelled                atomic.Bool
}

func (s *adviceService) name() string    { return "test service" }
func (s *adviceService) installed() bool { return s.installedNow }
func (s *adviceService) state(ctx context.Context) (string, bool) {
	select {
	case <-time.After(s.slow):
	case <-ctx.Done():
		// The service manager's process is cancelled with ctx.
		s.cancelled.Store(true)
		return "", false
	}
	return "", s.runningNow
}

// A user who may not use the node's socket is told so at once, with the
// drain saved, rather than left waiting.
func TestDrainSocketDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may use any socket")
	}
	dir := shortTempDir(t)
	writeSettings(t, dir, settings{Control: "https://control.test", Agents: []agentConfig{{Runtime: "codex", Name: "codex", Dir: t.TempDir()}}})
	socket := filepath.Join(dir, localAgentSocketName)
	listener, err := listenAgentSocket(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	if err := os.Chmod(socket, 0); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = RunAgent(context.Background(), []string{"drain", "codex", "-wait", "-data-dir", dir}, &out)
	if err == nil || !strings.Contains(err.Error(), "the drain is saved, but this user may not use the node's socket") {
		t.Fatalf("denied: %q %v", out.String(), err)
	}
	if loadDrain(drainPath(dir, "codex")) == nil {
		t.Fatal("drain not saved")
	}
}

// A task whose admission waits for the agent's lock while the owner drains
// it is refused: admission reads the drain in the critical section that
// takes the running slot, so agent drain -wait's idle answer holds.
func TestDrainReadWhereTheSlotIsTaken(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t, testSender)
	h.svc.mu.Lock()
	sent := make(chan *a2a.Task, 1)
	go func() { sent <- h.send(t, "", "slips in", nil) }()
	// Let the request reach admission and wait there for the lock.
	time.Sleep(100 * time.Millisecond)
	if err := saveDrain(h.dir, "helper", drainMark{Reason: "reboot", Since: time.Now().UTC(), By: caller.DrainSetOnNode}); err != nil {
		h.svc.mu.Unlock()
		t.Fatal(err)
	}
	h.svc.mu.Unlock()
	task := <-sent
	got := h.await(t, task.ID, a2a.TaskStateRejected)
	if text := statusText(got); text != "refused: helper is draining: reboot; it takes no new tasks" {
		t.Fatalf("refusal: %q", text)
	}
}

// An activity answer is one moment: the drain is read under the lock that
// guards the task state, so a drain lifted while the answer waits for that
// lock is not paired with the state after it.
func TestActivityReadsDrainWithTaskState(t *testing.T) {
	t.Parallel()
	h := newAgentHarness(t, testSender)
	if err := saveDrain(h.dir, "helper", drainMark{Reason: "reboot", Since: time.Now().UTC(), By: caller.DrainSetOnNode}); err != nil {
		t.Fatal(err)
	}
	h.svc.mu.Lock()
	got := make(chan agentActivity, 1)
	go func() { got <- h.svc.activity() }()
	// Let the answer start and wait for the lock.
	time.Sleep(100 * time.Millisecond)
	if _, err := removeDrain(h.dir, "helper"); err != nil {
		h.svc.mu.Unlock()
		t.Fatal(err)
	}
	h.svc.mu.Unlock()
	if a := <-got; a.Drain != nil {
		t.Fatalf("an undrained agent's answer carries the lifted drain: %+v", a.Drain)
	}
}

// A waiting question is judged at the snapshot's moment, not when its
// agent happens to be read: agentsActivity reads every agent as of one
// clock reading.
func TestActivityJudgesExpiryAtTheSnapshot(t *testing.T) {
	t.Parallel()
	s, err := newAgentService(t.TempDir(), nil, t.TempDir(), "b")
	if err != nil {
		t.Fatal(err)
	}
	s.answerWait = time.Hour
	asked := time.Now().Add(-2 * time.Hour) // expired by the wall clock
	s.waiting[a2a.TaskID("t1")] = &waitingTask{since: asked}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.activityLocked(asked.Add(59 * time.Minute)); a.Waiting != 1 {
		t.Fatalf("at the snapshot the question still waits: %+v", a)
	}
	if a := s.activityLocked(asked.Add(time.Hour)); a.Waiting != 0 {
		t.Fatalf("at its expiry the question no longer waits: %+v", a)
	}
}
