package main

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
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
	if err == nil || !strings.Contains(err.Error(), "no agent named nobody is available to this node") {
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
		{[]string{"-peer", "--", "a", "b"}, []string{"a", "b"}, "--", false},
		{[]string{"a", "-peer=--", "--", "-b"}, []string{"a", "-b"}, "--", false},
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
		name := r.URL.Query().Get("name")
		if r.URL.Query().Get("type") != "a2a" || (name != "" && name != "helper") {
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

// A call that ends while the agent has not yet confirmed the task says the
// task may still run; an answer to a known task cancels it.
func TestTaskSendStopsWhileTheSendIsInFlight(t *testing.T) {
	net := newFakeNetwork(t)
	remote := randomPeer(t)
	net.offer(remote)
	var cancelled atomic.Value
	release := make(chan struct{})
	defer close(release)
	net.mu.Lock()
	net.routes[remote] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&call)
		switch call.Method {
		case "SendMessage":
			select {
			case <-release:
			case <-r.Context().Done():
			}
		case "CancelTask":
			cancelled.Store(string(call.Params))
			http.Error(w, "gone", http.StatusBadGateway)
		default:
			http.Error(w, "unexpected "+call.Method, http.StatusBadRequest)
		}
	})
	net.mu.Unlock()

	err := startTaskSend("", "-data-dir", net.dir, "-timeout", "300ms", "helper", "hi").wait(t)
	if err == nil || !strings.Contains(err.Error(), "no answer within 300ms before helper confirmed the task, which may still run") {
		t.Fatalf("new task: %v", err)
	}
	if cancelled.Load() != nil {
		t.Fatal("cancelled a task without its ID")
	}
	err = startTaskSend("", "-data-dir", net.dir, "-timeout", "300ms", "-task", "t-7", "helper", "21").wait(t)
	if err == nil || !strings.Contains(err.Error(), "task t-7: no answer within 300ms") {
		t.Fatalf("answer: %v", err)
	}
	if p, _ := cancelled.Load().(string); !strings.Contains(p, `"t-7"`) {
		t.Fatalf("cancel of the answered task: %q", p)
	}
}

// -wait-agent bounds the search, however short, with several lookups in it.
func TestTaskSendWaitsForTheAgentOnlyAsLongAsAsked(t *testing.T) {
	net := newFakeNetwork(t)
	start := time.Now()
	err := startTaskSend("", "-data-dir", net.dir, "-wait-agent", "1s", "helper", "hi").wait(t)
	if took := time.Since(start); err == nil || !strings.Contains(err.Error(), "no agent named helper is available to this node") || took > 1500*time.Millisecond {
		t.Fatalf("after %s: %v", took, err)
	}
	if n := net.lookups.Load(); n < 3 {
		t.Fatalf("%d lookups in 1s", n)
	}
}

func TestAnswerCommand(t *testing.T) {
	o := taskSendOptions{dataDir: defaultDataDir()}
	if got, want := answerCommand(o, taskTarget{Service: "helper"}, "a-1"), `agentiscaled task send -task a-1 helper -- "<your answer>"`; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	o.dataDir = "/tmp/my node"
	got := answerCommand(o, taskTarget{PeerID: "12D3Koo", Service: "helper"}, "a-1")
	if want := `agentiscaled task send -data-dir '/tmp/my node' -task a-1 -peer 12D3Koo helper -- "<your answer>"`; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// Only plain task IDs, such as the UUIDs agentiscaled agents use, are taken.
func TestTaskIDs(t *testing.T) {
	for _, id := range []string{"3f2b6c1e-9d4a-4c1b-8f0e-2a7d5b9c1e33", "a-1", "t.2:x_y"} {
		if !taskID.MatchString(id) {
			t.Errorf("%q refused", id)
		}
	}
	for _, id := range []string{"", "--", "-json", "a b", "$(x)", "t\x1b[2J", "a\x00b", "a\n", "it's", strings.Repeat("a", 129)} {
		if taskID.MatchString(id) {
			t.Errorf("%q taken", id)
		}
	}
	err := startTaskSend("", "-data-dir", t.TempDir(), "-task", "$(x)", "helper", "21").wait(t)
	if err == nil || !strings.Contains(err.Error(), "not a task ID") {
		t.Fatalf("-task $(x): %v", err)
	}
}

// The whole command, run by bash, reaches task send's parser as the task
// ID, the agent and the answer.
func TestAnswerCommandRoundTrips(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	cmd := answerCommand(taskSendOptions{dataDir: "/tmp/it's here"}, taskTarget{Service: "helper"}, "a-1")
	cmd = strings.Replace(cmd, `"<your answer>"`, `-42`, 1)
	out, err := exec.Command(bash, "-c", `agentiscaled() { printf '%s\0' "$@"; }; `+cmd).Output()
	if err != nil {
		t.Fatalf("bash ran %s: %v", cmd, err)
	}
	args := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	task, dir := fs.String("task", "", ""), fs.String("data-dir", "", "")
	pos, err := parseInterleaved(fs, args[2:])
	if err != nil || *task != "a-1" || *dir != "/tmp/it's here" || !slices.Equal(pos, []string{"helper", "-42"}) {
		t.Fatalf("task %q, dir %q, args %q, %v", *task, *dir, pos, err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestReportTheAnswer(t *testing.T) {
	task := taskOut{TaskID: "t", State: "completed", Terminal: "completed", Result: "\x1b[31mred\x1b[0m done\n"}
	var b strings.Builder
	if err := report(&b, taskSendOptions{}, task, nil); err != nil || b.String() != "\x1b[31mred\x1b[0m done\n" {
		t.Fatalf("to a pipe: %q %v", b.String(), err)
	}
	b.Reset()
	if err := report(&b, taskSendOptions{terminal: true}, task, nil); err != nil || b.String() != "red done\n" {
		t.Fatalf("to a terminal: %q %v", b.String(), err)
	}
	for _, o := range []taskSendOptions{{}, {json: true}} {
		if err := report(failingWriter{}, o, task, nil); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("json=%t: write error lost: %v", o.json, err)
		}
	}
	failed := errors.New("task t ended failed")
	if err := report(failingWriter{}, taskSendOptions{json: true}, task, failed); err != failed {
		t.Errorf("the task's own error should win: %v", err)
	}
}

// Ctrl-C ends a wait for a message on standard input that never closes.
func TestTaskSendStopsReadingStandardInputWhenInterrupted(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runTaskSend(ctx, []string{"-data-dir", t.TempDir(), "helper"}, r, io.Discard, io.Discard)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still reading standard input after Ctrl-C")
	}
}

// What the agent or network says in an error reaches the terminal without
// its control sequences.
func TestTaskSendErrorsAreTerminalSafe(t *testing.T) {
	_, fix, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), &meshnode.NodeConfigComplete{})
	fix.err = errors.New("boom\x1b]0;owned\x07\x1b[2J")
	err := startTaskSend("", "-data-dir", dir, "helper", "hi").wait(t)
	if err == nil || strings.ContainsRune(err.Error(), 0x1b) || strings.ContainsRune(err.Error(), 0x07) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %q", err)
	}
}
