package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

const testSender = "12D3KooWGPMbCHEiW61Swz3L4HP7LpdoeVqMMo7p3ycHKWFnybQS"

// fixture is a deterministic runtime in place of Codex: each run reports the request it got and waits on release, or
// returns err.
type fixture struct {
	prompts chan taskRequest
	release chan string
	err     error
}

func newFixture() *fixture {
	return &fixture{prompts: make(chan taskRequest, 4), release: make(chan string, 4)}
}

func (f *fixture) Describe(card *a2a.AgentCard) { card.Description = "fixture" }

func (f *fixture) Run(ctx context.Context, dir string, req taskRequest) (string, error) {
	f.prompts <- req
	if f.err != nil {
		return "", f.err
	}
	select {
	case out := <-f.release:
		return out, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// meshHeaders stands in for the node: it sends the backend credential and
// stamps the authenticated sender, as the mesh ingress does.
type meshHeaders struct {
	token, sender string
}

func (m meshHeaders) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+m.token)
	if m.sender != "" {
		r.Header.Set("X-Peer-Id", m.sender)
	}
	return http.DefaultTransport.RoundTrip(r)
}

type agentHarness struct {
	svc    *agentService
	fix    *fixture
	url    string
	client *a2aclient.Client
	dir    string
}

func newAgentHarness(t *testing.T, sender string) *agentHarness {
	t.Helper()
	return newAgentHarnessIn(t, sender, t.TempDir())
}

// newAgentHarnessIn starts an agent service whose directory and node data
// directory are dir, as a node started again on the same data would.
func newAgentHarnessIn(t *testing.T, sender, dir string) *agentHarness {
	t.Helper()
	fix := newFixture()
	svc, err := newAgentService(dir, fix, dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.handler("codex-review", "secret"))
	t.Cleanup(srv.Close)
	return &agentHarness{svc: svc, fix: fix, url: srv.URL, dir: dir, client: a2aClient(t, srv.URL, meshHeaders{"secret", sender})}
}

// as is a client of the same agent that the node delivers for sender.
func (h *agentHarness) as(t *testing.T, sender string) *a2aclient.Client {
	return a2aClient(t, h.url, meshHeaders{"secret", sender})
}

func a2aClient(t *testing.T, url string, rt http.RoundTripper) *a2aclient.Client {
	t.Helper()
	c, err := a2aclient.NewFromEndpoints(context.Background(),
		[]*a2a.AgentInterface{a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)},
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (h *agentHarness) send(t *testing.T, id, text string, meta map[string]any) *a2a.Task {
	t.Helper()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	if id != "" {
		msg.ID = id
	}
	msg.Metadata = meta
	res, err := h.client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("got %T, want a task", res)
	}
	return task
}

// await polls until the task reaches want.
func (h *agentHarness) await(t *testing.T, id a2a.TaskID, want a2a.TaskState) *a2a.Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
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
		time.Sleep(20 * time.Millisecond)
	}
}

func statusText(task *a2a.Task) string {
	if task.Status.Message == nil {
		return ""
	}
	return messageText(task.Status.Message)
}

func terminal(task *a2a.Task) string {
	s, _ := task.Metadata[metaTerminal].(string)
	return s
}

func TestTaskCompletes(t *testing.T) {
	h := newAgentHarness(t, testSender)
	task := h.send(t, "", "Review the uncommitted changes", nil)
	req := <-h.fix.prompts
	h.fix.release <- "main.go:3: unchecked error"
	done := h.await(t, task.ID, a2a.TaskStateCompleted)

	if terminal(done) != "completed" || describeTask(done).Result != "main.go:3: unchecked error" {
		t.Fatalf("result: %+v", describeTask(done))
	}
	// Provenance survives into the task and to the runtime.
	if done.Metadata[metaSender] != testSender || req != (taskRequest{Sender: testSender, Text: "Review the uncommitted changes"}) {
		t.Fatalf("sender or request lost: %v / %+v", done.Metadata, req)
	}
}

func TestRuntimeFailureIsATerminalState(t *testing.T) {
	h := newAgentHarness(t, testSender)
	h.fix.err = errors.New("codex exited 1")
	task := h.send(t, "", "review", nil)
	done := h.await(t, task.ID, a2a.TaskStateFailed)
	if terminal(done) != "failed" || !strings.Contains(statusText(done), "codex exited 1") {
		t.Fatalf("failed task: %s %q", terminal(done), statusText(done))
	}
}

func TestTaskTimesOut(t *testing.T) {
	h := newAgentHarness(t, testSender)
	h.svc.deadline = 100 * time.Millisecond
	task := h.send(t, "", "review", nil)
	done := h.await(t, task.ID, a2a.TaskStateFailed)
	if terminal(done) != "timeout" {
		t.Fatalf("terminal %q, want timeout (%s)", terminal(done), statusText(done))
	}
}

func TestCancelReportsWhetherTheTaskHadBegun(t *testing.T) {
	h := newAgentHarness(t, testSender)
	task := h.send(t, "", "review", nil)
	<-h.fix.prompts
	h.await(t, task.ID, a2a.TaskStateWorking)
	cancelled, err := h.client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status.State != a2a.TaskStateCanceled || cancelled.Metadata[metaExecutionBegun] != true {
		t.Fatalf("cancel: %s %v", cancelled.Status.State, cancelled.Metadata)
	}
	// The slot is free again once the run has stopped.
	waitIdle(t, h.svc)
	next := h.send(t, "", "review again", nil)
	<-h.fix.prompts
	h.fix.release <- "No findings"
	h.await(t, next.ID, a2a.TaskStateCompleted)
}

func TestOneTaskAtATime(t *testing.T) {
	h := newAgentHarness(t, testSender)
	first := h.send(t, "", "review", nil)
	<-h.fix.prompts
	second := h.send(t, "", "another review", nil)
	busy := h.await(t, second.ID, a2a.TaskStateRejected)
	if !strings.Contains(statusText(busy), "already running") {
		t.Fatalf("busy: %q", statusText(busy))
	}
	h.fix.release <- "No findings"
	h.await(t, first.ID, a2a.TaskStateCompleted)
}

func TestARetriedMessageIsNotRunTwice(t *testing.T) {
	h := newAgentHarness(t, testSender)
	first := h.send(t, "m-1", "review", nil)
	<-h.fix.prompts
	h.fix.release <- "No findings"
	h.await(t, first.ID, a2a.TaskStateCompleted)

	retry := h.send(t, "m-1", "review", nil)
	dup := h.await(t, retry.ID, a2a.TaskStateRejected)
	if dup.Metadata[metaDuplicateOf] != string(first.ID) {
		t.Fatalf("duplicate: %v", dup.Metadata)
	}
	select {
	case p := <-h.fix.prompts:
		t.Fatalf("the retry ran the task again: %+v", p)
	default:
	}
}

func TestRefusedRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sender string
		meta   map[string]any
		want   string
	}{
		{"delegated request", testSender, map[string]any{metaDelegationDepth: 1}, "delegation depth"},
		{"malformed depth", testSender, map[string]any{metaDelegationDepth: "0"}, "delegation depth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgentHarness(t, tc.sender)
			task := h.send(t, "", "review", tc.meta)
			done := h.await(t, task.ID, a2a.TaskStateRejected)
			if !strings.Contains(statusText(done), tc.want) {
				t.Fatalf("got %q, want %q", statusText(done), tc.want)
			}
			select {
			case p := <-h.fix.prompts:
				t.Fatalf("the task ran: %+v", p)
			default:
			}
		})
	}
}

// A request the node did not stamp with an authenticated sender did not come
// through the mesh, and is refused before it becomes a task.
func TestARequestWithoutASenderIsRefused(t *testing.T) {
	h := newAgentHarness(t, "")
	_, err := h.client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("review"))})
	if !errors.Is(err, a2a.ErrUnauthenticated) {
		t.Fatalf("send without a sender: %v", err)
	}
}

// Another peer of the network that learns a task ID can neither read the
// task's result nor cancel it.
func TestTasksBelongToTheirSender(t *testing.T) {
	const other = "12D3KooWRBy97UB99e3J6hiPesre1MZeuNQvfan4gBziswrRJsNK"
	h := newAgentHarness(t, testSender)
	task := h.send(t, "", "review", nil)
	<-h.fix.prompts
	intruder := h.as(t, other)
	if _, err := intruder.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("another peer got the task: %v", err)
	}
	if _, err := intruder.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("another peer cancelled the task: %v", err)
	}
	h.fix.release <- "No findings"
	done := h.await(t, task.ID, a2a.TaskStateCompleted)
	if _, err := intruder.GetTask(context.Background(), &a2a.GetTaskRequest{ID: done.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("another peer got the result: %v", err)
	}
}

// The record of delivered requests outlives the node: a retry of a request
// that ran before a restart is refused, not run again. Message
// IDs belong to their sender, so another peer's message with the same ID
// still runs.
func TestARetryAfterARestartIsNotRunTwice(t *testing.T) {
	const other = "12D3KooWRBy97UB99e3J6hiPesre1MZeuNQvfan4gBziswrRJsNK"
	dir := t.TempDir()
	h := newAgentHarnessIn(t, testSender, dir)
	first := h.send(t, "m-1", "review", nil)
	<-h.fix.prompts
	h.fix.release <- "No findings"
	h.await(t, first.ID, a2a.TaskStateCompleted)

	restarted := newAgentHarnessIn(t, testSender, dir)
	retry := restarted.send(t, "m-1", "review", nil)
	dup := restarted.await(t, retry.ID, a2a.TaskStateRejected)
	if dup.Metadata[metaDuplicateOf] != string(first.ID) {
		t.Fatalf("retry after restart: %v (%s)", dup.Metadata, statusText(dup))
	}
	select {
	case p := <-restarted.fix.prompts:
		t.Fatalf("the retry ran the task again: %+v", p)
	default:
	}

	otherPeer := restarted.as(t, other)
	res, err := otherPeer.SendMessage(context.Background(), &a2a.SendMessageRequest{
		Message: &a2a.Message{ID: "m-1", Role: a2a.MessageRoleUser, Parts: []*a2a.Part{a2a.NewTextPart("review")}},
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p := <-restarted.fix.prompts; p.Sender != other {
		t.Fatalf("ran for %s, want %s", p.Sender, other)
	}
	restarted.fix.release <- "No findings"
	_ = res
}

func TestOwnerStopCancelsAndRefuses(t *testing.T) {
	h := newAgentHarness(t, testSender)
	task := h.send(t, "", "review", nil)
	<-h.fix.prompts
	if err := runTasks([]string{"stop", "-data-dir", h.dir}, new(strings.Builder)); err != nil {
		t.Fatal(err)
	}
	stopped := h.await(t, task.ID, a2a.TaskStateCanceled)
	if !strings.Contains(statusText(stopped), "owner stopped") {
		t.Fatalf("stopped: %q", statusText(stopped))
	}
	waitIdle(t, h.svc)
	refused := h.await(t, h.send(t, "", "review", nil).ID, a2a.TaskStateRejected)
	if !strings.Contains(statusText(refused), "stopped task execution") {
		t.Fatalf("refused: %q", statusText(refused))
	}
	// A stopped agent is not ready: its card is withheld.
	if code := get(t, h.url+"/.well-known/agent-card.json", "secret"); code != http.StatusServiceUnavailable {
		t.Fatalf("card while stopped: %d", code)
	}

	if err := runTasks([]string{"resume", "-data-dir", h.dir}, new(strings.Builder)); err != nil {
		t.Fatal(err)
	}
	if code := get(t, h.url+"/.well-known/agent-card.json", "secret"); code != http.StatusOK {
		t.Fatalf("card after resume: %d", code)
	}
}

// Only the node, holding the backend credential, can reach the agent: another
// local process cannot call it directly and claim a sender.
func TestAgentNeedsTheNodeCredential(t *testing.T) {
	h := newAgentHarness(t, testSender)
	for _, token := range []string{"", "wrong"} {
		if code := get(t, h.url+"/.well-known/agent-card.json", token); code != http.StatusUnauthorized {
			t.Fatalf("card with token %q: %d", token, code)
		}
	}
	impostor := a2aClient(t, h.url, meshHeaders{"wrong", testSender})
	_, err := impostor.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("review"))})
	if err == nil {
		t.Fatal("a call without the node credential was accepted")
	}
}

func TestCodexRuntimeIsReadOnlyAndUnconfigured(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	codex := filepath.Join(dir, "codex")
	// Records its arguments and prompt, and answers through -o.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\necho \"$CODEX_HOME $HOME\" > " + argsFile + ".env\nwhile [ $# -gt 0 ]; do [ \"$1\" = --output-last-message ] && out=$2; shift; done\ncat > " + argsFile + ".prompt\necho 'No findings' > \"$out\"\n"
	if err := os.WriteFile(codex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := (&codexRuntime{command: codex, home: filepath.Join(dir, "agent-codex")}).Run(context.Background(), dir, taskRequest{Sender: testSender, Text: "Review main.go"})
	if err != nil || out != "No findings" {
		t.Fatalf("run: %q, %v", out, err)
	}
	b, _ := os.ReadFile(argsFile)
	args := " " + strings.ReplaceAll(string(b), "\n", " ")
	for _, want := range []string{" exec ", " --ignore-user-config ", ` approval_policy="never" `,
		// None of the owner's variables in commands, and reads confined to
		// dir.
		` shell_environment_policy.inherit="core" `,
		` default_permissions="agentiscale-review" `,
		` permissions.agentiscale-review={extends=":read-only", filesystem={":root"="deny", ":minimal"="read", ":workspace_roots"="read", "` + filepath.Join(dir, "agent-codex") + `"="deny"}} `,
		" -C " + dir + " "} {
		if !strings.Contains(args, want) {
			t.Errorf("codex arguments %q lack %q", args, want)
		}
	}
	// The run has the agent's own CODEX_HOME and HOME, not the owner's.
	if env, _ := os.ReadFile(argsFile + ".env"); string(env) != filepath.Join(dir, "agent-codex")+" "+filepath.Join(dir, "agent-codex", "home")+"\n" {
		t.Errorf("CODEX_HOME and HOME: %q", env)
	}
	// The request reaches Codex on stdin, framed as untrusted data with its
	// authenticated sender.
	p, _ := os.ReadFile(argsFile + ".prompt")
	for _, want := range []string{"must not modify", "from node " + testSender, "<request>\nReview main.go\n</request>"} {
		if !strings.Contains(string(p), want) {
			t.Errorf("prompt on stdin %q lacks %q", p, want)
		}
	}
}

func get(t *testing.T, url, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func waitIdle(t *testing.T, s *agentService) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		idle := s.current == nil
		s.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the review run did not end")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A failed run reports the lines that say why, not only its last line,
// which for Codex is the token count.
func TestFailureLinesKeepTheError(t *testing.T) {
	out := "OpenAI Codex v0.144.1\nworkdir: /repo\nERROR: stream disconnected\nthinking\ntokens used\n45,312\n"
	if got := failureLines(out, 3); got != "ERROR: stream disconnected" {
		t.Fatalf("got %q", got)
	}
	if got := failureLines("a\nb\nc\nd\n", 3); got != "b | c | d" {
		t.Fatalf("got %q", got)
	}
}

// The Codex install a run may read never widens to the home directory or a
// shared prefix, where remotely prompted commands could read credentials.
func TestCodexInstallDirStaysNarrow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	exe := func(parts ...string) string {
		p := filepath.Join(append([]string{home}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	for _, tc := range []struct{ path, want string }{
		// A binary in ~/bin: the bin directory, not the home it sits in.
		{exe("bin", "codex"), filepath.Join(home, "bin")},
		// A binary directly in the home directory: nothing.
		{exe("codex"), ""},
		// A release directory: that release.
		{exe(".codex", "packages", "standalone", "releases", "1.0", "codex"), filepath.Join(home, ".codex", "packages", "standalone", "releases", "1.0")},
		// A release with its own bin: the release.
		{exe("opt", "codex-1.0", "bin", "codex"), filepath.Join(home, "opt", "codex-1.0")},
		// npm: the node_modules directory with the native package.
		{exe(".npm-global", "lib", "node_modules", "@openai", "codex", "bin", "codex.js"), filepath.Join(home, ".npm-global", "lib", "node_modules")},
	} {
		want := tc.want
		if want != "" {
			want = real(want)
		}
		if got := codexInstallDir(tc.path); got != want {
			t.Errorf("codexInstallDir(%s) = %q, want %q", tc.path, got, want)
		}
	}
	for _, dir := range []string{"/", "/usr", "/usr/local", "/opt/homebrew", "/home", home, filepath.Dir(home)} {
		if !tooBroad(dir) {
			t.Errorf("tooBroad(%s) = false", dir)
		}
	}
}

// The agent directory and the node's data directory must not overlap: the
// agent's commands read the agent directory, and the data directory holds the
// node's key and the agent's sign-in.
func TestAgentDirKeepsApartFromTheDataDir(t *testing.T) {
	repo := t.TempDir()
	other := t.TempDir()
	link := filepath.Join(other, "repo-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		agentDir, dataDir string
		ok                bool
	}{
		{repo, other, true},
		{repo, filepath.Join(repo, ".agentiscale"), false},
		{repo, repo, false},
		{filepath.Join(other, "data", "src"), filepath.Join(other, "data"), false},
		{link, filepath.Join(repo, "state"), false},
		{repo, filepath.Join(other, "repo-sibling"), true},
	} {
		if err := agentDirApart(tc.agentDir, tc.dataDir); (err == nil) != tc.ok {
			t.Errorf("agentDirApart(%s, %s) = %v, want ok %v", tc.agentDir, tc.dataDir, err, tc.ok)
		}
	}
}

// A generated API token is readable by its owner only, even when an empty
// token file with looser permissions was there before.
func TestGeneratedAPITokenIsPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api-token")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	token, err := apiTokenFor(config{dataDir: dir})
	if err != nil || token == "" {
		t.Fatalf("apiTokenFor: %q, %v", token, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("api-token mode %v, want 0600", fi.Mode().Perm())
	}
	if again, _ := apiTokenFor(config{dataDir: dir}); again != token {
		t.Errorf("second start: token %q, want %q", again, token)
	}
}
