package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/agentmesh/api"
	"github.com/whiteredhouse/agentiscaled/internal/caller"
)

// fakeACP is where TestMain sets up the fake ACP agent's build, before
// any test changes the directory or the environment (TMPDIR, HOME) that
// go build would otherwise pick up.
var fakeACP struct {
	dir string   // the build's output directory, removed after the tests
	src string   // the package directory, where go build runs
	env []string // the environment go build runs with
}

// setUpFakeACP makes the fake ACP agent's output directory and notes the
// package directory and environment its build will use.
func setUpFakeACP() (cleanup func(), err error) {
	src, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "fake-acp")
	if err != nil {
		return nil, err
	}
	fakeACP.dir, fakeACP.src, fakeACP.env = dir, src, os.Environ()
	return func() { os.RemoveAll(dir) }, nil
}

// fakeACPBinary is testdata/fakeacp, built once for the package's tests.
// It is its own small program, not this test binary run again: under
// -race the test binary takes over a second just to start, and the acp
// tests start an agent for nearly every turn.
var fakeACPBinary = sync.OnceValues(func() (string, error) {
	if fakeACP.dir == "" {
		return "", errors.New("TestMain did not set up the fake ACP agent")
	}
	if err := readFakeACPSource(); err != nil {
		return "", err
	}
	bin := filepath.Join(fakeACP.dir, "fake-acp-agent")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/fakeacp")
	cmd.Dir, cmd.Env = fakeACP.src, fakeACP.env
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build the fake ACP agent: %v\n%s", err, out)
	}
	return bin, nil
})

// readFakeACPSource reads the fake ACP agent's source from this test
// process. Only go build reads it otherwise, and go test caches a result
// against the files the test process itself opens, so without this an
// edit to the fake would leave a cached pass in place. (Its one module
// import, internal/checkprompt, this package imports too.)
func readFakeACPSource() error {
	dir := filepath.Join(fakeACP.src, "testdata", "fakeacp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := os.ReadFile(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// fakeACPCommand is a command line that runs the fake ACP agent with
// flags, through a script whose path the tests can link or rename.
func fakeACPCommand(t *testing.T, flags ...string) string {
	t.Helper()
	agent, err := fakeACPBinary()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := filepath.Join(bin, "fake-acp")
	body := fmt.Sprintf("#!/bin/sh\nexec '%s' \"$@\"\n", agent)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return strings.Join(append([]string{script}, flags...), " ")
}

// newACPFromLine makes the acp runtime from a -command line.
func newACPFromLine(ctx context.Context, line, dataDir string, policy agentPolicy) (runtime, error) {
	program, args, err := splitACPCommand(line)
	if err != nil {
		return nil, err
	}
	a := agentConfig{Runtime: "acp", Name: runtimeDefaultName("acp", program), Command: program, Args: args, Dir: dataDir}
	a.setPolicy(policy)
	return a.newRuntime(ctx, dataDir)
}

func newFakeACP(t *testing.T, policy agentPolicy, flags ...string) *acpRuntime {
	t.Helper()
	rt, err := newACPFromLine(t.Context(), fakeACPCommand(t, flags...), t.TempDir(), policy)
	if err != nil {
		t.Fatal(err)
	}
	return rt.(*acpRuntime)
}

var ownerPolicy = agentPolicy{DirInstructions: true, DirSkills: true, OwnerConfig: true}

func TestACPRuntimeTurn(t *testing.T) {
	for _, status := range []bool{true, false} {
		t.Run(fmt.Sprintf("status=%v", status), func(t *testing.T) {
			flags := []string{"-http", "-load"}
			if status {
				flags = append(flags, "-status")
			}
			r := newFakeACP(t, ownerPolicy, flags...)
			r.setDefinition(&agentDefinition{Instructions: "You review code."})
			var steps []string
			res, err := r.Run(context.Background(), t.TempDir(), taskRequest{
				Sender: "peer", Text: "do it",
				Progress: func(s string) { steps = append(steps, s) },
				Network:  &networkAccess{URL: "http://127.0.0.1:1/mcp", Token: "tok"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Question || res.Session != "sess-1" {
				t.Fatalf("result %+v", res)
			}
			for _, want := range []string{
				"env=AGENTISCALE_CONTRACT=1,AGENTISCALE_ENV=1 ",
				"mcp=agentiscale-network@http://127.0.0.1:1/mcp@Authorization:Bearer tok ",
				"perm=selected:no ",
				fmt.Sprintf("json=%v ", !status),
				"role=true",
				"answer=false",
			} {
				if !strings.Contains(res.Text, want) {
					t.Errorf("result %q lacks %q", res.Text, want)
				}
			}
			// Steps are fixed summaries, never the agent's titles,
			// commands or plan text.
			if want := []string{"Reading main.go", "Running a command", "Working on step 2 of 2"}; !slices.Equal(steps, want) {
				t.Errorf("steps %q, want %q", steps, want)
			}
		})
	}
}

func TestACPRuntimeNoNetworkWithoutHTTP(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status")
	res, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x", Network: &networkAccess{URL: "http://127.0.0.1:1/mcp", Token: "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "mcp= ") {
		t.Errorf("an agent without mcpCapabilities.http got the network: %q", res.Text)
	}
}

func TestACPRuntimeQuestionAndAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flags   []string
		release bool // the kept process is gone when the answer comes
		prompts string
	}{
		{"kept", []string{"-load", "-status"}, false, "prompts=2 "},
		{"kept json", []string{"-load"}, false, "prompts=2 "},
		{"kept without loadSession", []string{"-status"}, false, "prompts=2 "},
		{"loaded", []string{"-load", "-status"}, true, "prompts=1 "},
		{"loaded json", []string{"-load"}, true, "prompts=1 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeACP(t, ownerPolicy, tc.flags...)
			r.setDefinition(&agentDefinition{Instructions: "role"})
			dir := t.TempDir()
			q, err := r.Run(context.Background(), dir, taskRequest{Text: "ASK-ME", Task: "task-1"})
			if err != nil {
				t.Fatal(err)
			}
			if !q.Question || q.Text != "Which color?" || q.Session != "sess-1" {
				t.Fatalf("question %+v", q)
			}
			if tc.release {
				r.release("task-1")
			}
			var steps []string
			a, err := r.Run(context.Background(), dir, taskRequest{Text: "blue", Session: q.Session, Task: "task-1", Progress: func(s string) { steps = append(steps, s) }})
			if err != nil {
				t.Fatal(err)
			}
			// The replayed history is not the turn's: no OLD QUESTION in
			// the result, no step of the replayed tool call.
			if a.Question || strings.Contains(a.Text, "OLD QUESTION") || !strings.Contains(a.Text, "answer=true") || !strings.Contains(a.Text, "role=false") || !strings.Contains(a.Text, tc.prompts) {
				t.Errorf("answer %+v", a)
			}
			if slices.Contains(steps, "Reading replayed.go") || !slices.Contains(steps, "Reading main.go") {
				t.Errorf("steps %q", steps)
			}
			if len(r.kept) != 0 {
				t.Errorf("processes still kept: %v", r.kept)
			}
		})
	}
}

// Once the service admits an answer, a slow token mint must not expire
// the process that can take it. This agent cannot reload the session.
func TestACPAdmittedAnswerKeepsProcess(t *testing.T) {
	const wait = time.Second
	h := newAgentHarness(t, testSender)
	h.svc.answerWait = wait
	rt := newFakeACP(t, ownerPolicy, "-status", "-http")
	t.Cleanup(rt.releaseAll)
	h.svc.rt = rt
	network, _ := fakeNodeNetworkServer(t)
	var mints atomic.Int32
	network.node = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/oauth/token" && mints.Add(1) == 2 {
			// The second turn owns the running slot before minting its token.
			time.Sleep(wait + 100*time.Millisecond)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"access_token":"test-token"}`))}, nil
	})}
	h.svc.network = network

	task := h.send(t, "", "ASK-ME", nil)
	h.await(t, task.ID, a2a.TaskStateInputRequired)
	h.answer(t, task.ID, "", "blue")
	done := h.await(t, task.ID, a2a.TaskStateCompleted)
	if got := mints.Load(); got != 2 {
		t.Fatalf("minted tokens for %d turns, want 2", got)
	}
	if result := caller.DescribeTask(done).Result; !strings.Contains(result, "prompts=2 ") {
		t.Errorf("expected the process that asked to take its second prompt, got %q", result)
	}
}

func TestACPRuntimeAnswerAfterProcessEnded(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status")
	q, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "ASK-ME", Task: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	r.release("task-1")
	if _, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x", Session: q.Session, Task: "task-1"}); err == nil || !strings.Contains(err.Error(), "cannot load a session") {
		t.Errorf("an answer whose process ended, to an agent without loadSession: %v", err)
	}
}

// Two processes may name their sessions alike: each answer goes to the
// process of its own task.
func TestACPRuntimeKeepsProcessesByTask(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-load", "-status")
	t.Cleanup(r.releaseAll)
	dir := t.TempDir()
	for _, task := range []string{"task-1", "task-2"} {
		q, err := r.Run(context.Background(), dir, taskRequest{Text: "ASK-ME", Task: task})
		if err != nil || !q.Question || q.Session != "sess-1" {
			t.Fatalf("%s: %+v %v", task, q, err)
		}
	}
	first := r.kept["task-1"].s
	a, err := r.Run(context.Background(), dir, taskRequest{Text: "blue", Session: "sess-1", Task: "task-2"})
	if err != nil || !strings.Contains(a.Text, "prompts=2 ") {
		t.Fatalf("answer: %+v %v", a, err)
	}
	if len(r.kept) != 1 || r.kept["task-1"] == nil || r.kept["task-1"].s != first {
		t.Errorf("task-2's answer took another process: %v", r.kept)
	}
	// A kept process takes only the answer to its own session.
	if s := r.take("task-1", "sess-other"); s != nil {
		t.Error("took a process for another session")
	}
}

// A process kept for an answer keeps what it declared at its initialize,
// also when the agent has declared otherwise since.
func TestACPRuntimeKeptProcessKeepsItsDeclarations(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status")
	t.Cleanup(r.releaseAll)
	if _, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "ASK-ME", Task: "task-1"}); err != nil {
		t.Fatal(err)
	}
	r.info.Meta.Agentiscale = nil
	a, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "blue", Session: "sess-1", Task: "task-1"})
	if err != nil || !strings.Contains(a.Text, "json=false") {
		t.Errorf("the answer to a process that declared status: %+v %v", a, err)
	}
}

// Every caller of releaseAll returns only once the kept processes have
// stopped, and none is kept from then on.
func TestACPRuntimeReleaseAllJoins(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status")
	kept := startFake(t, r)
	if !r.keep("t", "s", kept) {
		t.Fatal("keep")
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.releaseAll()
			if kept.cmd.ProcessState == nil {
				t.Error("releaseAll returned while a kept process ran")
			}
		}()
	}
	wg.Wait()
	// A process released on its own just before the node stops is waited
	// for too.
	r2 := newFakeACP(t, ownerPolicy, "-status")
	alone := startFake(t, r2)
	r2.keep("t", "s", alone)
	go r2.release("t")
	for deadline := time.Now().Add(10 * time.Second); ; {
		r2.sessionsMu.Lock()
		n := len(r2.kept)
		r2.sessionsMu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("release did not start")
		}
		time.Sleep(time.Millisecond)
	}
	r2.releaseAll()
	if alone.cmd.ProcessState == nil {
		t.Error("releaseAll returned while a process released on its own ran")
	}
	if extra, err := r.start(t.Context(), t.TempDir(), nil); err == nil {
		extra.close()
		t.Fatal("started an agent after shutdown")
	}
}

func TestACPRuntimeKeepsAtMostMaxWaiting(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status")
	t.Cleanup(r.releaseAll)
	for i := range maxWaiting {
		if !r.keep(fmt.Sprintf("t%d", i), "s", startFake(t, r)) {
			t.Fatalf("keep %d", i)
		}
	}
	extra := startFake(t, r)
	defer extra.close()
	if r.keep("one-more", "s", extra) {
		t.Error("kept more than maxWaiting processes")
	}
	// Without loadSession, a question past the bound fails the task.
	if _, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "ASK-ME", Task: "past"}); err == nil || !strings.Contains(err.Error(), "already wait for answers") {
		t.Errorf("question past the bound: %v", err)
	}
	kept := r.kept["t0"].s
	r.releaseAll()
	if len(r.kept) != 0 {
		t.Errorf("releaseAll left %d", len(r.kept))
	}
	select {
	case <-kept.done:
	case <-time.After(10 * time.Second):
		t.Error("a released process still runs")
	}
}

// startFake starts r's agent as a turn would.
func startFake(t *testing.T, r *acpRuntime) *acpSession {
	t.Helper()
	s, err := r.start(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestACPRuntimePolicies(t *testing.T) {
	data := t.TempDir()
	restricted := agentPolicy{}
	if _, err := newACPFromLine(t.Context(), fakeACPCommand(t, "-policies", "config-dir,dir-instructions"), data, restricted); err == nil || !strings.Contains(err.Error(), "-dir-skills=false") || strings.Contains(err.Error(), "-owner-config") {
		t.Errorf("an undeclared policy: %v", err)
	}
	if _, err := newACPFromLine(t.Context(), fakeACPCommand(t, "-no-meta"), data, agentPolicy{DirSkills: true, OwnerConfig: true}); err == nil || !strings.Contains(err.Error(), "-dir-instructions=false") {
		t.Errorf("a plain ACP agent with a policy: %v", err)
	}
	rt, err := newACPFromLine(t.Context(), fakeACPCommand(t, "-policies", "config-dir,dir-instructions,dir-skills", "-status"), data, restricted)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "owner-secret")
	res, err := rt.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(data, "agents", "fake-acp", "profile")
	want := "env=AGENTISCALE_CONFIG_DIR=" + config + ",AGENTISCALE_CONTRACT=1,AGENTISCALE_DIR_INSTRUCTIONS=0,AGENTISCALE_DIR_SKILLS=0,AGENTISCALE_ENV=1 "
	if !strings.Contains(res.Text, want) {
		t.Errorf("result %q lacks %q", res.Text, want)
	}
	if fi, err := os.Stat(config); err != nil || !fi.IsDir() {
		t.Errorf("config dir: %v", err)
	}
}

func TestACPProbeFollowsCaller(t *testing.T) {
	for _, entry := range []string{"node startup", "agent add", "runtime check"} {
		t.Run(entry, func(t *testing.T) {
			program := filepath.Join(t.TempDir(), "agent")
			script := "#!/bin/sh\nread -r request\nprintf '%s' \"$request\" > \"$0.started\"\ncat >/dev/null\n: > \"$0.closed\"\n"
			if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			a := agentConfig{Runtime: "acp", Name: "agent", Command: program, Dir: t.TempDir(), OwnConfig: true}
			c := config{dataDir: t.TempDir(), agents: []agentConfig{a}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				switch entry {
				case "node startup":
					_, err := newAgents(ctx, &c)
					done <- err
				case "agent add":
					done <- addAgent(ctx, &c, a, false)
				case "runtime check":
					done <- runRuntimeCheck(ctx, []string{"-command", program, "-dir", a.Dir, "-data-dir", c.dataDir}, io.Discard)
				}
			}()
			waitFor(t, func() bool {
				b, _ := os.ReadFile(program + ".started")
				return strings.Contains(string(b), `"method":"initialize"`)
			})
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("probe returned %v", err)
				}
			case <-time.After(acpCancelGrace + 2*time.Second):
				t.Fatal("shutdown did not interrupt the initialization probe")
			}
			if _, err := os.Stat(program + ".closed"); err != nil {
				t.Fatalf("probe returned before its process shut down: %v", err)
			}
		})
	}
}

func TestACPRuntimeRefusesNewerContract(t *testing.T) {
	if _, err := newACPFromLine(t.Context(), fakeACPCommand(t, "-contract", "2"), t.TempDir(), ownerPolicy); err == nil || !strings.Contains(err.Error(), "update agentiscaled") {
		t.Errorf("newer contract: %v", err)
	}
}

func TestACPRuntimeNotSignedIn(t *testing.T) {
	var logged tailBuffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	h := newAgentHarness(t, testSender)
	policy := ownerPolicy
	policy.OwnerConfig = false
	r := newFakeACP(t, policy, "-auth-required", "-policies", "config-dir")
	t.Cleanup(r.releaseAll)
	h.svc.rt = r
	task := h.await(t, h.send(t, "", "work", nil).ID, a2a.TaskStateFailed)
	if got := statusText(task); got != "task failed: the agent requires the node owner to sign in" {
		t.Errorf("peer-visible failure: %q", got)
	}
	want := "AGENTISCALE_CONFIG_DIR=" + shellQuote(r.config) + " '/opt/My Agent/fake-acp' login --account 'Jo'\\''s team'"
	if !strings.Contains(logged.String(), want) {
		t.Errorf("missing owner sign-in remedy in %q", logged.String())
	}
}

func TestACPSignInUsesFailedSession(t *testing.T) {
	r := &acpRuntime{command: "/opt/agent"}
	r.info.Meta.Agentiscale = &acpConventions{Login: []string{"other-version", "login"}}
	s := &acpSession{}
	s.info.Meta.Agentiscale = &acpConventions{Login: []string{"this-version", "login"}}
	failure := r.failure(t.Context(), s, &acpError{Method: "session/prompt", Code: acpAuthRequired})
	var signIn *acpSignInRequired
	if !errors.As(failure, &signIn) || !strings.HasSuffix(signIn.ownerMessage(), "\n  this-version login") {
		t.Fatalf("sign-in remedy for the process that failed: %+v", signIn)
	}
	// A process failing before initialize completes has supplied no command.
	r.config = "/opt/Agent Config"
	failure = r.failure(t.Context(), &acpSession{}, &acpError{Method: "initialize", Code: acpAuthRequired})
	if !errors.As(failure, &signIn) || signIn.ownerMessage() != "sign in on this node using the agent's sign-in flow with AGENTISCALE_CONFIG_DIR='/opt/Agent Config'" {
		t.Fatalf("sign-in remedy before initialize: %+v", signIn)
	}
}

func TestRuntimeCheckReportsLocalSignIn(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "check profiles")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	var out bytes.Buffer
	err := runRuntimeCheck(t.Context(), []string{
		"-command", fakeACPCommand(t, "-auth-required", "-policies", "config-dir"),
		"-owner-config=false", "-dir", t.TempDir(),
	}, &out)
	dirs, globErr := filepath.Glob(filepath.Join(tmp, "agentiscale-check-data-*"))
	if globErr != nil || len(dirs) != 1 {
		t.Fatalf("could not find the check's reusable data directory: %v, %v", dirs, globErr)
	}
	if err == nil || !strings.Contains(out.String(), "AGENTISCALE_CONFIG_DIR='"+dirs[0]+"/") ||
		!strings.Contains(out.String(), "retry this check with -data-dir "+shellQuote(dirs[0])) ||
		!strings.Contains(out.String(), "'/opt/My Agent/fake-acp' login --account 'Jo'\\''s team'") {
		t.Fatalf("runtime check: %v\n%s", err, &out)
	}
}

func TestACPRuntimeCancel(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignore=%v", ignore), func(t *testing.T) {
			flags := []string{"-status"}
			if ignore {
				flags = append(flags, "-ignore-cancel")
			}
			r := newFakeACP(t, ownerPolicy, flags...)
			var stop string
			r.stopped = func(s string) { stop = s }
			// The turn is cancelled once it runs: at its first step.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var start time.Time
			var once sync.Once
			_, err := r.Run(ctx, t.TempDir(), taskRequest{Text: "SLOW", Progress: func(string) {
				once.Do(func() { start = time.Now(); cancel() })
			}})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled turn: %v", err)
			}
			took := time.Since(start)
			if ignore {
				if stop != "" || took < acpCancelGrace {
					t.Errorf("an agent that ignores cancel: stop %q after %s", stop, took)
				}
			} else if stop != "cancelled" || took > 3*time.Second {
				t.Errorf("stop %q after %s", stop, took)
			}
		})
	}
}

func TestRuntimeCheck(t *testing.T) {
	old := checkCancelAfter
	checkCancelAfter = 2 * time.Second
	t.Cleanup(func() { checkCancelAfter = old })
	for _, tc := range []struct {
		flags []string
		load  string
	}{
		// Without loadSession the answer goes only to the process that asked.
		{[]string{"-status", "-http", "-policies", "config-dir"}, "SKIP  load"},
		{[]string{"-status", "-http", "-load"}, "PASS  load: session/load resumed"},
	} {
		var out bytes.Buffer
		err := runRuntimeCheck(context.Background(), []string{"-command", fakeACPCommand(t, tc.flags...)}, &out)
		if err != nil {
			t.Fatalf("check %v: %v\n%s", tc.flags, err, out.String())
		}
		for _, want := range []string{"PASS  initialize", "PASS  conventions", "INFO  network: advertises", "PASS  turn", "PASS  question", "PASS  answer: answered in the process that asked", tc.load, "PASS  cancel"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%v: output lacks %q:\n%s", tc.flags, want, out.String())
			}
		}
	}
}

func TestACPCommandLine(t *testing.T) {
	program, args, err := splitACPCommand("  my-agent  acp --x ")
	if err != nil || program != "my-agent" || !slices.Equal(args, []string{"acp", "--x"}) {
		t.Errorf("split: %q %q %v", program, args, err)
	}
	if _, _, err := splitACPCommand(" "); err == nil {
		t.Error("an empty command should not split")
	}
	if got := runtimeDefaultName("acp", "/opt/My Apps/My-Agent"); got != "my-agent" {
		t.Errorf("default name %q", got)
	}
	if got := runtimeDefaultName("codex", "/x/y"); got != "codex" {
		t.Errorf("codex default name %q", got)
	}
	// The program's path may hold spaces once it is resolved: it stays
	// apart from the arguments.
	line := fakeACPCommand(t, "-status")
	script, flags, _ := splitACPCommand(line)
	dir := filepath.Join(t.TempDir(), "my apps")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(script, filepath.Join(dir, "fake-acp")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	a := agentConfig{Runtime: "acp", Command: "fake-acp " + strings.Join(flags, " ")}
	if err := a.splitCommand(); err != nil {
		t.Fatal(err)
	}
	if err := checkAgent(t.Context(), &a, t.TempDir(), false); err == nil {
		t.Error("checkAgent without a directory should fail")
	}
	a.Dir = t.TempDir()
	if err := checkAgent(t.Context(), &a, t.TempDir(), false); err != nil {
		t.Fatalf("checkAgent: %v", err)
	}
	if a.Command != filepath.Join(dir, "fake-acp") || !slices.Equal(a.Args, flags) || a.Name != "fake-acp" {
		t.Errorf("checked agent: %q %q %q", a.Command, a.Args, a.Name)
	}
	if _, err := a.newRuntime(t.Context(), t.TempDir()); err != nil {
		t.Errorf("runtime from a path with a space: %v", err)
	}
}

func TestACPRuntimeRejectsOnlyOnce(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status", "-reject-always-only")
	res, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "perm=cancelled: ") {
		t.Errorf("with only reject_always the request should be cancelled: %q", res.Text)
	}
}

func TestACPRuntimeResultOverLimit(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status", "-big")
	if _, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"}); err == nil || !strings.Contains(err.Error(), "longer than the limit") {
		t.Errorf("a result over the limit: %v", err)
	}
}

func TestACPRuntimeFailureKeepsDiagnosticsLocal(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	r := newFakeACP(t, ownerPolicy, "-status", "-fail")
	_, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"})
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "session/prompt answered with error -32603") {
		t.Errorf("the caller's error: %v", err)
	}
	if !strings.Contains(logged.String(), "SECRET-TOKEN") {
		t.Errorf("the node's log lacks the agent's diagnostics: %q", logged.String())
	}
}

func TestACPRuntimeCancelWhenAgentStopsReading(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status", "-stop-reading")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		// A prompt bigger than a pipe holds, which the agent never reads.
		_, err := r.Run(ctx, t.TempDir(), taskRequest{Text: strings.Repeat("x", 1<<20)})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("turn: %v", err)
		}
	case <-time.After(time.Second + acpCancelGrace + 10*time.Second):
		t.Fatalf("the turn did not end %s after it was cancelled", time.Since(start))
	}
}

func TestRuntimeCheckTimesOut(t *testing.T) {
	old := checkCancelAfter
	checkCancelAfter = 200 * time.Millisecond
	t.Cleanup(func() { checkCancelAfter = old })
	var out bytes.Buffer
	err := runRuntimeCheck(context.Background(), []string{"-timeout", "1s", "-command", fakeACPCommand(t, "-status", "-hang")}, &out)
	if err == nil || !strings.Contains(out.String(), "FAIL  turn: no answer within 1s (-timeout)") {
		t.Errorf("check of an agent that never answers: %v\n%s", err, out.String())
	}
}

func TestACPRuntimeStdinClosed(t *testing.T) {
	r := newFakeACP(t, ownerPolicy, "-status", "-close-stdin")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, t.TempDir(), taskRequest{Text: strings.Repeat("x", 1<<20)})
	if err == nil || ctx.Err() != nil || time.Since(start) > 10*time.Second {
		t.Errorf("a turn whose agent closed stdin: %v after %s", err, time.Since(start))
	}
}

func TestACPRuntimeFixedTurnReasons(t *testing.T) {
	for flag, want := range map[string]string{
		"-bad-status": "not done or needs_input",
		"-bad-stop":   "the agent stopped the turn: an unknown reason",
	} {
		r := newFakeACP(t, ownerPolicy, "-status", flag)
		_, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"})
		if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", flag, err)
		}
	}
}

func TestACPRuntimeRelativeCommand(t *testing.T) {
	script, flags, _ := splitACPCommand(fakeACPCommand(t, "-status"))
	t.Chdir(filepath.Dir(script))
	a := agentConfig{Runtime: "acp", Name: "script", Command: "./" + filepath.Base(script), Args: flags, Dir: t.TempDir()}
	rt, err := a.newRuntime(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if r := rt.(*acpRuntime); r.command != script {
		t.Errorf("command %q, want %q", r.command, script)
	}
	if _, err := rt.Run(context.Background(), t.TempDir(), taskRequest{Text: "x"}); err != nil {
		t.Errorf("run from another directory: %v", err)
	}
}

func TestACPRuntimeCancelsPendingPermissions(t *testing.T) {
	// The agent asks for a permission after the cancel and then ignores
	// the cancel, so the node stops it.
	r := newFakeACP(t, ownerPolicy, "-status", "-perm-after-cancel", "-ignore-cancel")
	var stderr string
	r.stderr = func(s string) { stderr = s }
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	r.Run(ctx, t.TempDir(), taskRequest{Text: "SLOW", Progress: func(string) { once.Do(cancel) }})
	if !strings.Contains(stderr, `perm-after-cancel={"outcome":{"outcome":"cancelled"}}`) {
		t.Errorf("a permission asked after the cancel: %q", stderr)
	}
}

func TestACPSendDropsCancelledWrites(t *testing.T) {
	var written bytes.Buffer
	var mu sync.Mutex
	s := &acpSession{out: make(chan acpWrite), dead: make(chan struct{}), broken: make(chan struct{}), done: make(chan struct{}),
		stdin: nopCloser{writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return written.Write(p) })}}
	go s.write()
	defer close(s.done)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 64 {
		if err := s.send(ctx, map[string]string{"method": "session/prompt"}, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("send with a cancelled context: %v", err)
		}
	}
	if err := s.send(context.Background(), map[string]string{"method": "ok"}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if got := written.String(); got != "{\"method\":\"ok\"}\n" {
		t.Errorf("written %q", got)
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func TestRuntimeCheckRelativeDir(t *testing.T) {
	old := checkCancelAfter
	checkCancelAfter = 2 * time.Second
	t.Cleanup(func() { checkCancelAfter = old })
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	if err := runRuntimeCheck(context.Background(), []string{"-dir", ".", "-command", fakeACPCommand(t, "-status")}, &out); err != nil {
		t.Errorf("check with -dir .: %v\n%s", err, out.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// keepingFixture is a fixture that keeps something for each waiting task,
// as the acp runtime keeps the agent's process.
type keepingFixture struct {
	*fixture
	released chan string
	all      chan struct{}
}

func (k keepingFixture) release(task string) { k.released <- task }
func (k keepingFixture) releaseAll()         { close(k.all) }

// Run asks its question only once the turn is cancelled for a task named
// late, as when cancellation comes while the turn ends.
func (k keepingFixture) Run(ctx context.Context, dir string, req taskRequest) (turnResult, error) {
	if req.Text != "late" {
		return k.fixture.Run(ctx, dir, req)
	}
	k.prompts <- req
	req.progress("Thinking")
	<-ctx.Done()
	return turnResult{Text: "Too late?", Question: true, Session: "session-late"}, nil
}

// A question the caller never gets, because its input_required event or
// a progress event before it cannot be published, ends the task: it does
// not wait, and its runtime lets go of what it keeps for it.
func TestUnpublishedQuestionEndsTheTask(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		refuse     func(a2a.Event) bool
	}{
		{"question event", "ask", func(e a2a.Event) bool {
			ev, ok := e.(*a2a.TaskStatusUpdateEvent)
			return ok && ev.Status.State == a2a.TaskStateInputRequired
		}},
		{"progress event", "late", func(e a2a.Event) bool {
			ev, ok := e.(*a2a.TaskStatusUpdateEvent)
			return ok && ev.Metadata[metaProgress] == true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fix := keepingFixture{newFixture(), make(chan string, 4), make(chan struct{})}
			dir := t.TempDir()
			svc, err := newAgentService(dir, fix, dir, "helper")
			if err != nil {
				t.Fatal(err)
			}
			execCtx := &a2asrv.ExecutorContext{
				Message:       a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(tc.text)),
				TaskID:        a2a.TaskID("task-" + tc.text),
				ContextID:     "ctx",
				ServiceParams: a2asrv.NewServiceParams(map[string][]string{api.HeaderPeerID: {testSender}}),
			}
			go func() {
				<-fix.prompts
				fix.ask <- "Which one?"
			}()
			for ev := range svc.Execute(context.Background(), execCtx) {
				if tc.refuse(ev) {
					break
				}
			}
			select {
			case got := <-fix.released:
				if got != string(execCtx.TaskID) {
					t.Errorf("released %q", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the runtime kept what it holds for the task")
			}
			svc.mu.Lock()
			defer svc.mu.Unlock()
			if len(svc.waiting) != 0 {
				t.Errorf("the task still waits: %v", svc.waiting)
			}
		})
	}
}

// A waiting task that ends without an answer lets go of what its runtime
// keeps for it, and the node's stop of all of them.
func TestWaitingTaskReleasesWhatTheRuntimeKeeps(t *testing.T) {
	fix := keepingFixture{newFixture(), make(chan string, 4), make(chan struct{})}
	dir := t.TempDir()
	svc, err := newAgentService(dir, fix, dir, "helper")
	if err != nil {
		t.Fatal(err)
	}
	svc.answerWait = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target, stopped, err := serveAgent(ctx, svc, "helper")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(target)
	token, _ := u.User.Password()
	u.User = nil
	h := &agentHarness{svc: svc, fix: fix.fixture, url: u.String(), dir: dir, client: a2aClient(t, u.String(), meshHeaders{token, testSender})}

	task := h.send(t, "", "ask", nil)
	<-fix.prompts
	fix.ask <- "Which one?"
	h.await(t, task.ID, a2a.TaskStateInputRequired)
	if _, err := h.client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID}); err != nil {
		t.Fatal(err)
	}
	released := func(what string, id a2a.TaskID) {
		t.Helper()
		select {
		case s := <-fix.released:
			if s != string(id) {
				t.Errorf("%s released %q, not %q", what, s, id)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s released nothing", what)
		}
	}
	released("cancelling a waiting task", task.ID)
	// A question the cancellation discards before the task waits.
	late := h.send(t, "", "late", nil)
	<-fix.prompts
	if _, err := h.client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: late.ID}); err != nil {
		t.Fatal(err)
	}
	released("cancelling a turn that then asked", late.ID)
	// A question nobody answers in time ends its task by itself.
	lapsed := h.send(t, "", "lapse", nil)
	<-fix.prompts
	fix.ask <- "Anyone?"
	h.await(t, lapsed.ID, a2a.TaskStateInputRequired)
	released("a question nobody answered", lapsed.ID)
	// The caller sees the timeout, not a question nobody can answer.
	ended := h.await(t, lapsed.ID, a2a.TaskStateFailed)
	if got := statusText(ended); !strings.Contains(got, "timed out: no answer came") {
		t.Errorf("lapsed task: %s", got)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent service did not finish shutting down")
	}
	select {
	case <-fix.all:
	default:
		t.Fatal("the service stopped before its runtime")
	}
}
