package node

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// The agentiscale runtime contract for agents that speak the Agent Client
// Protocol (docs/add-agentiscale-support.md): ACP over stdio, with
// agentiscale's conventions in _meta.agentiscale and the environment.
const (
	// acpProtocolVersion is the ACP version the node speaks.
	acpProtocolVersion = 1
	// acpContract is the version of agentiscale's conventions.
	acpContract = 1
	// acpCancelGrace is how long a cancelled turn may take to end before
	// the node stops the agent's processes.
	acpCancelGrace = 5 * time.Second
	// acpProbeTimeout bounds the initialize exchange of a probe.
	acpProbeTimeout = 30 * time.Second
	// acpMaxLine bounds one ACP message.
	acpMaxLine = 16 << 20
	// acpMaxResult bounds a turn's result text.
	acpMaxResult = 1 << 20
	// Named messages can resume later in the same prompt. Bound the
	// identifiers and text retained while collecting their chunks.
	acpMaxMessageIDs = 1024
	acpMaxNamedBytes = 16 << 20
)

// acpStopReasons are the stop reasons of ACP besides end_turn, which a
// failure may name.
var acpStopReasons = []string{"max_tokens", "max_turn_requests", "refusal", "cancelled"}

// The policies an agent can declare that it enforces.
const (
	acpPolicyConfigDir       = "config-dir"
	acpPolicyDirInstructions = "dir-instructions"
	acpPolicyDirSkills       = "dir-skills"
)

// ACP's error code for a request that needs the agent signed in.
const acpAuthRequired = -32000

// acpRuntime runs each turn of a task with an agent that speaks ACP over
// stdio, under agentiscale's runtime contract. The node is the ACP client
// and starts the agent's command for each turn.
type acpRuntime struct {
	// command and args start the agent; command is an absolute path.
	command string
	args    []string
	// config is the agent's own configuration directory, set without
	// the owner's configuration (AGENTISCALE_CONFIG_DIR).
	config string
	policy agentPolicy
	def    *agentDefinition
	// info is what the agent declared when the runtime was made.
	info acpAgent
	// runtime check observes which process received a prompt, whether it
	// loaded the session, and the reason it stopped.
	prompted func(*acpSession, bool)
	stopped  func(string)
	// sessions holds every process until it is reaped. A running turn
	// supplies its cancellation function; an idle process needs only EOF.
	sessionsMu sync.Mutex
	sessions   map[*acpSession]context.CancelFunc
	kept       map[string]*keptSession
	draining   bool
	drain      sync.Once
	// stderr, if set, takes what the agent wrote to stderr in each turn,
	// once its run ends; tests use it.
	stderr func(string)
	// bridge is the agentiscaled executable that serves an agent without
	// HTTP MCP servers the network on stdio (networkBridgeCommand); empty
	// when it cannot be found.
	bridge string
}

func (r *acpRuntime) setDefinition(d *agentDefinition) { r.def = d }

// acpAgent is what an agent declares in its answer to initialize.
type acpAgent struct {
	ProtocolVersion int `json:"protocolVersion"`
	AgentInfo       struct {
		Name    string `json:"name"`
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"agentInfo"`
	AgentCapabilities struct {
		LoadSession     bool `json:"loadSession"`
		MCPCapabilities struct {
			HTTP bool `json:"http"`
		} `json:"mcpCapabilities"`
	} `json:"agentCapabilities"`
	Meta struct {
		Agentiscale *acpConventions `json:"agentiscale"`
	} `json:"_meta"`
}

// acpConventions is _meta.agentiscale of an agent's answer to initialize.
type acpConventions struct {
	Contract int      `json:"contract"`
	Policies []string `json:"policies"`
	// Status says the agent ends each turn with
	// _meta.agentiscale.status, so the prompt need not ask for an answer
	// in turnSchema.
	Status bool `json:"status"`
	// ResultText enables a final text alongside status in the prompt response.
	ResultText bool `json:"resultText"`
	// Login is the command that signs the agent in.
	Login []string `json:"login"`
}

func (a acpAgent) conventions() acpConventions {
	if a.Meta.Agentiscale == nil {
		return acpConventions{}
	}
	return *a.Meta.Agentiscale
}

// name is how the node calls the agent in messages.
func (a acpAgent) name(command string) string {
	if a.AgentInfo.Title != "" && plainName.MatchString(strings.ReplaceAll(a.AgentInfo.Title, " ", "-")) {
		return a.AgentInfo.Title
	}
	if plainName.MatchString(a.AgentInfo.Name) {
		return a.AgentInfo.Name
	}
	return filepath.Base(command)
}

// splitACPCommand splits the command line an owner gives for an ACP agent
// into its program and arguments, at spaces: neither can hold one. The node
// keeps them apart from then on, so that the program's resolved path may.
func splitACPCommand(line string) (string, []string, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", nil, errors.New("the acp runtime needs the agent's command: -command \"my-agent acp\"")
	}
	return fields[0], fields[1:], nil
}

// runtimeDefaultName is the name of an agent of runtime rt, run with
// the program command, that has no name of its own: an ACP agent's
// program's base name, else the runtime's.
func runtimeDefaultName(rt, command string) string {
	if rt == "acp" {
		if command == "" {
			return ""
		}
		return strings.ToLower(filepath.Base(command))
	}
	return runtimes[rt].defaultName
}

// acpConfigDir selects the service's private configuration. An unnamed
// runtime check uses a separate profile keyed by its resolved command,
// arguments and explicit working directory. A temporary check directory
// changes on every run and is omitted from that key.
func (a agentConfig) acpConfigDir(dataDir, checkDir string) (string, error) {
	if !a.OwnConfig {
		return "", nil
	}
	if a.Name == "" {
		key := strings.Join(append([]string{a.Command, checkDir}, a.Args...), "\x00")
		return filepath.Abs(filepath.Join(dataDir, "acp-checks", fmt.Sprintf("%x", sha256.Sum256([]byte(key)))))
	}
	return filepath.Abs(agentProfileDir(dataDir, a.Name))
}

// newACPRuntime probes the offered command in its configured working directory.
func newACPRuntime(ctx context.Context, a agentConfig, dataDir, checkDir string) (runtime, error) {
	if a.Command == "" {
		return nil, errors.New("the acp runtime needs the agent's command: -command \"my-agent acp\"")
	}
	path, err := exec.LookPath(a.Command)
	if err != nil {
		return nil, fmt.Errorf("the agent command %q was not found: %w", a.Command, err)
	}
	// Once the child switches to its working directory, a relative
	// executable path could identify a different program.
	a.Command, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	config, err := a.acpConfigDir(dataDir, checkDir)
	if err != nil {
		return nil, err
	}
	if config != "" {
		if err := os.MkdirAll(config, 0o700); err != nil {
			return nil, err
		}
	}
	r := &acpRuntime{command: a.Command, args: a.Args, config: config, policy: a.policy()}
	if bridge, err := bridgeExecutable(); err == nil {
		r.bridge = bridge
	}
	// The agent says what it supports before it runs any task, so an
	// owner's policy that it does not enforce stops agent add and the
	// node's start, not a task.
	ctx, cancel := context.WithTimeout(ctx, acpProbeTimeout)
	defer cancel()
	s, err := r.start(ctx, a.Dir, nil)
	if err != nil {
		return nil, err
	}
	defer s.close()
	if r.info, err = s.initialize(ctx); err != nil {
		return nil, err
	}
	if err := r.checkPolicy(); err != nil {
		return nil, err
	}
	return r, nil
}

// checkPolicy refuses an owner's policy that the agent does not declare it
// enforces.
func (r *acpRuntime) checkPolicy() error {
	c := r.info.conventions()
	var missing []string
	need := func(on bool, policy, flag string) {
		if on && !slices.Contains(c.Policies, policy) {
			missing = append(missing, fmt.Sprintf("%s (%s)", flag, policy))
		}
	}
	need(!r.policy.OwnerConfig, acpPolicyConfigDir, "-owner-config=false")
	need(!r.policy.DirInstructions, acpPolicyDirInstructions, "-dir-instructions=false")
	need(!r.policy.DirSkills, acpPolicyDirSkills, "-dir-skills=false")
	if len(missing) > 0 {
		return fmt.Errorf("%s does not say it enforces %s: run it without, or ask its vendor to support the agentiscale runtime contract", r.info.name(r.command), strings.Join(missing, ", "))
	}
	return nil
}

// env is the environment of the agent's process: the node's with the
// owner's configuration, else only what a process needs (processEnv), and
// the contract's variables.
func (r *acpRuntime) env() []string {
	var env []string
	if r.policy.OwnerConfig {
		env = withoutEnv(os.Environ(), []string{"AGENTISCALE_ENV", "AGENTISCALE_CONTRACT", "AGENTISCALE_CONFIG_DIR", "AGENTISCALE_DIR_INSTRUCTIONS", "AGENTISCALE_DIR_SKILLS"})
	} else {
		env = processEnv(os.Environ())
		env = append(env, "AGENTISCALE_CONFIG_DIR="+r.config)
	}
	env = append(env, "AGENTISCALE_ENV=1", "AGENTISCALE_CONTRACT="+strconv.Itoa(acpContract))
	if !r.policy.DirInstructions {
		env = append(env, "AGENTISCALE_DIR_INSTRUCTIONS=0")
	}
	if !r.policy.DirSkills {
		env = append(env, "AGENTISCALE_DIR_SKILLS=0")
	}
	return env
}

func (r *acpRuntime) Describe(card *a2a.AgentCard) {
	name := r.info.name(r.command)
	card.Description = fmt.Sprintf("A %s agent on this node that works in its configured directory under local runtime permissions. Send one text message with the task; it answers with the result, or asks a question (input-required) that you answer with a message on the same task.", name)
}

// Run runs one turn of a task: a new ACP session for a new task, or the
// task's session loaded again for the caller's answer to the agent's
// question.
func (r *acpRuntime) Run(ctx context.Context, dir string, req taskRequest) (turnResult, error) {
	// An answer goes to the agent's process that asked, if it is still
	// kept (keep); else to a new one that loads the session.
	s := r.take(req.Task, req.Session)
	resumed := s != nil
	if !resumed {
		var err error
		if s, err = r.start(ctx, dir, req.progress); err != nil {
			return turnResult{}, err
		}
	} else {
		s.setProgress(req.progress)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.sessionsMu.Lock()
	if r.draining {
		r.sessionsMu.Unlock()
		s.close()
		return turnResult{}, context.Canceled
	}
	// Arm escalation before exposing this turn to shutdown. A pending
	// permission reply can block the protocol lock even before the prompt.
	stopWatch := s.watchCancel(ctx)
	defer stopWatch()
	r.sessions[s] = cancel
	r.sessionsMu.Unlock()
	kept := false
	defer func() {
		if kept {
			return
		}
		s.close()
		if r.stderr != nil {
			r.stderr(s.stderr.String())
		}
	}()
	// A process keeps what it declared at its own initialize, also when
	// the agent has been updated since.
	if !resumed {
		info, err := s.initialize(ctx)
		if err != nil {
			return turnResult{}, r.failure(ctx, s, err)
		}
		s.info = info
		// The agent may have changed since the node started.
		r.info = info
		if err := r.checkPolicy(); err != nil {
			return turnResult{}, err
		}
	}
	info := s.info
	conventions := info.conventions()

	// A kept process has the servers of its session/new, whose bearer
	// stays the task's over its turns: each turn opens the network to it
	// with the turn's own token (taskNetwork).
	servers := []any{}
	if req.Network != nil {
		server, err := r.networkServer(req.Network, info)
		if err != nil {
			return turnResult{}, err
		}
		if server != nil {
			servers = append(servers, server)
		}
	}
	session := req.Session
	switch {
	case resumed:
		// The session is open on this connection: the answer is its
		// next prompt.
	case session == "":
		var created struct {
			SessionID string `json:"sessionId"`
		}
		if err := s.call(ctx, "session/new", map[string]any{"cwd": dir, "mcpServers": servers}, &created); err != nil {
			return turnResult{}, r.failure(ctx, s, err)
		}
		if created.SessionID == "" {
			return turnResult{}, errors.New("the agent's session/new has no sessionId")
		}
		session = created.SessionID
	case !info.AgentCapabilities.LoadSession:
		return turnResult{}, errors.New("the agent's process that asked the question has ended, and the agent cannot load a session to take the answer")
	default:
		if err := s.call(ctx, "session/load", map[string]any{"sessionId": session, "cwd": dir, "mcpServers": servers}, nil); err != nil {
			return turnResult{}, r.failure(ctx, s, err)
		}
	}
	// What the agent sends from here on is this turn's: session/load
	// replays the session's history before it answers.
	s.begin(session)
	var prompted struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			Agentiscale struct {
				Status string          `json:"status"`
				Text   json.RawMessage `json:"text"`
			} `json:"agentiscale"`
		} `json:"_meta"`
	}
	// The prompt goes out only while the turn may run, and a cancel only
	// after it (sendPrompt, cancel).
	var written chan bool
	if r.prompted != nil {
		written = make(chan bool, 1)
	}
	ch, err := s.sendPrompt(ctx, map[string]any{
		"sessionId": session,
		"prompt":    []map[string]string{{"type": "text", "text": r.prompt(req, conventions.Status)}},
	}, written)
	if err != nil {
		if ctx.Err() != nil {
			return turnResult{}, ctx.Err()
		}
		return turnResult{}, r.failure(ctx, s, err)
	}
	cancelSent := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(cancelSent)
		s.cancel(session)
	})
	observe := r.prompted
	loaded := !resumed && req.Session != ""
	if written != nil {
		select {
		case ok := <-written:
			if ok {
				observe(s, loaded)
				observe = nil
			}
		case <-s.done:
		case <-s.broken:
		case <-s.dead:
		case <-ctx.Done():
		}
	}
	err = s.await(context.Background(), "session/prompt", ch, &prompted)
	if observe != nil && err == nil && ctx.Err() == nil {
		// A reply also confirms delivery if EOF raced the writer's acknowledgement.
		observe(s, loaded)
	}
	if !stop() {
		<-cancelSent
	}
	if ctx.Err() != nil {
		if r.stopped != nil && err == nil {
			r.stopped(prompted.StopReason)
		}
		return turnResult{}, ctx.Err()
	}
	if err != nil {
		return turnResult{}, r.failure(ctx, s, err)
	}
	if r.stopped != nil {
		r.stopped(prompted.StopReason)
	}
	if prompted.StopReason != "end_turn" {
		reason := "an unknown reason"
		if slices.Contains(acpStopReasons, prompted.StopReason) {
			reason = prompted.StopReason
		}
		s.logFailure(r, fmt.Errorf("stop reason %q", prompted.StopReason))
		return turnResult{}, fmt.Errorf("the agent stopped the turn: %s", reason)
	}
	answer := prompted.Meta.Agentiscale
	var finalText json.RawMessage
	if conventions.ResultText && answer.Status != "" {
		finalText = answer.Text
	}
	text, err := s.result(finalText)
	if err != nil {
		return turnResult{}, err
	}
	var result turnResult
	if answer.Status == "" && text == "" {
		// No status, no text and no message: the agent gave up or was
		// stopped by a refused step, and has nothing for the caller.
		s.logFailure(r, errNoAnswer)
		return turnResult{}, errNoAnswer
	}
	if answer.Status != "" {
		result, err = turnAnswer{Status: answer.Status, Text: text}.result(session)
	} else {
		result, err = parseTurn(text, session)
	}
	if err != nil {
		// What the agent sent may hold anything: the caller gets a
		// fixed reason, the node's log the agent's.
		s.logFailure(r, err)
		return turnResult{}, errors.New("the agent ended the turn with an answer that is not done or needs_input, or an empty question")
	}
	if result.Question {
		// End this turn's watchdog before transferring the process to
		// the next turn. An already cancelled turn cannot keep it.
		stopWatch()
		if err := ctx.Err(); err != nil {
			return turnResult{}, err
		}
		// The process waits for the answer, with the session open.
		if kept = r.keep(req.Task, result.Session, s); !kept && !info.AgentCapabilities.LoadSession {
			if s.exited() {
				return turnResult{}, errors.New("the agent's process that asked the question has ended, and the agent cannot load a session to take the answer")
			}
			return turnResult{}, fmt.Errorf("the agent asked a question, but %d of its processes already wait for answers and it cannot load a session to take one later", maxWaiting)
		}
	}
	return result, nil
}

// keptSession is an agent's process that waits, with its session open,
// for the answer to the question its last turn asked.
type keptSession struct {
	s       *acpSession
	session string
}

// keep retains the process for the next turn of this task. The service
// releases it when the question expires or the task ends. Processes can
// choose identical session IDs, so the node's task ID identifies the entry.
func (r *acpRuntime) keep(task, session string, s *acpSession) bool {
	if task == "" {
		return false
	}
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	if r.draining || s.exited() || len(r.kept) >= maxWaiting || r.kept[task] != nil {
		return false
	}
	if r.kept == nil {
		r.kept = map[string]*keptSession{}
	}
	s.setProgress(nil)
	r.kept[task] = &keptSession{s: s, session: session}
	r.sessions[s] = nil
	return true
}

// take takes the process kept for task's question in session, nil when
// none is or it has ended.
func (r *acpRuntime) take(task, session string) *acpSession {
	if task == "" || session == "" {
		return nil
	}
	r.sessionsMu.Lock()
	k := r.kept[task]
	if k == nil || k.session != session {
		r.sessionsMu.Unlock()
		return nil
	}
	delete(r.kept, task)
	r.sessionsMu.Unlock()
	select {
	case <-k.s.done:
	default:
		if !k.s.exited() {
			return k.s
		}
	}
	k.s.close()
	return nil
}

// release stops the process kept for task, if one is: the task ended
// without an answer.
func (r *acpRuntime) release(task string) {
	r.sessionsMu.Lock()
	k := r.kept[task]
	delete(r.kept, task)
	r.sessionsMu.Unlock()
	if k != nil {
		k.s.close()
	}
}

// releaseAll cancels active turns, closes idle sessions and joins every
// process, including those already being released.
func (r *acpRuntime) releaseAll() {
	r.drain.Do(func() {
		r.sessionsMu.Lock()
		r.draining = true
		sessions := r.sessions
		r.sessions = nil
		r.kept = nil
		r.sessionsMu.Unlock()
		for s, cancel := range sessions {
			if cancel != nil {
				cancel()
			} else {
				go s.close()
			}
		}
		for s := range sessions {
			<-s.closed
		}
	})
}

// acpSignInRequired carries a local remedy for the node's owner. Only Error
// is published to a task's caller.
type acpSignInRequired struct {
	login  []string
	config string
}

func (*acpSignInRequired) Error() string { return "the agent requires the node owner to sign in" }

func (e *acpSignInRequired) ownerMessage() string {
	args := make([]string, 0, len(e.login)+1)
	if e.config != "" {
		args = append(args, "AGENTISCALE_CONFIG_DIR="+shellQuote(e.config))
	}
	if len(e.login) == 0 {
		text := "sign in on this node using the agent's sign-in flow"
		if len(args) > 0 {
			text += " with " + args[0]
		}
		return text
	}
	for _, arg := range e.login {
		args = append(args, shellQuote(arg))
	}
	return "sign in on this node by running\n  " + strings.Join(args, " ")
}

// failure records local diagnostics and returns the reason that the task
// service may publish to its caller.
func (r *acpRuntime) failure(ctx context.Context, s *acpSession, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var rpcErr *acpError
	if errors.As(err, &rpcErr) && rpcErr.Code == acpAuthRequired {
		signIn := &acpSignInRequired{login: s.info.conventions().Login, config: r.config}
		s.logFailure(r, fmt.Errorf("%w; %s", err, signIn.ownerMessage()))
		return signIn
	}
	s.logFailure(r, err)
	name := s.info.name(r.command)
	if rpcErr == nil {
		return fmt.Errorf("%s failed: %s", name, failureReason(err))
	}
	return fmt.Errorf("%s failed: %s answered with error %d; the node's log has its message", name, rpcErr.Method, rpcErr.Code)
}

// failureReason is a node-side error as the caller may read it.
func failureReason(err error) string {
	var e *acpExit
	if errors.As(err, &e) {
		return e.Error()
	}
	return "see the node's log"
}

// logFailure writes why a turn of the agent failed, with the last lines of
// its stderr, to the node's log.
func (s *acpSession) logFailure(r *acpRuntime, err error) {
	log.Printf("acp agent %s: %v; stderr: %s", r.command, err, failureLines(s.stderr.String(), 5))
}

// acpTurnInstructions are the turn's instructions for an agent that ends
// its turns with _meta.agentiscale.status.
const acpTurnInstructions = `When you are done, answer with the result.
If you cannot go on without information that only the sender can give, ask one
short question instead; the sender's answer comes in a later message. Ask only when
you must, never for permission or confirmation, and never for anything your
configuration or the directory can tell you.`

// prompt is the turn's prompt: the task or the answer, framed as
// untrusted (framedPrompt), with the definition's instructions in front of
// a new task. Every prompt names how the turn ends: the agent's own status
// when it declares one, else an answer in turnSchema.
func (r *acpRuntime) prompt(req taskRequest, status bool) string {
	instructions := turnInstructions + "\nAnswer with only a JSON object of this schema: " + turnSchema
	if status {
		instructions = acpTurnInstructions
	}
	p := framedPrompt(req, instructions)
	if req.Session == "" && r.def != nil {
		p = "<role>\n" + r.def.Instructions + "\n</role>\n\n" + p
	}
	return p
}

// start starts the agent's command in dir with stdin and stdout as the ACP
// connection. progress, if set, takes the turn's steps.
func (r *acpRuntime) start(ctx context.Context, dir string, progress func(string)) (*acpSession, error) {
	// Register the new process before shutdown can take its snapshot.
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	if r.draining {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(r.command, r.args...)
	cmd.Dir = dir
	cmd.Env = r.env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s := &acpSession{
		owner: r, cmd: cmd, progress: progress,
		pending:      make(map[int64]chan acpResponse),
		out:          make(chan acpWrite),
		dead:         make(chan struct{}),
		broken:       make(chan struct{}),
		done:         make(chan struct{}),
		exitObserved: make(chan struct{}),
		closed:       make(chan struct{}),
	}
	cmd.Stderr = &s.stderr
	// A detached helper can retain stderr after the agent exits. Give
	// os/exec a bounded interval to collect the last diagnostic bytes.
	cmd.WaitDelay = 100 * time.Millisecond
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", r.command, err)
	}
	s.stdin = stdin
	if r.sessions == nil {
		r.sessions = make(map[*acpSession]context.CancelFunc)
	}
	r.sessions[s] = nil
	go s.observeExit()
	go s.read(stdout)
	go s.write()
	return s, nil
}

// acpSession is one run of the agent's command: the node's ACP client
// connection to it.
type acpSession struct {
	owner     *acpRuntime
	closeOnce sync.Once
	closed    chan struct{}
	// A helper may inherit stdout or stderr and keep it open after the
	// agent exits. processExited is read only after exitObserved closes.
	exitObserved  chan struct{}
	processExited bool
	// info is what the agent declared at this process's initialize.
	info     acpAgent
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stderr   tailBuffer
	progress func(string)

	// out takes the messages to write to stdin, which one goroutine
	// writes (write), so that no sender waits on a full pipe but through
	// a select it can leave.
	out chan acpWrite
	// dead is closed when the node stops the agent's processes.
	dead     chan struct{}
	killOnce sync.Once
	// broken is closed when a write to stdin fails, which writeErr says.
	broken   chan struct{}
	writeErr error
	// order puts the prompt, the cancel and the answers to the agent's
	// requests in order: a cancel goes only after a prompt that went out,
	// and an answer after a cancel cancels.
	order      sync.Mutex
	promptSent bool
	cancelled  bool
	mu         sync.Mutex
	nextID     int64
	pending    map[int64]chan acpResponse
	// Updates belong to the turn only while its prompt request is pending.
	session  string
	promptID int64
	messages acpTurnMessages
	// readErr is why the connection ended.
	readErr error
	done    chan struct{}
}

type acpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpError       `json:"error,omitempty"`
}

type acpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Method is the request the error answers.
	Method string `json:"-"`
}

func (e *acpError) Error() string {
	return fmt.Sprintf("%s: agent error %d: %s", e.Method, e.Code, e.Message)
}

// acpExit is the agent's connection ending during a request.
type acpExit struct{ method string }

func (e *acpExit) Error() string { return "the agent exited during " + e.method }

type acpResponse struct {
	result json.RawMessage
	err    error
}

func (s *acpSession) initialize(ctx context.Context) (acpAgent, error) {
	var a acpAgent
	err := s.call(ctx, "initialize", map[string]any{
		"protocolVersion": acpProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]string{"name": "agentiscaled", "version": ReleaseVersion},
		"_meta":      map[string]any{"agentiscale": map[string]bool{"resultText": true}},
	}, &a)
	if err != nil {
		return a, err
	}
	if a.ProtocolVersion != acpProtocolVersion {
		return a, fmt.Errorf("the agent speaks ACP version %d; agentiscaled speaks %d", a.ProtocolVersion, acpProtocolVersion)
	}
	if c := a.conventions(); c.Contract > acpContract {
		return a, fmt.Errorf("the agent needs version %d of the agentiscale runtime contract; this agentiscaled has %d: update agentiscaled", c.Contract, acpContract)
	}
	return a, nil
}

// call sends a request and decodes its result into out, if set.
func (s *acpSession) call(ctx context.Context, method string, params any, out any) error {
	ch, err := s.request(ctx, method, params, nil)
	if err != nil {
		return err
	}
	return s.await(ctx, method, ch, out)
}

// request sends a request; its response comes on the channel.
func (s *acpSession) request(ctx context.Context, method string, params any, written chan<- bool) (chan acpResponse, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan acpResponse, 1)
	s.pending[id] = ch
	if method == "session/prompt" {
		s.promptID = id
	}
	s.mu.Unlock()
	if err := s.send(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}, written); err != nil {
		return nil, err
	}
	return ch, nil
}

// sendPrompt sends session/prompt unless ctx has ended.
func (s *acpSession) sendPrompt(ctx context.Context, params any, written chan<- bool) (chan acpResponse, error) {
	s.order.Lock()
	defer s.order.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ch, err := s.request(ctx, "session/prompt", params, written)
	s.promptSent = err == nil
	return ch, err
}

// await waits for the response on ch and decodes its result into out, if
// set.
func (s *acpSession) await(ctx context.Context, method string, ch chan acpResponse, out any) error {
	var resp acpResponse
	var connErr error
	select {
	case resp = <-ch:
	case <-s.done:
		connErr = s.ended(method)
	case <-s.dead:
		connErr = &acpExit{method: method}
	case <-s.broken:
		connErr = s.brokenErr(method)
	case <-ctx.Done():
		return ctx.Err()
	}
	if connErr != nil {
		// read queues complete replies before it reports EOF. A subsequent
		// shutdown or write error must also preserve an already queued reply.
		select {
		case resp = <-ch:
		default:
			return connErr
		}
	}
	if resp.err != nil {
		if e, ok := resp.err.(*acpError); ok {
			e.Method = method
		}
		return resp.err
	}
	if out != nil && len(resp.result) > 0 && string(resp.result) != "null" {
		if err := json.Unmarshal(resp.result, out); err != nil {
			return fmt.Errorf("the agent's %s result: %w", method, err)
		}
	}
	return nil
}

// brokenErr is the failed write to stdin, during method.
func (s *acpSession) brokenErr(method string) error {
	s.mu.Lock()
	err := s.writeErr
	s.mu.Unlock()
	log.Printf("acp agent %s: writing to its stdin failed during %s: %v", s.cmd.Path, method, err)
	return &acpExit{method: method}
}

func (s *acpSession) ended(method string) error {
	s.mu.Lock()
	readErr := s.readErr
	s.mu.Unlock()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		log.Printf("acp agent %s: connection broke during %s: %v", s.cmd.Path, method, readErr)
	}
	return &acpExit{method: method}
}

// acpWrite is one message for the writer, which says on ack whether it
// writes it: not when ctx has ended by the time the writer takes it.
type acpWrite struct {
	b   []byte
	ctx context.Context
	ack chan error
	// written optionally reports whether the complete message reached stdin.
	written chan<- bool
}

// send hands msg to the writer for the agent's stdin, which writes it
// unless ctx has ended when it takes it. It gives up when ctx ends or the
// agent's run does, however full the pipe is. A nil error means the
// message is the next the writer writes.
func (s *acpSession) send(ctx context.Context, msg any, written chan<- bool) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	w := acpWrite{b: append(b, '\n'), ctx: ctx, ack: make(chan error, 1), written: written}
	select {
	case s.out <- w:
		// The writer acks at once, before it writes.
		return <-w.ack
	case <-s.done:
		return &acpExit{method: "a write"}
	case <-s.dead:
		return &acpExit{method: "a write"}
	case <-s.broken:
		return s.brokenErr("a write")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// write writes the queued messages to stdin until the run ends.
func (s *acpSession) write() {
	for {
		select {
		case w := <-s.out:
			// The last check of the sender's context: a message whose
			// sender gave up, such as a cancelled turn's prompt, is
			// dropped here, never written.
			if err := w.ctx.Err(); err != nil {
				w.ack <- err
				continue
			}
			w.ack <- nil
			_, err := s.stdin.Write(w.b)
			if w.written != nil {
				w.written <- err == nil
			}
			if err != nil {
				// Every request waiting for an answer, and every
				// sender, learns that none comes.
				s.mu.Lock()
				s.writeErr = err
				s.mu.Unlock()
				close(s.broken)
				return
			}
		case <-s.done:
			return
		case <-s.dead:
			return
		}
	}
}

// read takes the agent's messages until its stdout ends.
func (s *acpSession) read(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), acpMaxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg acpMessage
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			go s.answer(msg)
		case msg.Method != "":
			if msg.Method == "session/update" {
				s.update(msg.Params)
			}
		case len(msg.ID) > 0:
			var id int64
			if json.Unmarshal(msg.ID, &id) != nil {
				continue
			}
			s.mu.Lock()
			ch := s.pending[id]
			delete(s.pending, id)
			if ch != nil && id == s.promptID {
				// Later notifications are outside this turn even if Run has
				// not yet consumed the reply on ch.
				s.promptID = 0
				s.messages.named = nil // Keep only the final message for Run.
			}
			s.mu.Unlock()
			if ch != nil {
				resp := acpResponse{result: msg.Result}
				if msg.Error != nil {
					resp.err = msg.Error
				}
				ch <- resp
			}
		}
	}
	s.mu.Lock()
	s.readErr = sc.Err()
	if s.readErr == nil {
		s.readErr = io.EOF
	}
	s.mu.Unlock()
	close(s.done)
}

// answer answers a request of the agent's. Nothing can prompt a person
// while a task runs, so every permission request is rejected; the node
// offers no file system or terminal.
func (s *acpSession) answer(msg acpMessage) {
	ctx := context.Background()
	if msg.Method != "session/request_permission" {
		s.send(ctx, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "method not found"}}, nil)
		return
	}
	var p struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	json.Unmarshal(msg.Params, &p)
	// Only a one-time rejection: the agent may keep an "always" choice,
	// which the owner did not make. Once the turn is cancelled, ACP has
	// every pending request answered as cancelled.
	s.order.Lock()
	defer s.order.Unlock()
	outcome := map[string]any{"outcome": "cancelled"}
	for _, o := range p.Options {
		if o.Kind == "reject_once" && !s.cancelled {
			outcome = map[string]any{"outcome": "selected", "optionId": o.OptionID}
			break
		}
	}
	s.send(ctx, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"outcome": outcome}}, nil)
}

// begin makes the updates of session from here on the turn's.
func (s *acpSession) begin(session string) {
	s.mu.Lock()
	s.session = session
	s.messages = acpTurnMessages{}
	s.mu.Unlock()
	s.order.Lock()
	s.promptSent, s.cancelled = false, false
	s.order.Unlock()
}

// setProgress makes progress take the steps from here on.
func (s *acpSession) setProgress(progress func(string)) {
	s.mu.Lock()
	s.progress = progress
	s.mu.Unlock()
}

type acpMessageText struct {
	text     strings.Builder
	overflow bool
}

// Named messages can interleave. For an agent that omits IDs, a tool call
// or another speaker separates two runs of unidentified chunks.
type acpTurnMessages struct {
	named      map[string]*acpMessageText
	namedBytes int
	limited    bool
	last       *acpMessageText
	lastNamed  bool
	separated  bool
}

func (m *acpTurnMessages) append(id *string, text string) {
	if m.limited {
		return
	}
	if id != nil {
		if m.named == nil {
			m.named = make(map[string]*acpMessageText)
		}
		m.last = m.named[*id]
		if m.last == nil {
			m.last = new(acpMessageText)
			m.named[*id] = m.last
			m.namedBytes += len(*id)
		}
	} else if m.last == nil || m.lastNamed || m.separated {
		m.last = new(acpMessageText)
	}
	m.lastNamed, m.separated = id != nil, false
	if !m.last.overflow {
		before := m.last.text.Len()
		if before+len(text) > acpMaxResult {
			m.last.overflow = true
			m.last.text.Reset()
		} else {
			m.last.text.WriteString(text)
		}
		if id != nil {
			m.namedBytes += m.last.text.Len() - before
		}
	}
	if len(m.named) > acpMaxMessageIDs || m.namedBytes > acpMaxNamedBytes {
		// Evicting some messages would let a later chunk be mistaken for
		// their complete text. Only explicit response text is reliable now.
		*m = acpTurnMessages{limited: true}
	}
}

// result prefers negotiated response text, including an explicit empty string.
// When it is absent the agent's last streamed message supplies the answer.
func (s *acpSession) result(finalText json.RawMessage) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var text string
	var overflow bool
	switch {
	case len(finalText) != 0 && string(finalText) != "null":
		if err := json.Unmarshal(finalText, &text); err != nil {
			return "", errors.New("the agent's final response text must be a string")
		}
		overflow = len(text) > acpMaxResult
	case s.messages.limited:
		return "", errors.New("the agent's streamed messages exceeded the turn's buffer limit")
	case s.messages.last != nil:
		text, overflow = s.messages.last.text.String(), s.messages.last.overflow
	}
	if overflow {
		return "", fmt.Errorf("the agent's result is longer than the limit of %d bytes", acpMaxResult)
	}
	return strings.TrimSpace(text), nil
}

// update takes a session/update notification: the agent's message text
// as the turn's result, its tool calls and plans as its steps.
func (s *acpSession) update(params json.RawMessage) {
	var n struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &n) != nil {
		return
	}
	s.mu.Lock()
	ours := s.promptID != 0 && n.SessionID == s.session
	s.mu.Unlock()
	if !ours {
		return
	}
	// content is one block in a message chunk but a list in a tool call,
	// so it is read only for a chunk.
	var u struct {
		SessionUpdate string          `json:"sessionUpdate"`
		MessageID     *string         `json:"messageId"`
		Content       json.RawMessage `json:"content"`
	}
	if json.Unmarshal(n.Update, &u) != nil {
		return
	}
	if u.SessionUpdate == "agent_message_chunk" {
		var c struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(u.Content, &c) != nil || c.Type == "" {
			return
		}
		if c.Type != "text" {
			c.Text = ""
		}
		s.mu.Lock()
		s.messages.append(u.MessageID, c.Text)
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	switch u.SessionUpdate {
	case "tool_call", "tool_call_update", "user_message_chunk", "agent_thought_chunk":
		s.messages.separated = true
	}
	progress := s.progress
	s.mu.Unlock()
	if progress != nil {
		if line := acpProgress(n.Update); line != "" {
			progress(line)
		}
	}
}

// watchCancel bounds teardown even when a turn cannot reach its next ACP
// operation. Disarming joins the watchdog before another turn can reuse s.
func (s *acpSession) watchCancel(ctx context.Context) func() {
	done, stopped := make(chan struct{}), make(chan struct{})
	disarm := context.AfterFunc(ctx, func() {
		defer close(stopped)
		select {
		case <-done:
		case <-time.After(acpCancelGrace):
			s.kill()
			s.close()
		}
	})
	return sync.OnceFunc(func() {
		close(done)
		if !disarm() {
			<-stopped
		}
	})
}

// cancel asks the agent to end a prompt that has gone out.
func (s *acpSession) cancel(session string) {
	s.order.Lock()
	s.cancelled = true
	sent := s.promptSent
	s.order.Unlock()
	if !sent {
		return
	}
	ctx, stop := context.WithTimeout(context.Background(), acpCancelGrace)
	defer stop()
	s.send(ctx, map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": session}}, nil)
}

// observeExit reaps a process that has exited while waiting for an answer.
// An active turn still owns stdout until it has read the final response.
func (s *acpSession) observeExit() {
	s.processExited = waitACPExit(s.cmd.Process.Pid)
	close(s.exitObserved)
	if s.processExited {
		r := s.owner
		r.sessionsMu.Lock()
		for task, k := range r.kept {
			if k.s == s {
				delete(r.kept, task)
				r.sessionsMu.Unlock()
				s.close()
				return
			}
		}
		r.sessionsMu.Unlock()
	}
}

func (s *acpSession) exited() bool {
	select {
	case <-s.exitObserved:
		return s.processExited
	default:
		return false
	}
}

// kill stops the agent's processes and every write waiting for them.
func (s *acpSession) kill() {
	s.killOnce.Do(func() {
		close(s.dead)
		if p := s.cmd.Process; p != nil {
			syscall.Kill(-p.Pid, syscall.SIGKILL)
			_ = p.Kill() // The agent may have moved out of its original group.
		}
	})
}

// close ends the agent's run: it closes stdin, on which the agent exits,
// and stops its processes if it has not after acpCancelGrace.
func (s *acpSession) close() {
	s.closeOnce.Do(func() {
		s.stdin.Close()
		timer := time.AfterFunc(acpCancelGrace, s.kill)
		// Observe exit without reaping. Keeping the leader's PID reserved
		// until we kill any remaining group members avoids signaling a
		// new group that happens to reuse the same ID.
		<-s.exitObserved
		if !s.processExited {
			<-s.dead
		}
		s.kill()
		timer.Stop()
		s.cmd.Wait()
		s.owner.sessionsMu.Lock()
		delete(s.owner.sessions, s)
		s.owner.sessionsMu.Unlock()
		close(s.closed)
	})
}

// acpProgress is the progress line of one session/update, or "" for one
// that is not a step. Like the other runtimes' steps it is a fixed summary
// of structured fields: what kind of tool runs and a file's base name,
// never the agent's own title or text, which can hold anything.
func acpProgress(update json.RawMessage) string {
	var u struct {
		SessionUpdate string `json:"sessionUpdate"`
		Kind          string `json:"kind"`
		Locations     []struct {
			Path string `json:"path"`
		} `json:"locations"`
		Entries []struct {
			Status string `json:"status"`
		} `json:"entries"`
	}
	if json.Unmarshal(update, &u) != nil {
		return ""
	}
	switch u.SessionUpdate {
	case "tool_call":
		file := "a file"
		if len(u.Locations) > 0 {
			file = fileName(u.Locations[0].Path)
		}
		switch u.Kind {
		case "read":
			return "Reading " + file
		case "edit":
			return "Editing " + file
		case "delete":
			return "Deleting " + file
		case "move":
			return "Moving " + file
		case "search":
			return "Searching"
		case "execute":
			return "Running a command"
		case "fetch":
			return "Fetching a page"
		case "think":
			return "Thinking"
		}
		return "Using a tool"
	case "plan":
		done := make([]bool, len(u.Entries))
		for i, e := range u.Entries {
			done[i] = e.Status == "completed"
		}
		return todoStep(done)
	}
	return ""
}

// tailBuffer keeps the last bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const tailBufferSize = 8 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailBufferSize {
		t.buf = slices.Clone(t.buf[len(t.buf)-tailBufferSize:])
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// networkServer is the network's MCP server as the agent takes it: over
// HTTP with the bearer when it declares mcpCapabilities.http, else, as
// every ACP agent takes stdio servers, the network bridge with a one-time
// ticket in its environment, which it spends for the bearer when the agent
// starts it. nil when neither can be given.
func (r *acpRuntime) networkServer(n *networkAccess, info acpAgent) (any, error) {
	if info.AgentCapabilities.MCPCapabilities.HTTP {
		return map[string]any{
			"type": "http", "name": networkServerName, "url": n.URL,
			"headers": []map[string]string{{"name": "Authorization", "value": "Bearer " + n.Token}},
		}, nil
	}
	if r.bridge == "" || n.ticket == nil {
		return nil, nil
	}
	// The agent may start the server lazily, in a later turn of a kept
	// process, and keeps it over the task's turns: the ticket waits for it
	// until it expires or the task ends, and the bearer it redeems lasts
	// as long as the task's own.
	ticket, _, err := n.ticket()
	if err != nil {
		return nil, fmt.Errorf("mint the network bridge's ticket: %w", err)
	}
	return map[string]any{
		"name": networkServerName, "command": r.bridge,
		"args": []string{networkBridgeCommand, "-url", n.URL},
		"env":  []map[string]string{{"name": networkTicketEnv, "value": ticket}},
	}, nil
}
