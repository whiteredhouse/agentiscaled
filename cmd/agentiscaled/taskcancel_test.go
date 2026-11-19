package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	meshnode "github.com/google/agentmesh/export/node"
)

// startTaskCancel runs `agentiscaled task cancel args...` in the background.
func startTaskCancel(args ...string) *sendRun {
	r := &sendRun{out: &syncBuffer{}, errOut: &syncBuffer{}, done: make(chan error, 1)}
	go func() {
		r.done <- runTasks(context.Background(), append([]string{"cancel"}, args...), strings.NewReader(""), r.out, r.errOut)
	}()
	return r
}

// task cancel stops a running task, leaves an ended one as it is, and
// skips an empty ID, so that a cleanup step can pass every ID it may have.
func TestTaskCancelStopsRunningTasks(t *testing.T) {
	_, fix, tasks, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	done := sendNoWait(t, "-data-dir", dir, "helper", "quick")
	<-fix.prompts
	fix.release <- "ok"
	if err := startTaskWait("-data-dir", dir, "helper", done).wait(t); err != nil {
		t.Fatal(err)
	}
	running := sendNoWait(t, "-data-dir", dir, "helper", "take forever")
	<-fix.prompts

	run := startTaskCancel("-data-dir", dir, "helper", running, "", done)
	if err := run.wait(t); err != nil {
		t.Fatalf("task cancel: %v\n%s", err, run.errOut)
	}
	want := "task " + running + " cancelled\ntask " + done + " had already ended: completed\n"
	if got := run.out.String(); got != want {
		t.Fatalf("stdout %q, want %q", got, want)
	}
	_, got, err := tasks.get(context.Background(), nil, getTaskIn{taskIn: taskIn{taskTarget: taskTarget{Service: "helper"}, TaskID: running}})
	if err != nil || got.Terminal != "cancelled" {
		t.Fatalf("task after cancel: %+v %v", got, err)
	}

	// Waiting for it now reports the cancellation.
	if err := startTaskWait("-data-dir", dir, "helper", running).wait(t); exitCode(err) != exitNotCompleted {
		t.Fatalf("wait after cancel: %v (exit %d)", err, exitCode(err))
	}

	run = startTaskCancel("-data-dir", dir, "-json", "helper", running)
	if err := run.wait(t); err != nil {
		t.Fatal(err)
	}
	var shown []taskOut
	if err := json.Unmarshal([]byte(run.out.String()), &shown); err != nil || len(shown) != 1 || shown[0].TaskID != running || shown[0].Terminal != "cancelled" {
		t.Fatalf("json %q: %+v %v", run.out.String(), shown, err)
	}
}

// A task the agent does not know fails the command, after the others are
// cancelled.
func TestTaskCancelReportsAnUnknownTask(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	running := sendNoWait(t, "-data-dir", dir, "helper", "take forever")
	<-fix.prompts
	unknown := "0f9a7c52-0000-4000-8000-000000000000"
	run := startTaskCancel("-data-dir", dir, "helper", unknown, running)
	err := run.wait(t)
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "1 of 2 tasks not cancelled") {
		t.Fatalf("got %v (exit %d)", err, exitCode(err))
	}
	if out := run.out.String(); !strings.Contains(out, "task "+unknown+": task "+unknown+" not found at helper") || !strings.Contains(out, "task "+running+" cancelled\n") {
		t.Fatalf("stdout: %s", out)
	}
}

func TestTaskCancelUsage(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"-data-dir", dir},
		{"-data-dir", dir, "helper"},
		{"-data-dir", dir, "bad name", "abc"},
		{"-data-dir", dir, "-timeout", "0s", "helper", "abc"},
	} {
		if err := startTaskCancel(args...).wait(t); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
	// Only empty IDs: nothing to do, and no node needed.
	run := startTaskCancel("-data-dir", dir, "helper", "", "")
	if err := run.wait(t); err != nil || run.out.String() != "" {
		t.Fatalf("empty IDs: %v %q", err, run.out.String())
	}
}

// A malformed ID, an unknown task and an agent that does not answer about
// one task fail only those: the other tasks are still cancelled, and -json
// says why each failed.
func TestTaskCancelGoesOnPastBadTasks(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	running := sendNoWait(t, "-data-dir", dir, "helper", "take forever")
	<-fix.prompts
	unknown := "0f9a7c52-0000-4000-8000-000000000000"
	run := startTaskCancel("-data-dir", dir, "-json", "helper", "not an id", unknown, running)
	err := run.wait(t)
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "2 of 3 tasks not cancelled") {
		t.Fatalf("got %v (exit %d)", err, exitCode(err))
	}
	var shown []cancelled
	if err := json.Unmarshal([]byte(run.out.String()), &shown); err != nil || len(shown) != 3 {
		t.Fatalf("json %q: %v", run.out.String(), err)
	}
	if shown[0].TaskID != "not an id" || shown[0].Error != "not a task ID" ||
		shown[1].TaskID != unknown || !strings.Contains(shown[1].Error, "not found at helper") ||
		shown[2].TaskID != running || shown[2].Terminal != "cancelled" || shown[2].Error != "" {
		t.Fatalf("json: %+v", shown)
	}
}

func TestTaskCancelIsNotHeldUpByASlowAnswer(t *testing.T) {
	net := newFakeNetwork(t)
	h := newAgentHarness(t, testSender)
	remote := randomPeer(t)
	net.offer(remote)
	net.route(remote, h.url)
	running := sendNoWait(t, "-data-dir", net.dir, "helper", "take forever")
	<-h.fix.prompts

	// The agent's node holds any call about the slow task.
	slow := "0f9a7c52-0000-4000-8000-000000000000"
	u, _ := url.Parse(h.url)
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.Transport = meshHeaders{"secret", testSender}
	proxy.FlushInterval = -1
	net.mu.Lock()
	net.routes[remote] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(slow)) {
			<-r.Context().Done()
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		proxy.ServeHTTP(w, r)
	})
	net.mu.Unlock()

	run := startTaskCancel("-data-dir", net.dir, "-timeout", "2s", "helper", slow, running)
	err := run.wait(t)
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "1 of 2 tasks not cancelled") {
		t.Fatalf("got %v (exit %d)\n%s", err, exitCode(err), run.out)
	}
	if out := run.out.String(); !strings.HasPrefix(out, "task "+slow+": ") || !strings.HasSuffix(out, "task "+running+" cancelled\n") {
		t.Fatalf("stdout: %s", out)
	}
}

// A task waiting for an answer is cancelled too, freeing its agent.
func TestTaskCancelStopsATaskThatAsks(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	id := sendNoWait(t, "-data-dir", dir, "helper", "double a number")
	<-fix.prompts
	fix.ask <- "Which number?"
	if err := startTaskWait("-data-dir", dir, "helper", id).wait(t); exitCode(err) != exitInputRequired {
		t.Fatalf("wait: %v (exit %d)", err, exitCode(err))
	}
	run := startTaskCancel("-data-dir", dir, "helper", id)
	if err := run.wait(t); err != nil || run.out.String() != "task "+id+" cancelled\n" {
		t.Fatalf("cancel: %v %q", err, run.out.String())
	}
	// The agent takes a new task.
	run = startTaskSend("", "-data-dir", dir, "helper", "next")
	if p := <-fix.prompts; p.Text != "next" {
		t.Fatalf("the agent got %q", p.Text)
	}
	fix.release <- "ok"
	if err := run.wait(t); err != nil {
		t.Fatalf("next task: %v", err)
	}
}

// After --, a value that starts with a dash is a task ID that fails alone.
func TestTaskCancelTakesDashedValuesAfterDoubleDash(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	running := sendNoWait(t, "-data-dir", dir, "helper", "take forever")
	<-fix.prompts
	run := startTaskCancel("-data-dir", dir, "--", "helper", "-bad-id", running)
	err := run.wait(t)
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "1 of 2 tasks not cancelled") {
		t.Fatalf("got %v (exit %d)", err, exitCode(err))
	}
	if out := run.out.String(); out != "\"-bad-id\" is not a task ID\ntask "+running+" cancelled\n" {
		t.Fatalf("stdout %q", out)
	}
}

// When the cancel fails and so does reading the task, the cancel's error
// is reported, with the read's.
func TestTaskCancelKeepsTheCancelError(t *testing.T) {
	net := newFakeNetwork(t)
	remote := randomPeer(t)
	net.offer(remote)
	net.mu.Lock()
	net.routes[remote] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "mesh route down", http.StatusBadGateway)
	})
	net.mu.Unlock()
	id := "0f9a7c52-0000-4000-8000-000000000000"
	run := startTaskCancel("-data-dir", net.dir, "-json", "helper", id)
	if err := run.wait(t); exitCode(err) != 1 {
		t.Fatalf("got %v", err)
	}
	var shown []cancelled
	if err := json.Unmarshal([]byte(run.out.String()), &shown); err != nil || len(shown) != 1 {
		t.Fatalf("json %q: %v", run.out.String(), err)
	}
	if e := shown[0].Error; !strings.HasPrefix(e, "failed: no answer from helper") || !strings.Contains(e, "(reading the task failed too: ") {
		t.Fatalf("error %q", e)
	}
}
