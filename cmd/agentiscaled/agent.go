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
	"io"
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
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/agentmesh/api"
)

// The bounds on hosted agents' tasks.
const (
	// taskDeadline bounds a task's running time, over all its turns.
	taskDeadline = 15 * time.Minute
	// maxQuestions bounds the questions the agent may ask in one task.
	maxQuestions = 5
	// maxWaiting bounds an agent's tasks that wait for an answer.
	maxWaiting = 16
	// maxDelegationDepth bounds how far a task may have been delegated: a
	// person's client sends at depth 0, and a hosted agent working on such a
	// task may call other agents (taskNetwork), at depth 1, which its
	// token names (senderOf). A task at the bound runs without the
	// network, so it cannot delegate further, and a deeper one is refused.
	maxDelegationDepth = 1
)

// Metadata keys agentiscale reads and writes on A2A messages and tasks.
const (
	// metaTerminal names why a task ended, since A2A 1.0 has no timed-out
	// state: completed, rejected, cancelled, timeout or failed.
	metaTerminal = "agentiscale.terminal"
	// metaExecutionBegun says whether the runtime had started when a task
	// was cancelled.
	metaExecutionBegun = "agentiscale.execution_begun"
	metaSender         = "agentiscale.sender_peer_id"
	// metaProgress marks a working status whose message is the agent's
	// latest step.
	metaProgress    = "agentiscale.progress"
	metaDuplicateOf = "agentiscale.duplicate_of"
)

// runtime runs the tasks of a hosted agent with one agent CLI. The agent
// service around it keeps the A2A contract and the bounds, so supporting
// another agent CLI means adding a runtime.
type runtime interface {
	// Describe says on the agent card what the agent does.
	Describe(card *a2a.AgentCard)
	// Run performs one turn of a task in dir: the task itself or, with
	// req.Session, the caller's answer to the question the turn before it
	// asked, in that turn's session. It must stop when ctx ends.
	Run(ctx context.Context, dir string, req taskRequest) (turnResult, error)
}

// taskRequest is one remote request, as the runtime gets it. Text is
// untrusted.
type taskRequest struct {
	Sender string // peer ID the mesh authenticated
	Text   string
	// Session is the runtime's session of the task's turn before, which
	// ended in a question; Text is then the caller's answer. It is empty
	// for a new task.
	Session string
	// Progress, if set, takes the turn's steps as they happen, as short
	// lines (progressLine). It is called from one goroutine at a time and
	// must not block.
	Progress func(string)
	// Network, if set, is the turn's way to the network's other agents,
	// which the runtime offers the agent as an MCP server. It works only
	// while the turn runs.
	Network *networkAccess
}

func (r taskRequest) progress(line string) {
	if r.Progress != nil {
		r.Progress(line)
	}
}

// turnResult is how one turn of a task ended.
type turnResult struct {
	// Text is the agent's result or, with Question, its question to the
	// caller.
	Text     string
	Question bool
	// Session is the runtime's session of the turn, which the next turn
	// resumes.
	Session string
}

// agentPolicy is what the agent may take from the directory it works in,
// beyond its files. Each is on, as in any run of the agent there, unless the
// owner turns it off, such as for code from someone else.
type agentPolicy struct {
	// DirInstructions loads the directory's instruction files, such as
	// AGENTS.md or CLAUDE.md (-agent-dir-instructions).
	DirInstructions bool
	// DirSkills loads the directory's skills (-agent-dir-skills).
	DirSkills bool
	// OwnerConfig runs the agent with the owner's own configuration of its
	// runtime: their sign-in, skills, plugins, MCP servers, settings and permissions
	// (-agent-owner-config). Without it the agent has its own, in the node's
	// data directory, and signs in on its own.
	OwnerConfig bool
}

// agentNameError tells why a name cannot be an agent's service name in the
// network, by the rule the node's service registry applies to a2a://<name>.
func agentNameError(name string) error {
	if err := api.ValidateServiceFormat(api.ServiceTypeStringA2A + "://" + name); err != nil {
		return fmt.Errorf("agent name %q: the network takes a DNS name, of labels up to 63 characters", name)
	}
	return nil
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
	ownerConfigDir func() (string, error)
	// signedIn and login are for a runtime whose agent signs in: whether
	// it has, in a way the background service also sees, and the command
	// that signs it in, with how to show that command. A runtime may also
	// accept a credential in the environment (envSignedIn), which only a
	// node run from the owner's shell gets.
	signedIn    func(command, dataDir string, policy agentPolicy) bool
	envSignedIn func(policy agentPolicy) bool
	login       func(ctx context.Context, command, dataDir string, policy agentPolicy) (*exec.Cmd, string)
	// ownerEnv names the variables that locate the owner's configuration
	// of the runtime. `up` keeps their values for the background service,
	// which starts without the owner's shell, so that with
	// agentPolicy.OwnerConfig the agent finds the same configuration.
	ownerEnv []string
}{
	"codex":  {newCodexRuntime, "codex", "codex", codexOwnerConfigDir, codexSignedIn, codexEnvSignedIn, codexLogin, []string{"CODEX_HOME"}},
	"claude": {newClaudeRuntime, "claude", "claude", claudeOwnerConfigDir, claudeSignedIn, claudeEnvSignedIn, claudeLogin, []string{"CLAUDE_CONFIG_DIR"}},
}

func ownerRuntimeConfigDir(variable, defaultDir string) (string, error) {
	if dir := os.Getenv(variable); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, defaultDir), nil
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
	dir string
	rt  runtime
	// description, when set, replaces the runtime's on the card
	// (-agent-description).
	description string
	deadline    time.Duration
	// stopFile exists while the owner has stopped task execution
	// (`agentiscaled task stop`).
	stopFile string
	// life ends when the node stops, and with it any task still running.
	life context.Context
	// name is the agent's service name, and network, when set, opens the
	// network to each turn of its tasks that may delegate.
	name    string
	network *taskNetwork

	mu      sync.Mutex
	current *agentRun
	// waiting holds the tasks whose agent asked the caller a question,
	// until the answer comes.
	waiting map[a2a.TaskID]*waitingTask
	// delivered holds every request that started a task, so that a delivery
	// retry never runs a task twice.
	delivered *deliveries
}

type agentRun struct {
	taskID a2a.TaskID
	cancel context.CancelCauseFunc
	begun  bool
}

// waitingTask is a task that waits for the caller's answer to its agent's
// question.
type waitingTask struct {
	sender   string
	session  string
	question string
	since    time.Time
	// used is the task's running time so far, and questions the questions
	// its agent has asked.
	used      time.Duration
	questions int
	// depth is how far the task had been delegated when it came, which
	// its answers cannot change.
	depth int
}

// answerWait is how long a task waits for the caller to answer the agent's
// question.
var answerWait = time.Hour

var (
	errCancelledByCaller = errors.New("cancelled by the caller")
	errStoppedByOwner    = errors.New("stopped by the owner")
)

// newAgentService makes the service of the agent named name, which works in
// dir, keeping its state in the node's data directory: what is the agent's
// alone under agents/<name>, and the owner's stop for all of the node's
// agents.
func newAgentService(dir string, rt runtime, dataDir, name string) (*agentService, error) {
	state := filepath.Join(dataDir, "agents", name)
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, err
	}
	delivered, err := loadDeliveries(filepath.Join(state, "tasks-delivered.jsonl"))
	if err != nil {
		return nil, err
	}
	return &agentService{dir: dir, rt: rt, name: name, deadline: taskDeadline, stopFile: stopFilePath(dataDir), life: context.Background(), delivered: delivered, waiting: map[a2a.TaskID]*waitingTask{}}, nil
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
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var offset int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		if len(line) == 0 {
			return d, nil
		}
		complete := line[len(line)-1] == '\n'
		var record deliveryRecord
		if err := json.Unmarshal(line, &record); err != nil {
			if complete {
				// This may include an action that already ran. Never discard
				// completed, corrupt history and silently allow its retry.
				return nil, fmt.Errorf("invalid delivery log %s at byte %d: %w", path, offset, err)
			}
			// A torn final append did not authorize execution. Remove it and
			// sync the repair before a later append may authorize new work.
			if err := f.Truncate(offset); err != nil {
				return nil, err
			}
			if err := f.Sync(); err != nil {
				return nil, err
			}
			return d, nil
		}
		if record.Sender == "" || record.MessageID == "" || record.TaskID == "" {
			return nil, fmt.Errorf("invalid delivery log %s at byte %d: missing identity fields", path, offset)
		}
		d.seen[deliveryKey{record.Sender, record.MessageID}] = record.TaskID
		offset += int64(len(line))
		if !complete {
			// Preserve a valid record missing only its separator. Otherwise
			// the next append would turn both records into one invalid line.
			if _, err := f.Seek(0, io.SeekEnd); err != nil {
				return nil, err
			}
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return nil, err
			}
			if err := f.Sync(); err != nil {
				return nil, err
			}
			return d, nil
		}
	}
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

// Execute runs one turn of a task: a new task, or the caller's answer on a
// task whose agent asked a question (input-required), which carries on in
// the runtime's session of the turn before.
func (s *agentService) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		from, fromErr := senderOf(execCtx.ServiceParams)
		sender := from.String()
		answer := execCtx.StoredTask != nil
		if !answer {
			task := a2a.NewSubmittedTask(execCtx, execCtx.Message)
			if from.peer != "" {
				task.SetMeta(metaSender, from.peer)
			}
			if !yield(task, nil) {
				return
			}
		}
		end := func(state a2a.TaskState, terminal, reason string, meta map[string]any) {
			ev := a2a.NewStatusUpdateEvent(execCtx, state, agentText(execCtx, reason))
			ev.Metadata = map[string]any{metaTerminal: terminal}
			for k, v := range meta {
				ev.Metadata[k] = v
			}
			yield(ev, nil)
		}
		var prior waitingTask
		if answer {
			w := s.waitingFor(execCtx.TaskID)
			if w == nil {
				end(a2a.TaskStateFailed, "timeout", fmt.Sprintf("timed out: no answer came within %s of the question", answerWait), nil)
				return
			}
			prior = *w
		}
		// A new task that cannot run is rejected. An answer that cannot be
		// taken now is refused, and the task waits for one that can.
		refuse := func(reason string, meta map[string]any) {
			if answer {
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, agentText(execCtx,
					reason+"\n\nThe question still waits for your answer: "+prior.question)), nil)
				return
			}
			end(a2a.TaskStateRejected, "rejected", reason, meta)
		}
		// The node stamps the authenticated caller on every request it
		// delivers from the mesh; one without it did not come through the
		// mesh.
		if fromErr != nil {
			refuse("refused: "+fromErr.Error(), nil)
			return
		}
		if answer && prior.sender != sender {
			refuse("refused: only the task's sender can answer", nil)
			return
		}
		depth := from.depth
		if depth > maxDelegationDepth {
			refuse(fmt.Sprintf("refused: delegation depth above %d", maxDelegationDepth), nil)
			return
		}
		if answer {
			// The task is as deep as it was when it came.
			depth = prior.depth
		}
		request := messageText(execCtx.Message)
		if strings.TrimSpace(request) == "" {
			refuse("refused: the request has no text", nil)
			return
		}

		// The run outlives the request that started it (the caller polls),
		// but not the node.
		runCtx, cancel := context.WithCancelCause(s.life)
		defer cancel(nil)
		run, why, meta := s.admit(execCtx, sender, cancel, answer)
		if run == nil {
			refuse(why, meta)
			return
		}
		defer s.finish(run)

		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}
		runCtx, stopDeadline := context.WithTimeoutCause(runCtx, s.deadline-prior.used, context.DeadlineExceeded)
		defer stopDeadline()
		go s.watchStop(runCtx, cancel)

		s.mu.Lock()
		run.begun = true
		s.mu.Unlock()
		started := time.Now()
		// The run reports its steps while it works; the latest goes to the
		// caller as the message of the working status, at most one every
		// progressEvery and maxProgress in a turn, since each one stays in
		// the task's history.
		progress := make(chan string, 1)
		done := make(chan runOutcome, 1)
		req := taskRequest{Sender: from.peer, Text: request, Session: prior.session, Progress: func(line string) {
			select {
			case <-progress:
			default:
			}
			progress <- line
		}}
		// The turn may call other agents with a token of its own, which ends
		// with the turn: revoked when it ends, expired when its time is up.
		release := func() {}
		asked := false
		defer func() {
			// The tasks it sent stay its own while it waits for an answer.
			if !asked {
				s.network.forget(execCtx.TaskID)
			}
		}()
		if s.network != nil && depth < maxDelegationDepth {
			left := s.deadline - prior.used
			access, revoke, err := s.network.grant(runCtx, s.name, execCtx.TaskID, left, depth+1)
			if err != nil {
				log.Printf("agent %s: task %s runs without the network: %v", s.name, execCtx.TaskID, err)
			} else {
				req.Network, release = access, revoke
			}
		}
		go func() {
			out, err := s.rt.Run(runCtx, s.dir, req)
			release()
			done <- runOutcome{out, err}
		}()
		var res runOutcome
		var last time.Time
		sent := 0
	follow:
		for {
			select {
			case res = <-done:
				break follow
			case line := <-progress:
				if sent >= maxProgress {
					continue
				}
				if wait := progressEvery - time.Since(last); wait > 0 {
					select {
					case res = <-done:
						break follow
					case <-time.After(wait):
					}
					select {
					case line = <-progress:
					default:
					}
				}
				last = time.Now()
				sent++
				ev := a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, agentText(execCtx, line))
				ev.Metadata = map[string]any{metaProgress: true}
				if !yield(ev, nil) {
					cancel(nil)
					<-done
					return
				}
			}
		}
		out, err := res.out, res.err

		switch cause := context.Cause(runCtx); {
		case errors.Is(cause, errCancelledByCaller):
			// Cancel has already ended the task.
			return
		case errors.Is(cause, errStoppedByOwner):
			end(a2a.TaskStateCanceled, "cancelled", "cancelled: the owner stopped task execution on this node", map[string]any{metaExecutionBegun: true})
			return
		case errors.Is(cause, context.DeadlineExceeded):
			end(a2a.TaskStateFailed, "timeout", fmt.Sprintf("timed out after %s", s.deadline), nil)
			return
		}
		if err != nil {
			end(a2a.TaskStateFailed, "failed", "task failed: "+err.Error(), nil)
			return
		}
		if out.Question {
			if why := s.wait(execCtx.TaskID, waitingTask{sender: sender, session: out.Session, question: out.Text, since: time.Now(),
				used: prior.used + time.Since(started), questions: prior.questions + 1, depth: depth}); why != "" {
				end(a2a.TaskStateFailed, "failed", "task failed: "+why, nil)
				return
			}
			asked = true
			// The slot is free before the caller learns of the question,
			// so that the answer finds it free.
			s.finish(run)
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, agentText(execCtx, out.Text)), nil)
			return
		}
		artifact := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(out.Text))
		artifact.Artifact.Name = "result"
		if !yield(artifact, nil) {
			return
		}
		ev := a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil)
		ev.Metadata = map[string]any{metaTerminal: "completed"}
		yield(ev, nil)
	}
}

// progressEvery is the least time between two progress messages of a
// task, and maxProgress the most in one turn.
var progressEvery = 3 * time.Second

const maxProgress = 100

type runOutcome struct {
	out turnResult
	err error
}

// waitingFor is the task waiting for an answer under id, if it still
// waits.
func (s *agentService) waitingFor(id a2a.TaskID) *waitingTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.waiting[id]
	if w == nil || time.Since(w.since) > answerWait {
		delete(s.waiting, id)
		s.network.forget(id)
		return nil
	}
	return w
}

// wait keeps a task whose agent asked a question until the answer comes,
// or says why it cannot wait.
func (s *agentService) wait(id a2a.TaskID, w waitingTask) string {
	switch {
	case w.session == "":
		return "the agent asked a question, but its runtime cannot carry on the task"
	case w.questions > maxQuestions:
		return fmt.Sprintf("the agent asked more than %d questions", maxQuestions)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for other, o := range s.waiting {
		if time.Since(o.since) > answerWait {
			delete(s.waiting, other)
			s.network.forget(other)
		}
	}
	if len(s.waiting) >= maxWaiting {
		return fmt.Sprintf("the agent asked a question, but %d tasks already wait for answers", maxWaiting)
	}
	s.waiting[id] = &w
	return ""
}

// admit takes the one running slot for a turn of a task, or says why not.
// An answer takes its task out of the waiting ones.
func (s *agentService) admit(execCtx *a2asrv.ExecutorContext, sender string, cancel context.CancelCauseFunc, answer bool) (*agentRun, string, map[string]any) {
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
	if answer && s.waiting[execCtx.TaskID] == nil {
		// Cancelled meanwhile; a cancelled task takes no more messages.
		return nil, "refused: the task no longer waits for an answer", nil
	}
	// Without the record a retry after a restart could run the task again,
	// so a task that cannot be recorded does not start.
	if err := s.delivered.record(sender, execCtx.Message.ID, execCtx.TaskID); err != nil {
		log.Printf("agent service: record delivery: %v", err)
		return nil, "refused: the node could not record the request", nil
	}
	delete(s.waiting, execCtx.TaskID)
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
		// A task waiting for an answer has run already.
		if _, ok := s.waiting[execCtx.TaskID]; ok {
			begun = true
			delete(s.waiting, execCtx.TaskID)
			s.network.forget(execCtx.TaskID)
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
		// A caller can follow a task's progress as it happens
		// (SendStreamingMessage, SubscribeToTask). Nothing is pushed to a
		// caller's own URL.
		Capabilities: a2a.AgentCapabilities{Streaming: true},
	}
	s.rt.Describe(card)
	if s.description != "" {
		card.Description = s.description
	}
	return card
}

// handler serves the agent card and JSON-RPC, and only to the node, which
// sends token as a bearer credential (target_url userinfo). Any other local
// process could otherwise call the agent directly and claim any sender.
func (s *agentService) handler(name, token string) http.Handler {
	card := s.card(name)
	cardHandler := a2asrv.NewStaticAgentCardHandler(card)
	store := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{Authenticator: a2asrv.NewTaskStoreAuthenticator()})
	rpc := a2asrv.NewJSONRPCHandler(boundedStreams{a2asrv.NewHandler(s, a2asrv.WithTaskStore(store),
		a2asrv.WithCallInterceptors(peerIdentity{tasks: store}))})
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

// peerIdentity makes the sender that the node authenticated and stamped on
// the request (senderOf: the peer, and the task its token is for) the A2A
// user of the call. The task store keeps each task for the user that created
// it, so a sender can get, cancel or follow only its own tasks: a hosted
// agent's task only the tasks it sent itself. A call without a peer ID did
// not come through the mesh.
type peerIdentity struct {
	a2asrv.PassthroughCallInterceptor
	// tasks is the agent's task store, which keeps each task's owner.
	tasks taskstore.Store
}

func (p peerIdentity) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	from, err := senderOf(callCtx.ServiceParams())
	if err != nil {
		return ctx, nil, a2a.ErrUnauthenticated
	}
	callCtx.User = a2asrv.NewAuthenticatedUser(from.String(), nil)
	// A2A serves a running task's events to whoever subscribes to its ID:
	// only the task's owner may.
	if sub, ok := req.Payload.(*a2a.SubscribeToTaskRequest); ok && sub != nil {
		if _, err := p.tasks.Get(ctx, sub.ID); err != nil {
			return ctx, nil, a2a.ErrTaskNotFound
		}
	}
	return ctx, nil, nil
}

// maxStream bounds how long the agent streams a task's events to one
// request. The mesh does not pass a caller's hang-up on to the agent: a
// stream the caller left stays open on the agent's side until its next
// event. Ended on time, it closes all the way through the mesh. A caller
// that still wants the events subscribes again (SubscribeToTask).
var maxStream = 2 * time.Minute

// boundedStreams ends each stream of task events after maxStream.
type boundedStreams struct {
	a2asrv.RequestHandler
}

func (b boundedStreams) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return boundStream(ctx, func(ctx context.Context) iter.Seq2[a2a.Event, error] {
		return b.RequestHandler.SendStreamingMessage(ctx, req)
	})
}

func (b boundedStreams) SubscribeToTask(ctx context.Context, req *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	return boundStream(ctx, func(ctx context.Context) iter.Seq2[a2a.Event, error] {
		return b.RequestHandler.SubscribeToTask(ctx, req)
	})
}

// boundStream runs stream for at most maxStream and then ends it, without
// an error: the task goes on, only this stream of its events ends.
func boundStream(ctx context.Context, stream func(context.Context) iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		bounded, cancel := context.WithTimeout(ctx, maxStream)
		defer cancel()
		for ev, err := range stream(bounded) {
			if err != nil && bounded.Err() != nil && ctx.Err() == nil {
				return
			}
			if !yield(ev, err) {
				return
			}
		}
	}
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
