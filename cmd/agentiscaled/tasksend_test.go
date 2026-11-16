package main

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	meshnode "github.com/google/agentmesh/export/node"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// syncBuffer is an output that a test reads while task send writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type sendRun struct {
	out, errOut *syncBuffer
	done        chan error
}

// startTaskSend runs `agentiscaled task send args...` in the background.
func startTaskSend(stdin string, args ...string) *sendRun {
	r := &sendRun{out: &syncBuffer{}, errOut: &syncBuffer{}, done: make(chan error, 1)}
	go func() {
		r.done <- runTasks(context.Background(), append([]string{"send"}, args...), strings.NewReader(stdin), r.out, r.errOut)
	}()
	return r
}

func (r *sendRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatalf("task send did not return; stderr:\n%s", r.errOut.String())
		return nil
	}
}

func TestTaskSendCallsALocalAgent(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	run := startTaskSend("", "-data-dir", dir, "helper", "Review the changes")
	if p := <-fix.prompts; p.Text != "Review the changes" {
		t.Fatalf("the agent got %q", p.Text)
	}
	fix.steps <- "Running go test ./...\x1b[2J"
	waitFor(t, func() bool { return strings.Contains(run.errOut.String(), "helper: Running go test ./...\n") })
	fix.release <- "No findings"
	if err := run.wait(t); err != nil {
		t.Fatalf("task send: %v\n%s", err, run.errOut)
	}
	if got := run.out.String(); got != "No findings\n" {
		t.Fatalf("stdout %q, want the answer alone", got)
	}
	if e := run.errOut.String(); !strings.Contains(e, "sent task ") || !strings.Contains(e, "to helper on this node") {
		t.Fatalf("stderr: %s", e)
	}
}

// Flags may follow the agent and message, and the message can come on
// standard input.
func TestTaskSendReadsTheMessageFromStandardInput(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	run := startTaskSend("a long\nmessage\n", "helper", "-", "-data-dir", dir, "-json")
	if p := <-fix.prompts; p.Text != "a long\nmessage\n" {
		t.Fatalf("the agent got %q", p.Text)
	}
	fix.release <- "done"
	if err := run.wait(t); err != nil {
		t.Fatal(err)
	}
	var got taskOut
	if err := json.Unmarshal([]byte(run.out.String()), &got); err != nil || got.Terminal != "completed" || got.Result != "done" || got.TaskID == "" {
		t.Fatalf("json %q: %+v %v", run.out.String(), got, err)
	}
}

// A question ends the call with the command that answers it, which picks
// the task up where it waits.
func TestTaskSendAnswersAQuestion(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	run := startTaskSend("", "-data-dir", dir, "helper", "double a number")
	<-fix.prompts
	fix.ask <- "Which number?"
	err := run.wait(t)
	if err == nil || !strings.Contains(err.Error(), "helper asks: Which number?") || !strings.Contains(err.Error(), "-task ") {
		t.Fatalf("got %v", err)
	}
	if run.out.String() != "" {
		t.Fatalf("stdout %q for a question", run.out.String())
	}
	id := strings.Fields(err.Error()[strings.Index(err.Error(), "-task ")+len("-task "):])[0]

	run = startTaskSend("", "-data-dir", dir, "-task", id, "helper", "21")
	if p := <-fix.prompts; p.Text != "21" || p.Session != "session-double a number" {
		t.Fatalf("answer reached the agent as %+v", p)
	}
	fix.release <- "42"
	if err := run.wait(t); err != nil || run.out.String() != "42\n" {
		t.Fatalf("answer: %v %q", err, run.out.String())
	}
	if e := run.errOut.String(); !strings.Contains(e, "answered task "+id) {
		t.Fatalf("stderr: %s", e)
	}
}

// Out of time, task send cancels the task, so that the agent stops too.
func TestTaskSendCancelsOnTimeout(t *testing.T) {
	_, fix, tasks, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	run := startTaskSend("", "-data-dir", dir, "-timeout", "1s", "helper", "take forever")
	<-fix.prompts
	err := run.wait(t)
	if err == nil || !strings.Contains(err.Error(), "no answer within 1s; cancelled it") {
		t.Fatalf("got %v", err)
	}
	id := strings.Fields(strings.TrimPrefix(err.Error(), "task "))[0]
	_, got, gerr := tasks.get(context.Background(), nil, getTaskIn{taskIn: taskIn{taskTarget: taskTarget{Service: "helper"}, TaskID: strings.TrimSuffix(id, ":")}})
	if gerr != nil || got.Terminal != "cancelled" {
		t.Fatalf("task after timeout: %+v %v", got, gerr)
	}
}

func TestTaskSendFailsForAnUnknownAgent(t *testing.T) {
	net := newFakeNetwork(t)
	err := startTaskSend("", "-data-dir", net.dir, "nobody", "hi").wait(t)
	if err == nil || !strings.Contains(err.Error(), "no node of your network offers an agent named nobody") {
		t.Fatalf("got %v", err)
	}
}

// A remote agent is found by name, waiting for it while -wait-agent lasts,
// and called through the node's mesh route.
func TestTaskSendFindsARemoteAgent(t *testing.T) {
	defer func(d time.Duration) { findEvery = d }(findEvery)
	findEvery = 50 * time.Millisecond
	net := newFakeNetwork(t)
	h := newAgentHarness(t, testSender)
	remote := randomPeer(t)
	net.route(remote, h.url)
	go func() {
		time.Sleep(300 * time.Millisecond)
		net.offer(remote)
	}()
	run := startTaskSend("", "-data-dir", net.dir, "-wait-agent", "10s", "helper", "hello")
	<-h.fix.prompts
	h.fix.release <- "hi from afar"
	if err := run.wait(t); err != nil || run.out.String() != "hi from afar\n" {
		t.Fatalf("remote call: %v %q\n%s", err, run.out.String(), run.errOut)
	}
	if !strings.Contains(run.errOut.String(), "to helper on node "+remote) {
		t.Fatalf("stderr: %s", run.errOut)
	}
	if net.lookups.Load() < 2 {
		t.Fatalf("looked for the agent %d times, want it to wait", net.lookups.Load())
	}
}

func TestTaskSendNeedsAPeerWhenSeveralNodesOfferTheAgent(t *testing.T) {
	net := newFakeNetwork(t)
	a, b := randomPeer(t), randomPeer(t)
	net.offer(a)
	net.offer(b)
	err := startTaskSend("", "-data-dir", net.dir, "helper", "hi").wait(t)
	if err == nil || !strings.Contains(err.Error(), "2 nodes offer helper: pick one with -peer") || !strings.Contains(err.Error(), a) {
		t.Fatalf("got %v", err)
	}
}

func TestTaskSendUsage(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"-data-dir", dir},
		{"-data-dir", dir, "a", "b", "c"},
		{"-data-dir", dir, "bad name", "hi"},
		{"-data-dir", dir, "helper", "  "},
		{"-data-dir", dir, "-timeout", "0s", "helper", "hi"},
	} {
		if err := startTaskSend("", args...).wait(t); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
}

func TestParseInterleaved(t *testing.T) {
	for _, tc := range []struct {
		args []string
		pos  []string
		peer string
		js   bool
	}{
		{[]string{"a", "b"}, []string{"a", "b"}, "", false},
		{[]string{"-peer", "p", "a", "b"}, []string{"a", "b"}, "p", false},
		{[]string{"a", "-json", "b", "-peer", "p"}, []string{"a", "b"}, "p", true},
		{[]string{"a", "--", "-json"}, []string{"a", "-json"}, "", false},
		{[]string{"-json", "--", "-a", "-b"}, []string{"-a", "-b"}, "", true},
		{[]string{"a", "-"}, []string{"a", "-"}, "", false},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		p := fs.String("peer", "", "")
		j := fs.Bool("json", false, "")
		pos, err := parseInterleaved(fs, tc.args)
		if err != nil || !slices.Equal(pos, tc.pos) || *p != tc.peer || *j != tc.js {
			t.Errorf("%q: %q peer=%q json=%t %v", tc.args, pos, *p, *j, err)
		}
	}
}

// fakeNetwork stands in for a running node with no local agents: its socket
// answers discovery and routes remote A2A calls.
type fakeNetwork struct {
	dir     string
	mu      sync.Mutex
	offers  []string
	routes  map[string]http.Handler
	lookups atomic.Int32
}

func newFakeNetwork(t *testing.T) *fakeNetwork {
	n := &fakeNetwork{dir: shortTempDir(t), routes: map[string]http.Handler{}}
	ln, err := net.Listen("unix", filepath.Join(n.dir, meshnode.DefaultSocketName))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /mesh/service/discover", func(w http.ResponseWriter, r *http.Request) {
		n.lookups.Add(1)
		if r.URL.Query().Get("type") != "a2a" || r.URL.Query().Get("name") != "helper" {
			json.NewEncoder(w).Encode([]any{})
			return
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		found := []map[string]string{}
		for _, p := range n.offers {
			found = append(found, map[string]string{"peer_id": p, "srv_name": "helper", "local_proxy_url": "http://x"})
		}
		json.NewEncoder(w).Encode(found)
	})
	mux.HandleFunc("/mesh/{peer}/a2a/helper/", func(w http.ResponseWriter, r *http.Request) { n.serve(w, r) })
	mux.HandleFunc("/mesh/{peer}/a2a/helper", func(w http.ResponseWriter, r *http.Request) { n.serve(w, r) })
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return n
}

func (n *fakeNetwork) serve(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	h := n.routes[r.PathValue("peer")]
	n.mu.Unlock()
	if h == nil {
		http.Error(w, "no route", http.StatusBadGateway)
		return
	}
	http.StripPrefix("/mesh/"+r.PathValue("peer")+"/a2a/helper", h).ServeHTTP(w, r)
}

func (n *fakeNetwork) offer(p string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.offers = append(n.offers, p)
}

// route sends calls for helper on node p to the agent at agentURL, as
// the mesh delivers them from this node.
func (n *fakeNetwork) route(p, agentURL string) {
	u, _ := url.Parse(agentURL)
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.Transport = meshHeaders{"secret", testSender}
	proxy.FlushInterval = -1
	n.mu.Lock()
	defer n.mu.Unlock()
	n.routes[p] = proxy
}

func randomPeer(t *testing.T) string {
	_, pub, err := crypto.GenerateEd25519Key(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, err := peer.IDFromPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTerminalSafe(t *testing.T) {
	for in, want := range map[string]string{
		"plain\ttext\nnext":              "plain\ttext\nnext",
		"\x1b[2Jclear\x1b[31mred\x1b[0m": "clearred",
		"\x1b]0;title\x07after":          "after",
		"\x1b]8;;http://x\x1b\\link":     "link",
		"bell\x07\rcr\x00nul":            "bellcrnul",
	} {
		if got := terminalSafe(in); got != want {
			t.Errorf("terminalSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
