package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/agentmesh/api"
)

// The bounds on hosted agents' tasks.
const (
	taskDeadline = 15 * time.Minute
	// A request that was itself delegated is refused: runtimes run with no
	// agentiscale tools, so nothing they start can delegate further.
	maxDelegationDepth = 0
)

// Metadata keys agentiscale reads and writes on A2A messages and tasks.
const (
	metaDelegationDepth = "agentiscale.delegation_depth"
	// metaTerminal names why a task ended, since A2A 1.0 has no timed-out
	// state: completed, rejected, cancelled, timeout or failed.
	metaTerminal = "agentiscale.terminal"
	// metaExecutionBegun says whether the runtime had started when a task
	// was cancelled.
	metaExecutionBegun = "agentiscale.execution_begun"
	metaSender         = "agentiscale.sender_peer_id"
	metaDuplicateOf    = "agentiscale.duplicate_of"
)

// runtime runs the tasks of a hosted agent with one agent CLI. The agent
// service around it keeps the A2A contract and the bounds, so supporting
// another agent CLI means adding a runtime. Codex is the only one today.
type runtime interface {
	// Describe says on the agent card what the agent does.
	Describe(card *a2a.AgentCard)
	// Run performs one task in dir and returns the agent's answer. It must
	// stop when ctx ends.
	Run(ctx context.Context, dir string, req taskRequest) (string, error)
}

// taskRequest is one remote request, as the runtime gets it. Text is
// untrusted.
type taskRequest struct {
	Sender string // peer ID the mesh authenticated
	Text   string
}

// agentPolicy is what the agent may take from the directory it works in,
// beyond its files. Each is on, as in any run of the agent there, unless the
// owner turns it off, such as for code from someone else.
type agentPolicy struct {
	// DirInstructions loads the directory's instruction files, such as
	// AGENTS.md (-agent-dir-instructions).
	DirInstructions bool
	// DirSkills loads the directory's skills (-agent-dir-skills).
	DirSkills bool
	// OwnerConfig runs the agent with the owner's own configuration of its
	// runtime: their sign-in, skills, plugins, MCP servers and settings
	// (-agent-owner-config). Without it the agent has its own, in the node's
	// data directory, and signs in on its own.
	OwnerConfig bool
}

// runtimes maps each -agent value to how its runtime is made, from
// -agent-command (empty for the runtime's default command), the node's
// data directory, where the runtime may keep state, and the agent's policy;
// and the service name it
// is served under unless -agent-name says otherwise, and the command it runs
// unless -agent-command does.
var runtimes = map[string]struct {
	new            func(command, dataDir string, policy agentPolicy) (runtime, error)
	defaultName    string
	defaultCommand string
	// signedIn and login are for a runtime whose agent signs in: whether
	// it has, in a way the background service also sees, and the command
	// that signs it in, with how to show that command. A runtime may also
	// accept a credential in the environment (envSignedIn), which only a
	// node run from the owner's shell gets.
	signedIn    func(dataDir string, policy agentPolicy) bool
	envSignedIn func(policy agentPolicy) bool
	login       func(ctx context.Context, command, dataDir string, policy agentPolicy) (*exec.Cmd, string)
	// ownerEnv names the variables that locate the owner's configuration
	// of the runtime. `up` keeps their values for the background service,
	// which starts without the owner's shell, so that with
	// agentPolicy.OwnerConfig the agent finds the same configuration.
	ownerEnv []string
}{
	"codex": {newCodexRuntime, "codex", "codex", codexSignedIn, codexEnvSignedIn, codexLogin, []string{"CODEX_HOME"}},
}

// agentOwnerEnv is what `up` keeps of the runtime's ownerEnv: each variable
// set in its environment, as NAME=value.
func agentOwnerEnv(agent string, policy agentPolicy) []string {
	if !policy.OwnerConfig {
		return nil
	}
	var env []string
	for _, name := range runtimes[agent].ownerEnv {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// agentService is the receiving side of a task: an A2A 1.0 agent that runs
// each task with its runtime in one configured directory.
type agentService struct {
	dir      string
	rt       runtime
	deadline time.Duration
	// stopFile exists while the owner has stopped task execution
	// (`agentiscaled tasks stop`).
	stopFile string
	// life ends when the node stops, and with it any task still running.
	life context.Context

	mu      sync.Mutex
	current *agentRun
	// delivered holds every request that started a task, so that a delivery
	// retry never runs a task twice.
	delivered *deliveries
}

type agentRun struct {
	taskID a2a.TaskID
	cancel context.CancelCauseFunc
	begun  bool
}

var (
	errCancelledByCaller = errors.New("cancelled by the caller")
	errStoppedByOwner    = errors.New("stopped by the owner")
)

// newAgentService makes the agent service for dir, keeping its state in the
// node's data directory.
func newAgentService(dir string, rt runtime, dataDir string) (*agentService, error) {
	delivered, err := loadDeliveries(filepath.Join(dataDir, "tasks-delivered.jsonl"))
	if err != nil {
		return nil, err
	}
	return &agentService{dir: dir, rt: rt, deadline: taskDeadline, stopFile: stopFilePath(dataDir), life: context.Background(), delivered: delivered}, nil
}

// deliveries records the requests that started a task, keyed by the sender
// the mesh authenticated and the message ID, in an append-only file that
// outlives the node process. Message IDs are the sender's, so one sender
// cannot use up another's.
type deliveries struct {
	path string
	seen map[deliveryKey]a2a.TaskID
}

type deliveryKey struct{ sender, messageID string }

type deliveryRecord struct {
	Sender    string     `json:"sender"`
	MessageID string     `json:"message_id"`
	TaskID    a2a.TaskID `json:"task_id"`
}

func loadDeliveries(path string) (*deliveries, error) {
	d := &deliveries{path: path, seen: map[deliveryKey]a2a.TaskID{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r deliveryRecord
		// A line cut short by a crash recorded nothing that started.
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			d.seen[deliveryKey{r.Sender, r.MessageID}] = r.TaskID
		}
	}
	return d, sc.Err()
}

func (d *deliveries) first(sender, messageID string) (a2a.TaskID, bool) {
	id, ok := d.seen[deliveryKey{sender, messageID}]
	return id, ok
}

// record writes the delivery to disk, synced, before the task may start.
func (d *deliveries) record(sender, messageID string, task a2a.TaskID) error {
	line, err := json.Marshal(deliveryRecord{sender, messageID, task})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(d.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(line, '\n')); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	d.seen[deliveryKey{sender, messageID}] = task
	return nil
}

func (s *agentService) stopped() bool {
	_, err := os.Stat(s.stopFile)
	return err == nil
}

func (s *agentService) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		task := a2a.NewSubmittedTask(execCtx, execCtx.Message)
		sender := firstParam(execCtx.ServiceParams, api.HeaderPeerID)
		if sender != "" {
			task.SetMeta(metaSender, sender)
		}
		if !yield(task, nil) {
			return
		}
		reject := func(reason string, meta map[string]any) {
			ev := a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateRejected, agentText(execCtx, reason))
			ev.Metadata = map[string]any{metaTerminal: "rejected"}
			for k, v := range meta {
				ev.Metadata[k] = v
			}
			yield(ev, nil)
		}
		// The node stamps the authenticated caller on every request it
		// delivers from the mesh; one without it did not come through the
		// mesh.
		if sender == "" {
			reject("refused: the request carries no authenticated sender", nil)
			return
		}
		if execCtx.StoredTask != nil {
			reject("refused: a task takes exactly one message", nil)
			return
		}
		if depth, ok := delegationDepth(execCtx.Message); !ok || depth > maxDelegationDepth {
			reject(fmt.Sprintf("refused: delegation depth above %d", maxDelegationDepth), nil)
			return
		}
		request := messageText(execCtx.Message)
		if strings.TrimSpace(request) == "" {
			reject("refused: the request has no text", nil)
			return
		}

		// The run outlives the request that started it (the caller polls),
		// but not the node.
		runCtx, cancel := context.WithCancelCause(s.life)
		defer cancel(nil)
		run, why, meta := s.admit(execCtx, sender, cancel)
		if run == nil {
			reject(why, meta)
			return
		}
		defer s.finish(run)

		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}
		runCtx, stopDeadline := context.WithTimeoutCause(runCtx, s.deadline, context.DeadlineExceeded)
		defer stopDeadline()
		go s.watchStop(runCtx, cancel)

		s.mu.Lock()
		run.begun = true
		s.mu.Unlock()
		out, err := s.rt.Run(runCtx, s.dir, taskRequest{Sender: sender, Text: request})

		switch cause := context.Cause(runCtx); {
		case errors.Is(cause, errCancelledByCaller):
			// Cancel has already ended the task.
			return
		case errors.Is(cause, errStoppedByOwner):
			ev := a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, agentText(execCtx, "cancelled: the owner stopped task execution on this node"))
			ev.Metadata = map[string]any{metaTerminal: "cancelled", metaExecutionBegun: true}
			yield(ev, nil)
			return
		case errors.Is(cause, context.DeadlineExceeded):
			ev := a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, agentText(execCtx, fmt.Sprintf("timed out after %s", s.deadline)))
			ev.Metadata = map[string]any{metaTerminal: "timeout"}
			yield(ev, nil)
			return
		}
		if err != nil {
			ev := a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, agentText(execCtx, "task failed: "+err.Error()))
			ev.Metadata = map[string]any{metaTerminal: "failed"}
			yield(ev, nil)
			return
		}
		artifact := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(out))
		artifact.Artifact.Name = "result"
		if !yield(artifact, nil) {
			return
		}
		ev := a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil)
		ev.Metadata = map[string]any{metaTerminal: "completed"}
		yield(ev, nil)
	}
}

// admit takes the one running slot for a task, or says why not.
func (s *agentService) admit(execCtx *a2asrv.ExecutorContext, sender string, cancel context.CancelCauseFunc) (*agentRun, string, map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if first, ok := s.delivered.first(sender, execCtx.Message.ID); ok {
		return nil, fmt.Sprintf("refused: message %s was already delivered as task %s", execCtx.Message.ID, first), map[string]any{metaDuplicateOf: string(first)}
	}
	if s.stopped() {
		return nil, "refused: the owner has stopped task execution on this node", nil
	}
	if s.current != nil {
		return nil, "refused: a task is already running; try again when it ends", nil
	}
	// Without the record a retry after a restart could run the task again,
	// so a task that cannot be recorded does not start.
	if err := s.delivered.record(sender, execCtx.Message.ID, execCtx.TaskID); err != nil {
		log.Printf("agent service: record delivery: %v", err)
		return nil, "refused: the node could not record the request", nil
	}
	s.current = &agentRun{taskID: execCtx.TaskID, cancel: cancel}
	return s.current, "", nil
}

func (s *agentService) finish(run *agentRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == run {
		s.current = nil
	}
}

// watchStop ends the run when the owner stops task execution.
func (s *agentService) watchStop(ctx context.Context, cancel context.CancelCauseFunc) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.stopped() {
				cancel(errStoppedByOwner)
				return
			}
		}
	}
}

func (s *agentService) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		s.mu.Lock()
		begun := false
		if run := s.current; run != nil && run.taskID == execCtx.TaskID {
			begun = run.begun
			run.cancel(errCancelledByCaller)
		}
		s.mu.Unlock()
		text := "cancelled before the task started"
		if begun {
			text = "cancelled after the task had started; it was stopped"
		}
		ev := a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, agentText(execCtx, text))
		ev.Metadata = map[string]any{metaTerminal: "cancelled", metaExecutionBegun: begun}
		yield(ev, nil)
	}
}

func (s *agentService) card(name string) *a2a.AgentCard {
	card := &a2a.AgentCard{
		Name:    name,
		Version: "0.1.0",
		// The node rewrites this to the mesh URL for callers.
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface("/", a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
	}
	s.rt.Describe(card)
	return card
}

// handler serves the agent card and JSON-RPC, and only to the node, which
// sends token as a bearer credential (target_url userinfo). Any other local
// process could otherwise call the agent directly and claim any sender.
func (s *agentService) handler(name, token string) http.Handler {
	card := s.card(name)
	cardHandler := a2asrv.NewStaticAgentCardHandler(card)
	rpc := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(s, a2asrv.WithCallInterceptors(peerIdentity{})))
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && (r.URL.Path == "/.well-known/agent-card.json" || r.URL.Path == "/") {
			// While stopped the agent is not ready, so the node stops
			// advertising it.
			if s.stopped() {
				http.Error(w, "task execution is stopped on this node", http.StatusServiceUnavailable)
				return
			}
			cardHandler.ServeHTTP(w, r)
			return
		}
		rpc.ServeHTTP(w, r)
	})
}

// peerIdentity makes the peer that the node authenticated and stamped on the
// request the A2A user of the call. The task store keeps each task for the
// user that created it, so a peer can get or cancel only its own tasks, and a
// call without a peer ID did not come through the mesh.
type peerIdentity struct {
	a2asrv.PassthroughCallInterceptor
}

func (peerIdentity) Before(ctx context.Context, callCtx *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	peer := firstParam(callCtx.ServiceParams(), api.HeaderPeerID)
	if peer == "" {
		return ctx, nil, a2a.ErrUnauthenticated
	}
	callCtx.User = a2asrv.NewAuthenticatedUser(peer, nil)
	return ctx, nil, nil
}

// serveAgent starts the agent service on a loopback port and returns the
// target URL to register with the node, credential included.
func serveAgent(ctx context.Context, s *agentService, name string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	s.life = ctx
	srv := &http.Server{Handler: s.handler(name, token), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("agent service: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return fmt.Sprintf("http://:%s@%s", token, ln.Addr()), nil
}

func stopFilePath(dataDir string) string { return filepath.Join(dataDir, "tasks-stopped") }

func delegationDepth(m *a2a.Message) (int, bool) {
	v, ok := m.Metadata[metaDelegationDepth]
	if !ok {
		return 0, true
	}
	f, ok := v.(float64)
	if !ok || f < 0 || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

func messageText(m *a2a.Message) string {
	var parts []string
	for _, p := range m.Parts {
		if t := p.Text(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func agentText(info a2a.TaskInfoProvider, text string) *a2a.Message {
	return a2a.NewMessageForTask(a2a.MessageRoleAgent, info, a2a.NewTextPart(text))
}

func firstParam(p *a2asrv.ServiceParams, key string) string {
	if p == nil {
		return ""
	}
	if v, ok := p.Get(key); ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// failureLines picks from a failed run's output what says why it failed: its
// last lines that mention an error, or else its last lines, at most n of
// them and about 1 KB in all.
func failureLines(s string, n int) string {
	var all, errs []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		all = append(all, l)
		if strings.Contains(strings.ToLower(l), "error") {
			errs = append(errs, l)
		}
	}
	pick := all
	if len(errs) > 0 {
		pick = errs
	}
	pick = pick[max(len(pick)-n, 0):]
	out := strings.Join(pick, " | ")
	if len(out) > 1024 {
		out = "…" + out[len(out)-1024:]
	}
	return out
}
