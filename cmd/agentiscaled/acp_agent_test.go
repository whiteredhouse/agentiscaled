package main

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// TestACPRealAgent offers a real ACP agent, given by its command line in
// AGENTISCALE_TEST_ACP, the way a node does (agent add acp), and sends it
// tasks over A2A: one that completes, one that asks a question and takes
// the answer, and one that is cancelled. The agent's model decides what it
// answers, so point it at a model that does the tasks, or a stand-in.
//
//	AGENTISCALE_TEST_ACP="claude-agent-acp" go test ./cmd/agentiscaled -run TestACPRealAgent -v
func TestACPRealAgent(t *testing.T) {
	line := os.Getenv("AGENTISCALE_TEST_ACP")
	if line == "" {
		t.Skip("set AGENTISCALE_TEST_ACP to an ACP agent's command line to run")
	}
	data := t.TempDir()
	a := agentConfig{Runtime: "acp", Command: line, Dir: t.TempDir()}
	if err := a.splitCommand(); err != nil {
		t.Fatal(err)
	}
	if err := checkAgent(&a, data, false); err != nil {
		t.Fatal(err)
	}
	svc, err := newAgent(&a, data)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.handler(a.Name, "secret"))
	t.Cleanup(srv.Close)
	h := &agentHarness{svc: svc, url: srv.URL, dir: a.Dir, client: a2aClient(t, srv.URL, meshHeaders{"secret", testSender})}

	wait := func(id a2a.TaskID, want a2a.TaskState) *a2a.Task {
		t.Helper()
		deadline := time.Now().Add(3 * time.Minute)
		for {
			task, err := h.client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id})
			if err != nil {
				t.Fatal(err)
			}
			if task.Status.State == want {
				return task
			}
			if task.Status.State.Terminal() || time.Now().After(deadline) {
				t.Fatalf("task %s: state %s (%s), want %s", id, task.Status.State, statusText(task), want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	done := wait(h.send(t, "", checkDoneTask, nil).ID, a2a.TaskStateCompleted)
	t.Logf("result %q", describeTask(done).Result)

	asked := wait(h.send(t, "", checkQuestionTask, nil).ID, a2a.TaskStateInputRequired)
	t.Logf("question %q", statusText(asked))
	h.answer(t, asked.ID, "", checkAnswer)
	answered := wait(asked.ID, a2a.TaskStateCompleted)
	t.Logf("answer %q", describeTask(answered).Result)

	long := h.send(t, "", checkLongTask, nil)
	wait(long.ID, a2a.TaskStateWorking)
	time.Sleep(2 * time.Second)
	if _, err := h.client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: long.ID}); err != nil {
		t.Fatal(err)
	}
	cancelled := wait(long.ID, a2a.TaskStateCanceled)
	if s := statusText(cancelled); strings.Contains(s, "stderr") {
		t.Errorf("cancelled task's status shows the agent's stderr: %q", s)
	}
}
