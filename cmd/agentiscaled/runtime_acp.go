package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

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
	// stopped, if set, takes each turn's stop reason; runtime check uses it.
	stopped func(string)
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

// splitACPCommand splits an ACP agent's command line into its program and
// arguments, at spaces: an argument cannot hold one.
func splitACPCommand(line string) (string, []string, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", nil, errors.New("the acp runtime needs the agent's command: -command \"my-agent acp\"")
	}
	return fields[0], fields[1:], nil
}

// acpDefaultName is the name of an ACP agent without -name: its program's
// base name.
func acpDefaultName(line string) string {
	program, _, err := splitACPCommand(line)
	if err != nil {
		return ""
	}
	return strings.ToLower(filepath.Base(program))
}

// runtimeDefaultName is the name of an agent of runtime rt, run with
// command, that has no name of its own.
func runtimeDefaultName(rt, command string) string {
	if rt == "acp" {
		return acpDefaultName(command)
	}
	return runtimes[rt].defaultName
}

// resolveACPCommand makes the program of an ACP command line an absolute
// path that the background service finds.
func resolveACPCommand(line string) (string, error) {
	program, args, err := splitACPCommand(line)
	if err != nil {
		return "", err
	}
	path, err := exec.LookPath(program)
	if err != nil {
		return "", fmt.Errorf("agent command %q not found: %w", program, err)
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", err
	}
	return strings.Join(append([]string{path}, args...), " "), nil
}

// acpConfigDir is the agent's own configuration directory: empty with the
// owner's configuration, else one per program in the node's data
// directory.
func acpConfigDir(command, dataDir string, policy agentPolicy) (string, error) {
	if policy.OwnerConfig {
		return "", nil
	}
	return filepath.Abs(filepath.Join(dataDir, "acp", strings.ToLower(filepath.Base(command))))
}

func newACPRuntime(line, dataDir string, policy agentPolicy) (runtime, error) {
	program, args, err := splitACPCommand(line)
	if err != nil {
		return nil, err
	}
	path, err := exec.LookPath(program)
	if err != nil {
		return nil, fmt.Errorf("the agent command %q was not found: %w", program, err)
	}
	config, err := acpConfigDir(path, dataDir, policy)
	if err != nil {
		return nil, err
	}
	if config != "" {
		if err := os.MkdirAll(config, 0o700); err != nil {
			return nil, err
		}
	}
	r := &acpRuntime{command: path, args: args, config: config, policy: policy}
	// The agent says what it supports before it runs any task, so an
	// owner's policy that it does not enforce stops agent add and the
	// node's start, not a task.
	ctx, cancel := context.WithTimeout(context.Background(), acpProbeTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "agentiscale-acp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	s, err := r.start(ctx, dir, nil)
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
	card.Description = fmt.Sprintf("A %s agent on this node that works in its configured directory under local runtime permissions. Send one text message with the task; it answers with the result", name)
	if r.info.AgentCapabilities.LoadSession {
		card.Description += ", or asks a question (input-required) that you answer with a message on the same task."
	} else {
		card.Description += "."
	}
	card.Skills = []a2a.AgentSkill{{
		ID:          "task",
		Name:        "Task",
		Description: "Does a task in the agent's directory within its configured permissions.",
		Tags:        []string{"acp"},
	}}
}

// Run runs one turn of a task: a new ACP session for a new task, or the
// task's session loaded again for the caller's answer to the agent's
// question.
func (r *acpRuntime) Run(ctx context.Context, dir string, req taskRequest) (turnResult, error) {
	s, err := r.start(ctx, dir, req.progress)
	if err != nil {
		return turnResult{}, err
	}
	defer s.close()
	info, err := s.initialize(ctx)
	if err != nil {
		return turnResult{}, r.failure(ctx, s, err)
	}
	// The agent may have changed since the node started.
	r.info = info
	if err := r.checkPolicy(); err != nil {
		return turnResult{}, err
	}
	conventions := info.conventions()
	canAsk := info.AgentCapabilities.LoadSession

	servers := []any{}
	if req.Network != nil && info.AgentCapabilities.MCPCapabilities.HTTP {
		servers = append(servers, map[string]any{
			"type": "http", "name": networkServerName, "url": req.Network.URL,
			"headers": []map[string]string{{"name": "Authorization", "value": "Bearer " + req.Network.Token}},
		})
	}
	session := req.Session
	if session == "" {
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
	} else {
		if !canAsk {
			return turnResult{}, errors.New("the agent cannot load a session, so it cannot take an answer")
		}
		if err := s.call(ctx, "session/load", map[string]any{"sessionId": session, "cwd": dir, "mcpServers": servers}, nil); err != nil {
			return turnResult{}, r.failure(ctx, s, err)
		}
	}
	// What the agent sends from here on is this turn's: session/load
	// replays the session's history before it answers.
	s.begin(session)
	stop := context.AfterFunc(ctx, func() { s.cancel(session) })
	defer stop()
	var prompted struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			Agentiscale struct {
				Status string `json:"status"`
			} `json:"agentiscale"`
		} `json:"_meta"`
	}
	err = s.call(context.Background(), "session/prompt", map[string]any{
		"sessionId": session,
		"prompt":    []map[string]string{{"type": "text", "text": r.prompt(req, conventions.Status, canAsk)}},
	}, &prompted)
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
		return turnResult{}, fmt.Errorf("the agent stopped (%s): %s", progressLine(prompted.StopReason), failureLines(s.stderr.String(), 3))
	}
	text := s.result()
	var result turnResult
	if status := prompted.Meta.Agentiscale.Status; status != "" {
		result, err = turnAnswer{Status: status, Text: text}.result(session)
	} else {
		result, err = parseTurn(text, session)
	}
	if err != nil {
		return turnResult{}, err
	}
	if result.Question && !canAsk {
		return turnResult{}, errors.New("the agent asked a question, but it cannot load a session to take the answer")
	}
	return result, nil
}

// failure is err as the task's failure, with the agent's sign-in command
// when it needs one and the last lines of its stderr.
func (r *acpRuntime) failure(ctx context.Context, s *acpSession, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var rpcErr *acpError
	if errors.As(err, &rpcErr) && rpcErr.Code == acpAuthRequired {
		name := r.info.name(r.command)
		if login := r.info.conventions().Login; len(login) > 0 {
			shown := strings.Join(login, " ")
			if r.config != "" {
				shown = "AGENTISCALE_CONFIG_DIR=" + shellQuote(r.config) + " " + shown
			}
			return fmt.Errorf("%s is not signed in: run\n  %s", name, shown)
		}
		return fmt.Errorf("%s is not signed in: %s", name, progressLine(rpcErr.Message))
	}
	if lines := failureLines(s.stderr.String(), 3); lines != "" {
		return fmt.Errorf("%v: %s", err, lines)
	}
	return err
}

// acpTurnInstructions are the turn's instructions for an agent that ends
// its turns with _meta.agentiscale.status; canAsk false tells it that it
// cannot ask.
const acpTurnInstructions = `When you are done, answer with the result.
If you cannot go on without information that only the sender can give, ask one
short question instead; the sender's answer comes in a later message. Ask only when
you must, never for permission or confirmation, and never for anything your
configuration or the directory can tell you.`

const acpNoQuestions = `When you are done, answer with the result. You cannot ask the sender
anything: if information is missing, say what is missing in the result.`

const acpNoQuestionsJSON = `When you are done, answer with status "done" and the result as text.
You cannot ask the sender anything: if information is missing, say what is missing
in the result.`

// prompt is the turn's prompt: the task or the answer, framed as
// untrusted (taskPrompt), with the definition's instructions in front of
// a new task. Every prompt names how the turn ends: the agent's own status
// when it declares one, else an answer in turnSchema.
func (r *acpRuntime) prompt(req taskRequest, status, canAsk bool) string {
	instructions := turnInstructions + "\nAnswer with only a JSON object of this schema: " + turnSchema
	switch {
	case status && canAsk:
		instructions = acpTurnInstructions
	case status:
		instructions = acpNoQuestions
	case !canAsk:
		instructions = acpNoQuestionsJSON + "\nAnswer with only a JSON object of this schema: " + turnSchema
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
	cmd := exec.Command(r.command, r.args...)
	cmd.Dir = dir
	cmd.Env = r.env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s := &acpSession{cmd: cmd, progress: progress, pending: map[int64]chan acpResponse{}, done: make(chan struct{})}
	cmd.Stderr = &s.stderr
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
	go s.read(stdout)
	return s, nil
}

// acpSession is one run of the agent's command: the node's ACP client
// connection to it.
type acpSession struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stderr   tailBuffer
	progress func(string)

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan acpResponse
	// session is the session whose updates are the turn's, once begun.
	session string
	text    strings.Builder
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
}

func (e *acpError) Error() string {
	return fmt.Sprintf("agent error %d: %s", e.Code, progressLine(e.Message))
}

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
		"clientInfo": map[string]string{"name": "agentiscaled", "version": version},
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
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan acpResponse, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case resp := <-ch:
		if resp.err != nil {
			return resp.err
		}
		if out != nil && len(resp.result) > 0 && string(resp.result) != "null" {
			if err := json.Unmarshal(resp.result, out); err != nil {
				return fmt.Errorf("the agent's %s result: %w", method, err)
			}
		}
		return nil
	case <-s.done:
		return s.ended(method)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *acpSession) ended(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil && !errors.Is(s.readErr, io.EOF) {
		return fmt.Errorf("the agent's connection broke during %s: %w", method, s.readErr)
	}
	return fmt.Errorf("the agent exited during %s", method)
}

func (s *acpSession) send(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdin.Write(append(b, '\n'))
	return err
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
			s.answer(msg)
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
	if msg.Method != "session/request_permission" {
		s.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		return
	}
	var p struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	json.Unmarshal(msg.Params, &p)
	outcome := map[string]any{"outcome": "cancelled"}
	for _, kind := range []string{"reject_once", "reject_always"} {
		if i := slices.IndexFunc(p.Options, func(o struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		}) bool {
			return o.Kind == kind
		}); i >= 0 {
			outcome = map[string]any{"outcome": "selected", "optionId": p.Options[i].OptionID}
			break
		}
	}
	s.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"outcome": outcome}})
}

// begin makes the updates of session from here on the turn's.
func (s *acpSession) begin(session string) {
	s.mu.Lock()
	s.session = session
	s.mu.Unlock()
}

func (s *acpSession) result() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.text.String())
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
	ours := s.session != "" && n.SessionID == s.session
	s.mu.Unlock()
	if !ours {
		return
	}
	var u struct {
		SessionUpdate string `json:"sessionUpdate"`
		Content       struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(n.Update, &u) != nil {
		return
	}
	if u.SessionUpdate == "agent_message_chunk" {
		if u.Content.Type == "text" {
			s.mu.Lock()
			if s.text.Len()+len(u.Content.Text) <= acpMaxResult {
				s.text.WriteString(u.Content.Text)
			}
			s.mu.Unlock()
		}
		return
	}
	if s.progress != nil {
		if line := acpProgress(n.Update); line != "" {
			s.progress(line)
		}
	}
}

// cancel asks the agent to end the session's turn, and stops its processes
// if it has not after acpCancelGrace.
func (s *acpSession) cancel(session string) {
	s.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": session}})
	go func() {
		select {
		case <-s.done:
		case <-time.After(acpCancelGrace):
			s.kill()
		}
	}()
}

func (s *acpSession) kill() {
	if p := s.cmd.Process; p != nil {
		syscall.Kill(-p.Pid, syscall.SIGKILL)
	}
}

// close ends the agent's run: it closes stdin, on which the agent exits,
// and stops its processes if it has not after acpCancelGrace.
func (s *acpSession) close() {
	s.stdin.Close()
	select {
	case <-s.done:
	case <-time.After(acpCancelGrace):
	}
	s.kill()
	s.cmd.Wait()
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
