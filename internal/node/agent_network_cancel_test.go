package node

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runtimeInCancelGrace stays inside Run until the test allows cleanup to finish.
type runtimeInCancelGrace struct {
	*fixture
	finish chan struct{}
}

func (r runtimeInCancelGrace) Run(ctx context.Context, _ string, req taskRequest) (turnResult, error) {
	r.prompts <- req
	<-r.finish
	return turnResult{}, ctx.Err()
}

func TestCancelledTaskLosesNetworkWhileRuntimeIsStopping(t *testing.T) {
	h := newAgentHarness(t, testSender)
	network, calls := fakeNodeNetworkServer(t)
	network.node = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"access_token":"test-token"}`))}, nil
	})}
	h.svc.network = network
	rt := runtimeInCancelGrace{h.fix, make(chan struct{})}
	h.svc.rt = rt
	finish := sync.OnceFunc(func() { close(rt.finish) })
	defer finish()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	task := h.send(t, "", "work", nil)
	req := <-rt.prompts
	if req.Network == nil {
		t.Fatal("the runtime received no network access")
	}
	session, err := networkSession(ctx, req.Network)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tool := &mcp.CallToolParams{Name: "list_agents"}
	if result, err := session.CallTool(ctx, tool); err != nil || result.IsError {
		t.Fatalf("list_agents during the run: %v, %v", result, err)
	}
	before := calls()
	if !strings.Contains(before, "GET /info Bearer test-token") {
		t.Fatalf("the running task did not reach the node: %q", before)
	}

	// CancelTask waits for Run to return. GetTask exposes the terminal state
	// while the runtime is still shutting down with the same MCP session.
	cancelled := make(chan error, 1)
	go func() {
		_, err := h.client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: task.ID})
		cancelled <- err
	}()
	h.await(t, task.ID, a2a.TaskStateCanceled)
	if result, err := session.CallTool(ctx, tool); err == nil && !result.IsError {
		t.Error("list_agents succeeded after the task was cancelled")
	}
	if after := calls(); after != before {
		t.Errorf("the cancelled task reached the node: before %q, after %q", before, after)
	}
	finish()
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
}
