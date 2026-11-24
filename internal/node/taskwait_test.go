package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	meshnode "github.com/google/agentmesh/export/node"
	"github.com/google/uuid"
	"github.com/whiteredhouse/agentiscaled/internal/caller"
)

// startTaskWait runs `agentiscaled task wait args...` in the background.
func startTaskWait(args ...string) *sendRun {
	r := &sendRun{out: &syncBuffer{}, errOut: &syncBuffer{}, done: make(chan error, 1)}
	go func() {
		r.done <- caller.RunTaskWait(context.Background(), args, r.out, r.errOut)
	}()
	return r
}

// sendNoWait sends a task with -no-wait and returns its ID.
func sendNoWait(t *testing.T, args ...string) string {
	t.Helper()
	run := startTaskSend("", append([]string{"-no-wait"}, args...)...)
	if err := run.wait(t); err != nil {
		t.Fatalf("task send -no-wait: %v\n%s", err, run.errOut)
	}
	id := strings.TrimSuffix(run.out.String(), "\n")
	if err := uuid.Validate(id); err != nil {
		t.Fatalf("task send -no-wait printed %q, want the task ID alone", run.out.String())
	}
	if !strings.Contains(run.errOut.String(), "follow it with: agentiscaled task wait ") || !strings.HasSuffix(run.errOut.String(), " helper "+id+"\n") {
		t.Fatalf("stderr: %s", run.errOut)
	}
	return id
}

// A task sent without waiting runs on; task wait then prints its steps and
// its answer.
func TestTaskWaitFollowsATaskSentWithoutWaiting(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	id := sendNoWait(t, "-data-dir", dir, "helper", "Review the changes")
	if p := <-fix.prompts; p.Text != "Review the changes" {
		t.Fatalf("the agent got %q", p.Text)
	}
	run := startTaskWait("-data-dir", dir, "helper", id)
	fix.steps <- "Running go test ./...\x1b[2J"
	waitFor(t, func() bool { return strings.Contains(run.errOut.String(), "helper: Running go test ./...\n") })
	fix.release <- "No findings"
	if err := run.wait(t); err != nil {
		t.Fatalf("task wait: %v\n%s", err, run.errOut)
	}
	if got := run.out.String(); got != "No findings\n" {
		t.Fatalf("stdout %q, want the answer alone", got)
	}

	// Waiting for a task that has ended answers at once.
	run = startTaskWait("-data-dir", dir, "-json", "helper", id)
	if err := run.wait(t); err != nil {
		t.Fatal(err)
	}
	var got caller.Task
	if err := json.Unmarshal([]byte(run.out.String()), &got); err != nil || got.TaskID != id || got.Terminal != "completed" || got.Result != "No findings" {
		t.Fatalf("json %q: %+v %v", run.out.String(), got, err)
	}
}

// A question ends the wait with exit status 3 and the command that
// answers it; after the answer, waiting again gets the result.
func TestTaskWaitStopsAtAQuestion(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	id := sendNoWait(t, "-data-dir", dir, "helper", "double a number")
	<-fix.prompts
	fix.ask <- "Which number?"
	err := startTaskWait("-data-dir", dir, "helper", id).wait(t)
	if caller.ExitCode(err) != 3 || !strings.Contains(err.Error(), "helper asks: Which number?") || !strings.Contains(err.Error(), "-task "+id+" helper -- ") {
		t.Fatalf("got %v (exit %d)", err, caller.ExitCode(err))
	}

	if again := sendNoWait(t, "-data-dir", dir, "-task", id, "helper", "21"); again != id {
		t.Fatalf("the answer went to task %s, want %s", again, id)
	}
	if p := <-fix.prompts; p.Text != "21" {
		t.Fatalf("answer reached the agent as %+v", p)
	}
	run := startTaskWait("-data-dir", dir, "helper", id)
	fix.release <- "42"
	if err := run.wait(t); err != nil || run.out.String() != "42\n" {
		t.Fatalf("wait after the answer: %v %q", err, run.out.String())
	}
}

// Out of time, task wait exits with status 4 and leaves the task running.
func TestTaskWaitTimesOutWithoutStoppingTheTask(t *testing.T) {
	_, fix, tasks, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	id := sendNoWait(t, "-data-dir", dir, "helper", "take a while")
	<-fix.prompts
	run := startTaskWait("-data-dir", dir, "-timeout", "1s", "-json", "helper", id)
	err := run.wait(t)
	if caller.ExitCode(err) != 4 || !strings.Contains(err.Error(), "is still working: no answer within 1s; it goes on, wait again with: agentiscaled task wait ") {
		t.Fatalf("got %v (exit %d)", err, caller.ExitCode(err))
	}
	var shown caller.Task
	if err := json.Unmarshal([]byte(run.out.String()), &shown); err != nil || shown.TaskID != id || shown.State != "working" {
		t.Fatalf("json %q: %+v %v", run.out.String(), shown, err)
	}
	_, got, gerr := tasks.GetTool(context.Background(), nil, caller.GetInput{TaskInput: caller.TaskInput{Target: caller.Target{Service: "helper"}, TaskID: id}})
	if gerr != nil || got.Terminal != "" {
		t.Fatalf("task after the wait timed out: %+v %v", got, gerr)
	}
	fix.release <- "done"
	if err := startTaskWait("-data-dir", dir, "helper", id).wait(t); err != nil {
		t.Fatalf("second wait: %v", err)
	}
}

// A task that fails ends the wait with exit status 2.
func TestTaskWaitReportsAFailedTask(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	fix.err = errors.New("the runtime crashed")
	id := sendNoWait(t, "-data-dir", dir, "helper", "do it")
	<-fix.prompts
	err := startTaskWait("-data-dir", dir, "helper", id).wait(t)
	if caller.ExitCode(err) != 2 || !strings.Contains(err.Error(), "task "+id+" ended failed") {
		t.Fatalf("got %v (exit %d)", err, caller.ExitCode(err))
	}
}

func TestTaskWaitFailsForAnUnknownTask(t *testing.T) {
	_, _, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	err := startTaskWait("-data-dir", dir, "helper", "0f9a7c52-0000-4000-8000-000000000000").wait(t)
	if caller.ExitCode(err) != 1 || !strings.Contains(err.Error(), "task 0f9a7c52-0000-4000-8000-000000000000 not found at helper") {
		t.Fatalf("got %v (exit %d)", err, caller.ExitCode(err))
	}
}

func TestTaskWaitUsage(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"-data-dir", dir},
		{"-data-dir", dir, "helper"},
		{"-data-dir", dir, "helper", "a", "b"},
		{"-data-dir", dir, "bad name", "abc"},
		{"-data-dir", dir, "helper", "-bad id"},
		{"-data-dir", dir, "helper", "abc def"},
		{"-data-dir", dir, "-timeout", "0s", "helper", "abc"},
	} {
		if err := startTaskWait(args...).wait(t); err == nil || caller.ExitCode(err) != 1 {
			t.Errorf("%q: %v", args, err)
		}
	}
}

// task send says with its exit status how the task ended, as task wait
// does.
func TestTaskSendExitStatus(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	run := startTaskSend("", "-data-dir", dir, "helper", "double a number")
	<-fix.prompts
	fix.ask <- "Which number?"
	if err := run.wait(t); caller.ExitCode(err) != 3 {
		t.Fatalf("question: %v (exit %d)", err, caller.ExitCode(err))
	}
	run = startTaskSend("", "-data-dir", dir, "-timeout", "1s", "helper", "take forever")
	<-fix.prompts
	if err := run.wait(t); caller.ExitCode(err) != 4 {
		t.Fatalf("timeout: %v (exit %d)", err, caller.ExitCode(err))
	}
}

// -timeout includes waiting for the node: with none running, the command
// gives up when it passes, with the timeout's exit status.
func TestTaskTimeoutCoversWaitingForTheNode(t *testing.T) {
	dir := shortTempDir(t)
	for name, run := range map[string]*sendRun{
		"wait": startTaskWait("-data-dir", dir, "-timeout", "200ms", "helper", "abc"),
		"send": startTaskSend("", "-data-dir", dir, "-timeout", "200ms", "helper", "hi"),
	} {
		start := time.Now()
		err := run.wait(t)
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("%s took %s with -timeout 200ms", name, took)
		}
		if caller.ExitCode(err) != 4 || !strings.Contains(err.Error(), "no answer within 200ms: no running agentiscaled") {
			t.Errorf("%s: %v (exit %d)", name, err, caller.ExitCode(err))
		}
	}
}

// A wait that times out at once still reports the task and how to wait
// again, as JSON with -json.
func TestTaskWaitReportsTheTaskWhenTimingOutEarly(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	id := sendNoWait(t, "-data-dir", dir, "helper", "take a while")
	<-fix.prompts
	run := startTaskWait("-data-dir", dir, "-timeout", "50ms", "-json", "helper", id)
	err := run.wait(t)
	if caller.ExitCode(err) != 4 || !strings.Contains(err.Error(), "wait again with: agentiscaled task wait ") {
		t.Fatalf("got %v (exit %d)", err, caller.ExitCode(err))
	}
	var shown caller.Task
	if err := json.Unmarshal([]byte(run.out.String()), &shown); err != nil || shown.TaskID != id {
		t.Fatalf("json %q: %+v %v", run.out.String(), shown, err)
	}
	fix.release <- "done"
}
