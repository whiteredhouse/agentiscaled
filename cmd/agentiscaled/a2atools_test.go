package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testAgentCard() *a2a.AgentCard {
	return &a2a.AgentCard{
		Name: "Configured agent", Description: "Works in the receiver's configured directory.", Version: "1",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface("http://agentiscaled/mesh/peer/a2a/agent", a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{{ID: "task", Name: "Task", Description: "Performs a generic task", Tags: []string{"task"}}},
	}
}

// The actual MCP bridge reaches the node's exact card route over its Unix
// socket. A refresh must observe refusal/offline rather than reuse an old card.
func TestMCPAgentCardTracksServiceAvailability(t *testing.T) {
	directory, err := os.MkdirTemp("", "ais-card-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "node.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	node := mcp.NewServer(&mcp.Implementation{Name: "test-node", Version: "test"}, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return node }, nil))
	var status atomic.Int32
	status.Store(http.StatusOK)
	var requests atomic.Int32
	card := testAgentCard()
	mux.HandleFunc("/mesh/"+testSender+"/a2a/agent/.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("X-Mesh-Authentication") != "" {
			t.Error("card request changed the local socket authentication contract")
		}
		if code := int(status.Load()); code != http.StatusOK {
			http.Error(w, "service unavailable", code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	bridgeInput, clientOutput := io.Pipe()
	clientInput, bridgeOutput := io.Pipe()
	bridgeResult := make(chan error, 1)
	go func() {
		bridgeResult <- bridgeMCP(ctx, &mcp.IOTransport{Reader: bridgeInput, Writer: bridgeOutput}, socket)
	}()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "card-test", Version: "test"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: clientInput, Writer: clientOutput}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case err := <-bridgeResult:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("bridge did not close")
		}
	})
	tools, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "get_agent_card" && (tool.Annotations == nil || !tool.Annotations.ReadOnlyHint) {
			t.Error("card tool should be annotated read-only")
		}
	}
	for _, code := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusOK, http.StatusBadGateway} {
		status.Store(int32(code))
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "get_agent_card", Arguments: map[string]any{"peer_id": testSender, "service": "agent"}})
		if err != nil || result == nil {
			t.Fatalf("call: %v", err)
		}
		if code != http.StatusOK {
			if !result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "HTTP ") {
				t.Fatalf("unavailable service produced a successful card: %+v", result)
			}
			continue
		}
		if result.IsError || len(result.Content) != 1 {
			t.Fatalf("card: %+v", result)
		}
		var got a2a.AgentCard
		if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &got); err != nil || !reflect.DeepEqual(&got, card) {
			t.Fatalf("card changed: %+v / %v", got, err)
		}
	}
	if requests.Load() != 4 {
		t.Fatalf("expected four fresh card requests, got %d", requests.Load())
	}
}

func TestAgentCardRejectsUnsafeTargetsBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	tools := &a2aTasks{http: server.Client(), base: server.URL}
	for _, target := range []taskTarget{
		{PeerID: "not-a-peer", Service: "agent"},
		{PeerID: testSender, Service: ""},
		{PeerID: testSender, Service: "."},
		{PeerID: testSender, Service: ".."},
		{PeerID: testSender, Service: "agent/../../other"},
		{PeerID: testSender, Service: "agent?query"},
		{PeerID: testSender, Service: "agent\n"},
	} {
		if _, _, err := tools.card(context.Background(), nil, target); err == nil {
			t.Fatalf("unsafe target accepted: %+v", target)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid target caused a request")
	}
}

func TestAgentCardRejectsRedirectsAndMalformedResponses(t *testing.T) {
	valid, err := json.Marshal(testAgentCard())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"null": "null", "empty": "{}", "non-card": "[]", "trailing JSON": string(valid) + "{}",
		"trailing garbage": string(valid) + "garbage", "oversized": string(valid) + strings.Repeat(" ", maxAgentCardBytes),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			tools := &a2aTasks{http: server.Client(), base: server.URL}
			if _, _, err := tools.card(context.Background(), nil, taskTarget{PeerID: testSender, Service: "agent"}); err == nil {
				t.Fatal("invalid or oversized card accepted")
			}
		})
	}
	t.Run("redirect", func(t *testing.T) {
		var redirected atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/other-route" {
				redirected.Add(1)
				_, _ = w.Write(valid)
				return
			}
			http.Redirect(w, r, "/other-route", http.StatusFound)
		}))
		defer server.Close()
		tools := &a2aTasks{http: server.Client(), base: server.URL}
		if _, _, err := tools.card(context.Background(), nil, taskTarget{PeerID: testSender, Service: "agent"}); err == nil || redirected.Load() != 0 {
			t.Fatal("agent card followed a redirect")
		}
	})
}

// A task call, like a card read, stays on the route it chose: a redirect
// from the agent is an error, not a request to another service through the
// node.
func TestTaskCallsRejectRedirects(t *testing.T) {
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/other-route" {
			redirected.Add(1)
			http.Error(w, "followed", http.StatusTeapot)
			return
		}
		http.Redirect(w, r, "/other-route", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	tools := &a2aTasks{http: server.Client(), base: server.URL}
	target := taskTarget{PeerID: testSender, Service: "agent"}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"send":   func() error { _, _, err := tools.send(ctx, nil, sendTaskIn{taskTarget: target, Text: "x"}); return err },
		"answer": func() error { _, _, err := tools.send(ctx, nil, sendTaskIn{taskTarget: target, Text: "x", TaskID: "t-1"}); return err },
		"get":    func() error { _, _, err := tools.get(ctx, nil, getTaskIn{taskIn: taskIn{taskTarget: target, TaskID: "t-1"}}); return err },
		"wait":   func() error { _, _, err := tools.get(ctx, nil, getTaskIn{taskIn: taskIn{taskTarget: target, TaskID: "t-1"}, WaitSeconds: 1}); return err },
		"cancel": func() error { _, _, err := tools.cancel(ctx, nil, taskIn{taskTarget: target, TaskID: "t-1"}); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: no error from a redirect", name)
		}
		if n := redirected.Swap(0); n != 0 {
			t.Errorf("%s followed a redirect %d times", name, n)
		}
	}
}

func TestAgentCardHonorsClientCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	tools := &a2aTasks{http: server.Client(), base: server.URL}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := tools.card(ctx, nil, taskTarget{PeerID: testSender, Service: "agent"})
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("card request ignored cancellation")
	}
}
