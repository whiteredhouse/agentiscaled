package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// answer sends text on task id, as the caller's answer to the agent's
// question.
func answer(c *a2aclient.Client, id a2a.TaskID, msgID, text string) (*a2a.Task, error) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID = id
	if msgID != "" {
		msg.ID = msgID
	}
	res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		return nil, err
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		return nil, fmt.Errorf("got %T, want a task", res)
	}
	return task, nil
}

func (h *agentHarness) answer(t *testing.T, id a2a.TaskID, msgID, text string) *a2a.Task {
	t.Helper()
	task, err := answer(h.client, id, msgID, text)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// The agent asks; the task waits in input-required with the question; the
// caller's answer on the same task resumes the agent's session.
func TestTheAgentAsksAndTheSenderAnswers(t *testing.T) {
	h := newAgentHarness(t, testSender)
	task := h.send(t, "", "double a number", nil)
	if p := <-h.fix.prompts; p.Session != "" {
		t.Fatalf("a new task resumed session %q", p.Session)
	}
	h.fix.ask <- "Which number?"
	asked := h.await(t, task.ID, a2a.TaskStateInputRequired)
	if statusText(asked) != "Which number?" || terminal(asked) != "" {
		t.Fatalf("question: %q (terminal %q)", statusText(asked), terminal(asked))
	}

	// The slot is free while the task waits: another task runs meanwhile.
	waitIdle(t, h.svc)
	other := h.send(t, "", "review", nil)
	<-h.fix.prompts
	h.fix.release <- "No findings"
	h.await(t, other.ID, a2a.TaskStateCompleted)

	h.answer(t, task.ID, "", "21")
	p := <-h.fix.prompts
	if p.Session != "session-double a number" || p.Text != "21" || p.Sender != testSender {
		t.Fatalf("answer reached the runtime as %+v", p)
	}
	h.fix.release <- "42"
	done := h.await(t, task.ID, a2a.TaskStateCompleted)
	if got := describeTask(done); got.Result != "42" || got.Terminal != "completed" {
		t.Fatalf("result: %+v", got)
	}
	// A finished task takes no more messages.
	if _, err := answer(h.client, task.ID, "", "again"); err == nil {
		t.Fatal("a completed task took another message")
	}
}

// An answer that cannot be taken now is refused without ending the task,
// which still waits for one that can.
func TestAnAnswerWhileBusyIsRefusedAndTheTaskKeepsWaiting(t *testing.T) {
	h := newAgentHarness(t, testSender)
	task := h.send(t, "", "double a number", nil)
	<-h.fix.prompts
	h.fix.ask <- "Which number?"
	h.await(t, task.ID, a2a.TaskStateInputRequired)
	waitIdle(t, h.svc)

	busy := h.send(t, "", "review", nil)
	<-h.fix.prompts
	for text, want := range map[string]string{"21": "already running", " ": "no text"} {
		got := h.answer(t, task.ID, "", text)
		if got.Status.State != a2a.TaskStateInputRequired || !strings.Contains(statusText(got), want) || !strings.Contains(statusText(got), "Which number?") {
			t.Fatalf("answer %q: %s %q", text, got.Status.State, statusText(got))
		}
	}
	h.fix.release <- "No findings"
	h.await(t, busy.ID, a2a.TaskStateCompleted)
	waitIdle(t, h.svc)

	h.answer(t, task.ID, "a-1", "21")
	<-h.fix.prompts
	h.fix.ask <- "Doubled or squared?"
	h.await(t, task.ID, a2a.TaskStateInputRequired)
	waitIdle(t, h.svc)
	// A retried answer is not taken as the answer to the next question.
	if got := h.answer(t, task.ID, "a-1", "21"); got.Status.State != a2a.TaskStateInputRequired || !strings.Contains(statusText(got), "already delivered") {
		t.Fatalf("retried answer: %s %q", got.Status.State, statusText(got))
	}
	h.answer(t, task.ID, "", "doubled")
	if p := <-h.fix.prompts; p.Text != "doubled" {
		t.Fatalf("second answer: %+v", p)
	}
	h.fix.release <- "42"
	h.await(t, task.ID, a2a.TaskStateCompleted)
}

// Only the sender can answer: to another peer the task does not exist.
func TestOnlyTheSenderCanAnswer(t *testing.T) {
	const other = "12D3KooWRBy97UB99e3J6hiPesre1MZeuNQvfan4gBziswrRJsNK"
	h := newAgentHarness(t, testSender)
	task := h.send(t, "", "double a number", nil)
	<-h.fix.prompts
	h.fix.ask <- "Which number?"
	h.await(t, task.ID, a2a.TaskStateInputRequired)
	waitIdle(t, h.svc)
	if _, err := answer(h.as(t, other), task.ID, "", "21"); err == nil {
		t.Fatal("another peer answered the question")
	}
	select {
	case p := <-h.fix.prompts:
		t.Fatalf("another peer's answer ran: %+v", p)
	default:
	}
	h.await(t, task.ID, a2a.TaskStateInputRequired)
}

func TestCancelATaskWaitingForAnAnswer(t *testing.T) {
	h := newAgentHarness(t, testSender)
	task := h.send(t, "", "double a number", nil)
	<-h.fix.prompts
	h.fix.ask <- "Which number?"
	h.await(t, task.ID, a2a.TaskStateInputRequired)
	waitIdle(t, h.svc)
	cancelled, err := h.client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status.State != a2a.TaskStateCanceled || cancelled.Metadata[metaExecutionBegun] != true {
		t.Fatalf("cancel: %s %v", cancelled.Status.State, cancelled.Metadata)
	}
	if h.svc.waitingFor(task.ID) != nil {
		t.Fatal("the cancelled task still waits")
	}
}

func TestAnAnswerAfterTheWaitEndsTheTask(t *testing.T) {
	defer func(d time.Duration) { answerWait = d }(answerWait)
	answerWait = 50 * time.Millisecond
	h := newAgentHarness(t, testSender)
	task := h.send(t, "", "double a number", nil)
	<-h.fix.prompts
	h.fix.ask <- "Which number?"
	h.await(t, task.ID, a2a.TaskStateInputRequired)
	time.Sleep(100 * time.Millisecond)
	h.answer(t, task.ID, "", "21")
	late := h.await(t, task.ID, a2a.TaskStateFailed)
	if terminal(late) != "timeout" || !strings.Contains(statusText(late), "no answer came") {
		t.Fatalf("late answer: %q %q", terminal(late), statusText(late))
	}
}

// The agent may ask a few questions, not without end.
func TestTheAgentAsksAtMostMaxQuestions(t *testing.T) {
	h := newAgentHarness(t, testSender)
	task := h.send(t, "", "double a number", nil)
	for i := 0; ; i++ {
		<-h.fix.prompts
		h.fix.ask <- "Which number?"
		if i == maxQuestions {
			break
		}
		h.await(t, task.ID, a2a.TaskStateInputRequired)
		waitIdle(t, h.svc)
		h.answer(t, task.ID, "", "I won't say")
	}
	failed := h.await(t, task.ID, a2a.TaskStateFailed)
	if !strings.Contains(statusText(failed), "more than") {
		t.Fatalf("failure: %q", statusText(failed))
	}
}

// The deadline bounds the task's running time over all its turns.
func TestTheDeadlineCoversEveryTurn(t *testing.T) {
	h := newAgentHarness(t, testSender)
	h.svc.deadline = 300 * time.Millisecond
	task := h.send(t, "", "double a number", nil)
	<-h.fix.prompts
	time.Sleep(200 * time.Millisecond)
	h.fix.ask <- "Which number?"
	h.await(t, task.ID, a2a.TaskStateInputRequired)
	waitIdle(t, h.svc)
	h.answer(t, task.ID, "", "21")
	<-h.fix.prompts
	start := time.Now()
	timedOut := h.await(t, task.ID, a2a.TaskStateFailed)
	if terminal(timedOut) != "timeout" || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("second turn: %q after %s", terminal(timedOut), time.Since(start))
	}
}

func TestParseTurn(t *testing.T) {
	for _, tc := range []struct {
		message string
		want    turnResult
		err     bool
	}{
		{`{"status":"done","text":" 42 "}`, turnResult{Text: "42", Session: "s"}, false},
		{`{"status":"needs_input","text":"Which number?"}`, turnResult{Text: "Which number?", Question: true, Session: "s"}, false},
		{`{"status":"needs_input","text":" "}`, turnResult{}, true},
		{`{"status":"maybe","text":"x"}`, turnResult{}, true},
		{"plain answer\n", turnResult{Text: "plain answer", Session: "s"}, false},
		{`{"other":"json"}`, turnResult{Text: `{"other":"json"}`, Session: "s"}, false},
	} {
		got, err := parseTurn(tc.message, "s")
		if (err != nil) != tc.err || (!tc.err && got != tc.want) {
			t.Errorf("parseTurn(%q) = %+v, %v", tc.message, got, err)
		}
	}
}

// An answer resumes the Codex session with the options `codex exec resume`
// takes: the same configuration, without -C, -s and --color.
func TestCodexRuntimeResumesTheSession(t *testing.T) {
	dir := t.TempDir()
	command, record := fakeCodexRecorder(t, dir)
	rt := &codexRuntime{command: command, home: filepath.Join(dir, "agent-codex"), def: &agentDefinition{WrittenFor: "codex", ReadOnly: true, Instructions: "x"},
		policy: agentPolicy{OwnerConfig: true}}
	const session = "0199a213-81c0-7800-8aa1-bbab2a035a53"
	if _, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "main.go", Session: session}); err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(readRecord(t, record)), "\n")
	if !slices.Equal(args[:2], []string{"exec", "resume"}) || !slices.Equal(args[len(args)-2:], []string{session, "-"}) {
		t.Fatalf("resume arguments: %q", args)
	}
	joined := " " + strings.Join(args, " ") + " "
	for _, avoid := range []string{" -C ", " -s ", " --color "} {
		if strings.Contains(joined, avoid) {
			t.Errorf("resume arguments %q have %q", joined, avoid)
		}
	}
	for _, want := range []string{` sandbox_mode="read-only" `, " --output-schema ", " --json "} {
		if !strings.Contains(joined, want) {
			t.Errorf("resume arguments %q lack %q", joined, want)
		}
	}
	if _, err := rt.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "x", Session: "--last"}); err == nil {
		t.Fatal("resumed a session that is not a session ID")
	}
}

func TestCodexThread(t *testing.T) {
	events := []byte("{\"type\":\"thread.started\",\"thread_id\":\"0199a213-81c0-7800-8aa1-bbab2a035a53\"}\n{\"type\":\"turn.started\"}\n")
	if got := codexThread(events); got != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
		t.Fatalf("thread %q", got)
	}
	if got := codexThread([]byte(`{"type":"thread.started","thread_id":"--last"}`)); got != "" {
		t.Fatalf("thread %q from a malformed id", got)
	}
}
