package node

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log"
	"maps"
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
	"github.com/whiteredhouse/agentiscaled/internal/caller"
	"github.com/whiteredhouse/agentiscaled/internal/nodestate"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The bounds on hosted agents' tasks.
const (
	// taskDeadline bounds a task's running time, over all its turns.
	taskDeadline = caller.DefaultTaskDeadline
	// defaultAnswerWait bounds the time a caller has to answer a question.
	defaultAnswerWait = time.Hour
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
	metaTerminal = caller.MetaTerminal
	// metaExecutionBegun says whether the runtime had started when a task
	// was cancelled.
	metaExecutionBegun = caller.MetaExecutionBegun
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
	// Task is the node's ID of the task, the same over its turns.
	Task string
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

// agentStateDir is the node's state for one hosted service. Ordinary offer
// names are already safe path components. Other accepted names can differ
// only by case or be dot names; hashing the exact name keeps them distinct
// even on a case-insensitive filesystem.
func agentStateDir(dataDir, name string) string {
	if !validAgentName(name) {
		name = fmt.Sprintf("~%x", sha256.Sum256([]byte(name)))
	}
	return filepath.Join(dataDir, "agents", name)
}

// agentProfileDir holds the runtime's private configuration and sign-in.
func agentProfileDir(dataDir, name string) string {
	return filepath.Join(agentStateDir(dataDir, name), "profile")
}

// agentNameError tells why a name cannot be an agent's service name in the
// network (caller.ParseAgentName).
func agentNameError(name string) error {
	_, err := caller.ParseAgentName(name)
	return err
}

// runtimes defines each runtime's default service name, command and sign-in
// behavior. Callers pass the named service's private profile directory.
// With OwnerConfig enabled, the runtime uses the owner's configuration.
var runtimes = map[string]struct {
	new            func(command, profileDir string, policy agentPolicy) (runtime, error)
	defaultName    string
	defaultCommand string
	ownerConfigDir func() (string, error)
	// signedIn and login are for a runtime whose agent signs in: whether
	// it has, in a way the background service also sees, and the command
	// that signs it in, with how to show that command. A runtime may also
	// accept a credential in the environment (envSignedIn), which only a
	// node run from the owner's shell gets.
	signedIn    func(command, profileDir string, policy agentPolicy) bool
	envSignedIn func(policy agentPolicy) bool
	login       func(ctx context.Context, command, profileDir string, policy agentPolicy) (*exec.Cmd, string)
	// ownerEnv names the variables that locate the owner's configuration
	// of the runtime. `up` keeps their values for the background service,
	// which starts without the owner's shell, so that with
	// agentPolicy.OwnerConfig the agent finds the same configuration.
	ownerEnv []string
}{
	"codex": {newCodexRuntime, "codex", "codex", codexOwnerConfigDir, codexSignedIn, codexEnvSignedIn, codexLogin, []string{"CODEX_HOME"}},
	// acp agents are made from their command and arguments
	// (agentConfig.newRuntime).
	"acp":    {},
	"claude": {newClaudeRuntime, "claude", "claude", claudeOwnerConfigDir, claudeSignedIn, claudeEnvSignedIn, claudeLogin, []string{"CLAUDE_CONFIG_DIR"}},
}

func ownerRuntimeConfigDir(variable, defaultDir string) (string, error) {
	return nodestate.RuntimeConfigDir(variable, defaultDir)
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
	answerWait  time.Duration
	// life ends when the node stops, and with it any task still running.
	life context.Context
	// name is the agent's service name, and network, when set, opens the
	// network to each turn of its tasks that may delegate.
	name    caller.AgentName
	network *taskNetwork

	mu      sync.Mutex
	current *agentRun
	// waiting holds the tasks whose agent asked the caller a question,
	// until the answer comes.
	waiting map[a2a.TaskID]*waitingTask
	// updates publishes the agent's own updates to its tasks, outside a
	// request (handler): the timeout of a question nobody answered (lapse).
	updates a2asrv.TaskUpdater
	// delivered holds every request that started a task, so that a delivery
	// retry never runs a task twice.
	delivered *deliveries
	// drainFile holds the owner's drain of the agent (agentiscaled agent
	// drain), which the node reads for each new task.
	drainFile string
	// runtimeName is what runs the agent's tasks, as its settings name it
	// (codex, claude, acp), for callers and the owner's console.
	runtimeName string
}

type agentRun struct {
	taskID  a2a.TaskID
	cancel  context.CancelCauseFunc
	begun   bool
	started time.Time
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
	// timer ends the task when its answer has not come in time.
	timer *time.Timer
	// contextID is the task's A2A context, which the timeout names (lapse).
	contextID string
}

var (
	errCancelledByCaller = errors.New("cancelled by the caller")
)

// newAgentService keeps the named agent's delivery history in its service
// directory. All agents on the node share the owner's stop file.
func newAgentService(dir string, rt runtime, dataDir string, name caller.AgentName) (*agentService, error) {
	state := agentStateDir(dataDir, name.String())
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, err
	}
	delivered, err := loadDeliveries(filepath.Join(state, "tasks-delivered.jsonl"))
	if err != nil {
		return nil, err
	}
	return &agentService{dir: dir, rt: rt, name: name, deadline: taskDeadline, answerWait: defaultAnswerWait, life: context.Background(), delivered: delivered, waiting: map[a2a.TaskID]*waitingTask{}, drainFile: drainPath(dataDir, name.String())}, nil
}

// drain is the owner's drain of the agent, or nil while it takes new tasks.
func (s *agentService) drain() *drainMark {
	if s.drainFile == "" {
		return nil
	}
	return loadDrain(s.drainFile)
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
		// The run outlives the request that started it (the caller polls),
		// but not the node.
		runCtx, cancel := context.WithCancelCause(s.life)
		defer cancel(nil)
		request := messageText(execCtx.Message)
		run, waiting, why, meta := s.admit(execCtx, from, fromErr, request, cancel)
		if run == nil {
			switch {
			case !answer:
				end(a2a.TaskStateRejected, "rejected", why, meta)
			case waiting == nil:
				end(a2a.TaskStateFailed, "timeout", why, map[string]any{metaExecutionBegun: true})
			default:
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, agentText(execCtx,
					why+"\n\nThe question still waits for your answer: "+waiting.question)), nil)
			}
			return
		}
		var prior waitingTask
		depth := from.depth
		if waiting != nil {
			prior = *waiting
			depth = prior.depth
		}
		defer s.finish(run)
		// From here the task either asks the caller a question, and waits
		// for the answer, or ends: on every other path, whatever it holds
		// over its turns goes (endTask).
		waits := false
		defer func() {
			if !waits {
				s.endTask(execCtx.TaskID)
			}
		}()

		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}
		runCtx, stopDeadline := context.WithTimeoutCause(runCtx, s.deadline-prior.used, context.DeadlineExceeded)
		defer stopDeadline()

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
		req := taskRequest{Sender: from.peer, Text: request, Session: prior.session, Task: string(execCtx.TaskID), Progress: func(line string) {
			select {
			case <-progress:
			default:
			}
			progress <- line
		}}
		// The turn may call other agents with a token of its own, which ends
		// with the turn: revoked when it ends, expired when its time is up.
		release := func() {}
		if s.network != nil && depth < maxDelegationDepth {
			left := s.deadline - prior.used
			access, revoke, err := s.network.grant(runCtx, s.name, execCtx.TaskID, left, depth+1)
			if err != nil {
				log.Printf("agent %s: this turn of task %s runs without the network: %v", s.name, execCtx.TaskID, err)
			}
			if access != nil {
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
		case errors.Is(cause, context.Canceled):
			end(a2a.TaskStateCanceled, "cancelled", "cancelled: the node is shutting down", map[string]any{metaExecutionBegun: true})
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
				used: prior.used + time.Since(started), questions: prior.questions + 1, depth: depth, contextID: execCtx.ContextID}); why != "" {
				end(a2a.TaskStateFailed, "failed", "task failed: "+why, nil)
				return
			}
			// The slot is free before the caller learns of the question,
			// so that the answer finds it free.
			s.finish(run)
			// A question the caller never gets ends the task.
			waits = yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, agentText(execCtx, out.Text)), nil)
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

// A sessionKeeper retains a session between turns. At shutdown releaseAll
// also stops active runs and waits for the runtime's processes to exit.
type sessionKeeper interface {
	release(task string)
	releaseAll()
}

// release lets the runtime go of what it keeps for task.
func (s *agentService) release(task a2a.TaskID) {
	if k, ok := s.rt.(sessionKeeper); ok && task != "" {
		go k.release(string(task))
	}
}

// shutdown runs after the service context is cancelled.
func (s *agentService) shutdown() {
	s.mu.Lock()
	for id, w := range s.waiting {
		w.timer.Stop()
		s.network.forget(id)
	}
	clear(s.waiting)
	s.mu.Unlock()
	if k, ok := s.rt.(sessionKeeper); ok {
		k.releaseAll()
	}
}

// endTask lets go of everything the task under id holds over its turns:
// its wait for an answer, what its runtime keeps for it (its agent's
// process) and its network (the grant and the tasks it sent). Every way a
// task ends comes here, and coming again does nothing more.
func (s *agentService) endTask(id a2a.TaskID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endTaskLocked(id)
}

func (s *agentService) endTaskLocked(id a2a.TaskID) {
	if w := s.waiting[id]; w != nil && w.timer != nil {
		w.timer.Stop()
	}
	delete(s.waiting, id)
	s.release(id)
	s.network.forget(id)
}

// expireLocked releases an unanswered task and publishes its timeout once
// the task store can accept the update.
func (s *agentService) expireLocked(id a2a.TaskID) {
	w := s.waiting[id]
	if w == nil {
		return
	}
	s.endTaskLocked(id)
	reason := s.answerTimeoutReason()
	go s.lapse(a2a.TaskInfo{TaskID: id, ContextID: w.contextID}, reason)
}

// lapse publishes that the task, whose answer did not come in time, has
// timed out. It is the agent's own update to its task, not a caller's: the
// task store and whoever follows the task get the terminal state, as from any
// other end of a task.
func (s *agentService) lapse(task a2a.TaskInfo, reason string) {
	if s.updates == nil {
		return
	}
	end := a2a.NewStatusUpdateEvent(task, a2a.TaskStateFailed, agentText(task, reason))
	end.Metadata = map[string]any{metaTerminal: "timeout", metaExecutionBegun: true}
	retry := 25 * time.Millisecond
	for s.life.Err() == nil {
		ctx, cancel := context.WithTimeout(s.life, 10*time.Second)
		_, err := s.updates.UpdateTask(ctx, end)
		cancel()
		switch {
		case err == nil, errors.Is(err, a2a.ErrUnsupportedOperation):
			// A concurrent answer or cancellation may have ended the task.
			return
		case errors.Is(err, a2asrv.ErrTaskActive):
			// A refused answer still publishes input-required. Wait for
			// its execution to finish so the timeout reaches the store.
			select {
			case <-s.life.Done():
				return
			case <-time.After(retry):
				retry = min(2*retry, time.Second)
			}
		default:
			log.Printf("agent %s: publish the timeout of task %s: %v", s.name, task.TaskID, err)
			return
		}
	}
}

func (s *agentService) answerTimeoutReason() string {
	return fmt.Sprintf("timed out: no answer came within %s of the question", s.answerWait)
}

// wait keeps a task whose agent asked a question until the answer comes,
// or says why it cannot wait.
func (s *agentService) wait(id a2a.TaskID, w waitingTask) (why string) {
	switch {
	case w.session == "":
		return "the agent asked a question, but its runtime cannot carry on the task"
	case w.questions > maxQuestions:
		return fmt.Sprintf("the agent asked more than %d questions", maxQuestions)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.life.Err() != nil {
		return "the node is shutting down"
	}
	for other, o := range s.waiting {
		if time.Since(o.since) >= s.answerWait {
			s.expireLocked(other)
		}
	}
	if len(s.waiting) >= maxWaiting {
		return fmt.Sprintf("the agent asked a question, but %d tasks already wait for answers", maxWaiting)
	}
	// A task whose answer does not come in time ends then, not only when
	// someone next looks.
	wp := &w
	wp.timer = time.AfterFunc(time.Until(w.since.Add(s.answerWait)), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.waiting[id] == wp {
			s.expireLocked(id)
		}
	})
	s.waiting[id] = wp
	return ""
}

// admit takes the running slot and, for an answer, its waiting session in
// the same critical section as expiry. A refused answer returns its pending
// question if the caller can still retry.
func (s *agentService) admit(execCtx *a2asrv.ExecutorContext, from taskSender, fromErr error, request string, cancel context.CancelCauseFunc) (run *agentRun, prior *waitingTask, reason string, metadata map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A drained agent finishes its tasks, answers to its questions
	// included, but takes no new one. The drain is read in the same
	// critical section that takes the running slot, so once agent drain
	// saved it and saw the agent idle, no new task can still start.
	var drained *drainMark
	if execCtx.StoredTask == nil {
		drained = s.drain()
	}
	if execCtx.StoredTask != nil {
		prior = s.waiting[execCtx.TaskID]
		if prior == nil || time.Since(prior.since) >= s.answerWait {
			s.expireLocked(execCtx.TaskID)
			return nil, nil, s.answerTimeoutReason(), nil
		}
	}
	sender := from.String()
	switch {
	case s.life.Err() != nil:
		return nil, prior, "refused: the node is shutting down", nil
	case fromErr != nil:
		return nil, prior, "refused: " + fromErr.Error(), nil
	case prior != nil && prior.sender != sender:
		return nil, prior, "refused: only the task's sender can answer", nil
	case from.depth > maxDelegationDepth:
		return nil, prior, fmt.Sprintf("refused: delegation depth above %d", maxDelegationDepth), nil
	case strings.TrimSpace(request) == "":
		return nil, prior, "refused: the request has no text", nil
	}
	if first, ok := s.delivered.first(sender, execCtx.Message.ID); ok {
		return nil, prior, fmt.Sprintf("refused: message %s was already delivered as task %s", execCtx.Message.ID, first), map[string]any{metaDuplicateOf: string(first)}
	}
	if drained != nil {
		return nil, prior, "refused: " + s.name.String() + " " + drained.Text() + "; it takes no new tasks", nil
	}
	if s.current != nil {
		return nil, prior, "refused: a task is already running; try again when it ends", nil
	}
	// Without the record a retry after a restart could run the task again,
	// so a task that cannot be recorded does not start.
	if err := s.delivered.record(sender, execCtx.Message.ID, execCtx.TaskID); err != nil {
		log.Printf("agent service: record delivery: %v", err)
		return nil, prior, "refused: the node could not record the request", nil
	}
	if prior != nil {
		prior.timer.Stop()
		delete(s.waiting, execCtx.TaskID)
	}
	// An answer continues a task that already executed its previous turn.
	s.current = &agentRun{taskID: execCtx.TaskID, cancel: cancel, begun: prior != nil, started: time.Now()}
	return s.current, prior, "", nil
}

// agentActivity is what an agent is doing now, as `agentiscaled agent
// list` shows it.
type agentActivity struct {
	Name string `json:"name"`
	// State is busy while a turn of a task runs and idle otherwise.
	State string `json:"state"`
	// Since is when the running turn began.
	Since *time.Time `json:"since,omitempty"`
	// Waiting counts the tasks whose agent asked the caller a question and
	// waits for the answer, and WaitingSince is when the longest waiting
	// one asked.
	Waiting      int        `json:"waiting,omitempty"`
	WaitingSince *time.Time `json:"waiting_since,omitempty"`
	// Drain, when set, is the owner's drain: the agent finishes its tasks
	// and takes no new one.
	Drain *drainMark `json:"drain,omitempty"`
	// Runtime is what runs the agent's tasks (codex, claude, acp).
	Runtime string `json:"runtime,omitempty"`
}

const (
	activityIdle = "idle"
	activityBusy = "busy"
)

// activity says what the agent is doing now.
func (s *agentService) activity() agentActivity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activityLocked(time.Now())
}

// agentsActivity is what all agents of the node do at one moment: it holds
// every agent's lock while it reads them, taken in the agents' fixed order,
// so agent drain -wait never sees one agent drained and idle and another
// at a different moment.
func agentsActivity(agents []*agentService) []agentActivity {
	for _, s := range agents {
		s.mu.Lock()
	}
	defer func() {
		for _, s := range agents {
			s.mu.Unlock()
		}
	}()
	// One clock reading for every agent, taken before any drain is read:
	// a question that expires while the agents are read is judged at the
	// same moment for all of them.
	now := time.Now()
	out := make([]agentActivity, 0, len(agents))
	for _, s := range agents {
		out = append(out, s.activityLocked(now))
	}
	return out
}

// activityLocked is activity with s.mu held, as of now. The drain is read
// with the task state, under the lock that admission takes, so an answer is
// one moment: it never pairs a drain that has since been lifted with work
// that ended after it. A waiting question counts until now reaches its
// expiry, whatever the clock says by the time it is read.
func (s *agentService) activityLocked(now time.Time) agentActivity {
	a := agentActivity{Name: s.name.String(), State: activityIdle, Drain: s.drain(), Runtime: s.runtimeName}
	if s.current != nil {
		a.State = activityBusy
		started := s.current.started
		a.Since = &started
	}
	for _, w := range s.waiting {
		// The timer may still be waiting for the lock.
		if now.Sub(w.since) >= s.answerWait {
			continue
		}
		a.Waiting++
		if a.WaitingSince == nil || w.since.Before(*a.WaitingSince) {
			since := w.since
			a.WaitingSince = &since
		}
	}
	return a
}

// serviceStatus is the activity as the node's catalog reports carry it,
// which the owner's console shows. It carries no task content.
func (a agentActivity) serviceStatus() *api.ServiceStatus {
	st := &api.ServiceStatus{State: api.ServiceState_SERVICE_STATE_IDLE, InputRequired: uint32(a.Waiting)}
	if a.State == activityBusy {
		st.State = api.ServiceState_SERVICE_STATE_BUSY
	}
	if a.Since != nil {
		st.Since = timestamppb.New(*a.Since)
	}
	if a.WaitingSince != nil {
		st.InputRequiredSince = timestamppb.New(*a.WaitingSince)
	}
	return st
}

// activityEvery is how often the node looks at what its agents are doing
// for its catalog reports; the mesh node reports a change early on its
// own, at most once per a few seconds.
var activityEvery = time.Second

// serviceMetadata is the agent's service metadata, which callers see when
// they discover it and the owner's console with its catalog: its runtime
// and its drain. A runtime that is not a valid one is left out.
func (a agentActivity) serviceMetadata() map[string]string {
	md := map[string]string{}
	if caller.ValidRuntime(a.Runtime) {
		md[caller.RuntimeMetadataKey] = a.Runtime
	}
	if a.Drain != nil {
		md[caller.DrainMetadataKey] = a.Drain.Metadata()
	}
	return md
}

// reportActivity hands each agent's activity to setStatus, and its
// metadata to setMetadata when it changes, until ctx ends.
func reportActivity(ctx context.Context, agents []*agentService, setStatus func(name string, status *api.ServiceStatus), setMetadata func(name string, metadata map[string]string)) {
	ticker := time.NewTicker(activityEvery)
	defer ticker.Stop()
	reported := map[caller.AgentName]map[string]string{}
	for {
		for _, s := range agents {
			a := s.activity()
			setStatus(s.name.String(), a.serviceStatus())
			md := a.serviceMetadata()
			if last, ok := reported[s.name]; !ok || !maps.Equal(last, md) {
				setMetadata(s.name.String(), md)
				reported[s.name] = md
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *agentService) finish(run *agentRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == run {
		s.current = nil
	}
}

func (s *agentService) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		s.mu.Lock()
		// Expiry may have removed the waiting entry while the store still
		// holds the question that proves the task already ran.
		begun := execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired
		if run := s.current; run != nil && run.taskID == execCtx.TaskID {
			begun = begun || run.begun
			run.cancel(errCancelledByCaller)
		}
		// A task waiting for an answer has run already.
		if _, ok := s.waiting[execCtx.TaskID]; ok {
			begun = true
			s.endTaskLocked(execCtx.TaskID)
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
	// The caller authenticator tells the server's own updates (lapse) apart,
	// which reach every task.
	store := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{CallerAuthenticator: a2asrv.NewTaskStoreCallerAuthenticator()})
	requests := a2asrv.NewHandler(s, a2asrv.WithTaskStore(store),
		a2asrv.WithCallInterceptors(peerIdentity{tasks: store}))
	s.updates, _ = requests.(a2asrv.TaskUpdater)
	rpc := a2asrv.NewJSONRPCHandler(boundedStreams{requests})
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && (r.URL.Path == "/.well-known/agent-card.json" || r.URL.Path == "/") {
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

// serveAgent returns the authenticated target URL and a channel that closes
// once the server and its runtime have stopped.
func serveAgent(ctx context.Context, s *agentService, name string) (string, <-chan struct{}, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		ln.Close()
		return "", nil, err
	}
	token := hex.EncodeToString(b)
	s.life = ctx
	srv := &http.Server{Handler: s.handler(name, token), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("agent service: %v", err)
		}
	}()
	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		s.shutdown()
		close(stopped)
	}()
	return fmt.Sprintf("http://:%s@%s", token, ln.Addr()), stopped, nil
}

func messageText(m *a2a.Message) string { return caller.MessageText(m) }

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
