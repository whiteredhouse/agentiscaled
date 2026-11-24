package node

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

const mcpInitializeMessage = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`

func newTestNetworkGrant(t *testing.T, task a2a.TaskID) (*taskNetwork, *networkGrant) {
	t.Helper()
	network := &taskNetwork{grants: map[[32]byte]*networkGrant{}, sent: map[a2a.TaskID]*sentTasks{}}
	g, err := network.grantFor("caller", task)
	if err != nil {
		t.Fatal(err)
	}
	return network, g
}

func mcpSessionRequest(t *testing.T, bearer, session, message string) *http.Request {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(message))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	}
	return req
}

// Answers arrive over ACP stdio, so resuming a turn need not send any MCP
// traffic. The same MCP session must last across consecutive answer waits.
func TestNetworkSessionSurvivesTwoAnswerWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const task = "waiting-task"
		network, g := newTestNetworkGrant(t, task)
		defer network.forget(task)
		var session string
		post := func(message string) *httptest.ResponseRecorder {
			req := mcpSessionRequest(t, g.bearer, session, message)
			res := httptest.NewRecorder()
			network.ServeHTTP(res, req)
			return res
		}
		end := g.begin(t.Context(), "first", time.Now().Add(taskDeadline))
		init := post(mcpInitializeMessage)
		session = init.Header().Get("Mcp-Session-Id")
		if init.Code != http.StatusOK || session == "" {
			t.Fatalf("initialize: HTTP %d, session %q, %s", init.Code, session, init.Body)
		}
		if res := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); res.Code != http.StatusAccepted {
			t.Fatalf("initialized: HTTP %d, %s", res.Code, res.Body)
		}
		for _, token := range []string{"second", "third"} {
			end()
			time.Sleep(40 * time.Minute)
			end = g.begin(t.Context(), token, time.Now().Add(taskDeadline))
		}
		const listTools = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
		if res := post(listTools); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "list_agents") {
			t.Fatalf("original session after two waits: HTTP %d, %s", res.Code, res.Body)
		}
		end()
		network.forget(task)
		synctest.Wait()
		for range g.server.Sessions() {
			t.Fatal("the MCP session remained open after task cleanup")
		}
		if res := post(listTools); res.Code != http.StatusUnauthorized {
			t.Fatalf("ended task's bearer received HTTP %d", res.Code)
		}
	})
}

func TestNetworkClosesSessionInitializingWhenGrantEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const task = "initializing-task"
		network, g := newTestNetworkGrant(t, task)
		defer func() {
			for session := range g.server.Sessions() {
				session.Close()
			}
		}()
		entered, resume := make(chan struct{}), make(chan struct{})
		sdk := g.handler
		g.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The grant admitted this request, but the SDK has not registered its session.
			close(entered)
			<-resume
			sdk.ServeHTTP(w, r)
		})
		req := mcpSessionRequest(t, g.bearer, "", mcpInitializeMessage)
		res := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			network.ServeHTTP(res, req)
		}()
		select {
		case <-entered:
		case <-done:
			t.Fatalf("initialize returned before session registration: HTTP %d, %s", res.Code, res.Body)
		}

		go network.forget(task)
		synctest.Wait()
		close(resume)
		<-done
		synctest.Wait()
		for range g.server.Sessions() {
			t.Fatal("an MCP session created during grant cleanup remained open")
		}
	})
}

func TestNetworkForgetInterruptsIncompleteHandshake(t *testing.T) {
	const task = "streaming-initialize"
	network, g := newTestNetworkGrant(t, task)
	handled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handled)
		network.ServeHTTP(w, r)
	}))
	defer srv.Close()

	// The server sends 100 Continue when the MCP handler starts reading the body.
	reading := make(chan struct{})
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{Got100Continue: func() { close(reading) }})
	reader, writer := io.Pipe()
	defer reader.Close()
	body := io.MultiReader(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize",`), reader)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/mcp", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+g.bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Expect", "100-continue")
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		if res, err := srv.Client().Do(req); err == nil {
			res.Body.Close()
		}
	}()
	defer func() {
		writer.Close()
		<-clientDone
	}()
	select {
	case <-reading:
	case <-clientDone:
		t.Fatal("request ended before the MCP handler read the body")
	case <-time.After(5 * time.Second):
		t.Fatal("MCP handler did not start reading the body")
	}

	stopped := make(chan struct{})
	go func() {
		network.forget(task)
		<-handled
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("grant cleanup did not interrupt the unfinished request")
	}
}
