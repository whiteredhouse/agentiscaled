package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// wrapACP runs the usual fake agent through a shell script in each task's directory.
func wrapACP(t *testing.T, r *acpRuntime, script string) {
	t.Helper()
	program := r.command
	r.command = filepath.Join(t.TempDir(), "wrapped-acp")
	r.args = append([]string{program}, r.args...)
	if err := os.WriteFile(r.command, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestACPShutdownJoinsActiveAndWaitingAgents(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status", "-ignore-cancel")
	wrapACP(t, r, "echo $$ > agent.pid\nexec \"$@\"\n")
	t.Cleanup(r.releaseAll)
	waitingDir, activeDir := t.TempDir(), t.TempDir()
	question, err := r.Run(t.Context(), waitingDir, taskRequest{Task: "waiting", Text: "ASK-ME"})
	if err != nil || !question.Question {
		t.Fatalf("waiting task: %+v, %v", question, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	reportStart := sync.OnceFunc(func() { close(started) })
	result := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, activeDir, taskRequest{Task: "active", Text: "SLOW", Progress: func(string) { reportStart() }})
		result <- err
	}()
	defer cancel()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("active task ended before reporting progress: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("active task did not report progress")
	}

	var pids []int
	for _, dir := range []string{activeDir, waitingDir} {
		record := readRecord(t, filepath.Join(dir, "agent.pid"))
		pid, err := strconv.Atoi(strings.TrimSpace(record))
		if err != nil || pid <= 0 {
			t.Fatalf("invalid process ID %q", record)
		}
		pids = append(pids, pid)
	}

	forceStop := func() {
		cancel()
		for _, pid := range pids {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
	stopped := make(chan struct{})
	go func() { r.releaseAll(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(acpCancelGrace + 5*time.Second):
		forceStop()
		t.Fatal("shutdown did not finish")
	}
	for _, pid := range pids {
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			cancel()
			t.Errorf("agent %d was still present when shutdown returned: %v", pid, err)
		}
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("active turn ended with %v, want cancellation", err)
		}
	case <-time.After(acpCancelGrace + 5*time.Second):
		forceStop()
		t.Fatal("active task did not finish")
	}
}

func TestACPCompletingAgentCanSaveAfterClosingStdout(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status")
	// The agent finishes its ACP messages, sees EOF on stdin, and closes
	// stdout before spending a little time persisting its session.
	wrapACP(t, r, "\"$@\"\nexec 1>&-\nsleep 0.2\nprintf saved > session-state\n")
	dir := t.TempDir()
	res, err := r.Run(t.Context(), dir, taskRequest{Task: "done", Text: "finish"})
	if err != nil || res.Text == "" || res.Question {
		t.Fatalf("completed turn: %+v, %v", res, err)
	}
	if saved := readRecord(t, filepath.Join(dir, "session-state")); saved != "saved" {
		t.Errorf("saved session: %q", saved)
	}
}

func TestACPCloseIsBoundedWithInheritedStderr(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("this fixture uses procfs to duplicate the agent's stderr")
	}
	r := newFakeACP(t, ownerPolicy)
	s := startFake(t, r)
	closeSession := sync.OnceFunc(s.close)
	t.Cleanup(closeSession)

	// Give a process outside the agent's group a copy of its stderr. The
	// test owns this process and reaps it after the session closes.
	stderr, err := os.OpenFile(fmt.Sprintf("/proc/%d/fd/2", s.cmd.Process.Pid), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	holder := exec.Command("cat")
	holder.Stderr = stderr
	input, err := holder.StdinPipe()
	if err != nil {
		stderr.Close()
		t.Fatal(err)
	}
	err = holder.Start()
	stderr.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		input.Close()
		if err := holder.Wait(); err != nil {
			t.Errorf("stderr holder: %v", err)
		}
	})
	closed := make(chan struct{})
	go func() { closeSession(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(acpCancelGrace + 5*time.Second):
		t.Fatal("the agent exited, but inherited stderr blocked shutdown")
	}
}

func askACPQuestion(t *testing.T, r *acpRuntime, dir string) (turnResult, *acpSession) {
	t.Helper()
	question, err := r.Run(t.Context(), dir, taskRequest{Task: "question", Text: "ASK-ME"})
	if err != nil || !question.Question {
		t.Fatalf("expected a question, got %+v, %v", question, err)
	}
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	kept := r.kept["question"]
	if kept == nil {
		t.Fatal("the process that asked was not kept")
	}
	return question, kept.s
}

func TestACPLoadsAfterWaitingAgentExitsWithStdoutOpen(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("this fixture uses procfs to duplicate the agent's stdout")
	}
	r := newFakeACP(t, ownerPolicy, "-status", "-load")
	t.Cleanup(r.releaseAll)
	dir := t.TempDir()
	question, s := askACPQuestion(t, r, dir)

	// Keep stdout open as an inheriting process would. Its EOF cannot tell
	// the runtime that the agent has exited.
	stdout, err := os.OpenFile(fmt.Sprintf("/proc/%d/fd/1", s.cmd.Process.Pid), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.closed:
	case <-time.After(acpCancelGrace + 5*time.Second):
		t.Fatal("waiting agent exited but was not reaped")
	}
	if err := syscall.Kill(s.cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("waiting agent still present after reaping: %v", err)
	}
	wrapACP(t, r, "tee -a requests.jsonl | \"$@\"\n")
	answer, err := r.Run(t.Context(), dir, taskRequest{Task: "question", Session: question.Session, Text: "blue"})
	if err != nil || answer.Question || !strings.Contains(answer.Text, "prompts=1 ") || !strings.Contains(answer.Text, "answer=true") {
		t.Fatalf("new process did not resume the question: %+v, %v", answer, err)
	}
	if requests := readRecord(t, filepath.Join(dir, "requests.jsonl")); !strings.Contains(requests, `"method":"session/load"`) {
		t.Fatal("the replacement process received no session/load request")
	}
}

func TestACPShutdownReapsBeforeBlockedAnswerPrompt(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status")
	t.Cleanup(r.releaseAll)
	dir := t.TempDir()
	question, s := askACPQuestion(t, r, dir)

	// A previous protocol write holds the lock needed to start the answer.
	// Only release it after shutdown has reaped the process.
	s.order.Lock()
	unlock := sync.OnceFunc(s.order.Unlock)
	defer unlock()
	result := make(chan error, 1)
	go func() {
		_, err := r.Run(t.Context(), dir, taskRequest{Task: "question", Session: question.Session, Text: "blue"})
		result <- err
	}()
	waitFor(t, func() bool {
		r.sessionsMu.Lock()
		defer r.sessionsMu.Unlock()
		return r.sessions[s] != nil
	})
	stopped := make(chan struct{})
	go func() { r.releaseAll(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(acpCancelGrace + 5*time.Second):
		t.Fatal("shutdown waited for the blocked prompt")
	}
	if err := syscall.Kill(s.cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("shutdown returned before the process was reaped: %v", err)
	}
	unlock()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("answer ended with %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("answer did not return after releasing the lock")
	}
}
