package node

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
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
	if a := h.svc.activity(); a.Drain != nil || a.serviceMetadata() != nil {
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
		if _, err := json.Marshal(caller.LocalInfo{Draining: map[string]caller.Drain{"helper": *activity.Drain}}); err != nil {
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
	if md := next(); md != nil {
		t.Fatalf("first: %v", md)
	}
	if err := saveDrain(h.dir, "helper", drainMark{Reason: "swap definition", Since: time.Now().UTC(), By: caller.DrainSetOnNode}); err != nil {
		t.Fatal(err)
	}
	if d := caller.DrainFromMetadata(next()); d == nil || d.Reason != "swap definition" || d.By != "node" {
		t.Fatalf("drain: %+v", d)
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
	if md := next(); md != nil {
		t.Fatalf("undrained: %v", md)
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
	if err != nil || !strings.Contains(out, "codex is drained: its node is not running") {
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

// agent drain -wait returns once the agent has no task left, gives up
// after -timeout with the agent still drained, and a node that does not
// read drains is reported, not trusted.
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
	// No node runs: nothing to wait for.
	if out, err := run("drain", "codex", "-wait"); err != nil || !strings.Contains(out, "its node is not running") {
		t.Fatalf("no node: %q %v", out, err)
	}

	prev := drainWaitEvery
	drainWaitEvery = 10 * time.Millisecond
	t.Cleanup(func() { drainWaitEvery = prev })
	listener, err := listenAgentSocket(filepath.Join(dir, localAgentSocketName))
	if err != nil {
		t.Fatal(err)
	}
	var busy, reads atomic.Bool
	reads.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		a := agentActivity{Name: "codex", State: activityIdle}
		if busy.Load() {
			a.State = activityBusy
		}
		if reads.Load() {
			a.Drain = loadDrain(drainPath(dir, "codex"))
		}
		_ = json.NewEncoder(w).Encode(localAgentsActivity{Agents: []agentActivity{a}})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })

	busy.Store(true)
	out, err := run("drain", "codex", "-wait", "-timeout", "100ms")
	if err == nil || !strings.Contains(err.Error(), "codex still has work after 100ms; it stays drained") || !strings.Contains(out, "codex is draining") {
		t.Fatalf("timeout: %q %v", out, err)
	}
	if loadDrain(drainPath(dir, "codex")) == nil {
		t.Fatal("a timed-out wait lifted the drain")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		busy.Store(false)
	}()
	out, err = run("drain", "codex", "-wait")
	if err != nil || !strings.Contains(out, "codex: no task left.") {
		t.Fatalf("wait: %q %v", out, err)
	}

	// A node from before drains reports no drain for an agent it serves.
	reads.Store(false)
	busy.Store(false)
	if _, err := run("drain", "codex"); err == nil || !strings.Contains(err.Error(), "the running node is older than this command") {
		t.Fatalf("old node: %v", err)
	}
}
