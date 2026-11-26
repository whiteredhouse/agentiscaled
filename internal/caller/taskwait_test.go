package caller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExitCode(t *testing.T) {
	if ExitCode(nil) != 0 || ExitCode(errors.New("x")) != 1 {
		t.Fatal("plain errors")
	}
	err := safeError(withExit(exitTimeout, errors.New("late \x1b[2J")))
	if ExitCode(err) != exitTimeout || err.Error() != "late " {
		t.Fatalf("safeError: %q exit %d", err, ExitCode(err))
	}
	if withExit(exitTimeout, nil) != nil {
		t.Fatal("withExit")
	}
}

// When the final read also times out, report the task and how to wait for it again.
func TestTaskWaitReportsTheTaskWhenItsStateCannotBeRead(t *testing.T) {
	defer func(d time.Duration) { lastReadTimeout = d }(lastReadTimeout)
	lastReadTimeout = 100 * time.Millisecond
	hang := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-hang:
		}
	}))
	defer server.Close()
	defer close(hang)
	const remote = "12D3KooWRBy97UB99e3J6hiPesre1MZeuNQvfan4gBziswrRJsNK"
	const id = "0f9a7c52-0000-4000-8000-000000000000"
	o := taskSendOptions{dataDir: t.TempDir(), json: true, timeout: 300 * time.Millisecond}
	ref := TaskRef{Target{PeerID: remote, Service: "helper"}, id}
	deadline := time.Now().Add(o.timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var out strings.Builder
	err := waitForTask(ctx, deadline, &Client{Mesh: server.Client(), BaseURL: server.URL}, ref, o, &out, io.Discard)
	if ExitCode(err) != exitTimeout || !strings.Contains(err.Error(), "task "+id+" has not settled") || !strings.Contains(err.Error(), "wait again with: agentiscaled task wait -data-dir ") || !strings.Contains(err.Error(), " "+ref.String()+"\nor cancel it with: agentiscaled task cancel -data-dir ") {
		t.Fatalf("got %v (exit %d)", err, ExitCode(err))
	}
	var shown Task
	if err := json.Unmarshal([]byte(out.String()), &shown); err != nil || shown.TaskID != id || shown.Ref != ref.String() {
		t.Fatalf("json %q: %+v %v", out.String(), shown, err)
	}
}
