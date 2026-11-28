package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
)

// agyRuntime does tasks with Google's Antigravity CLI (agy) in the agent's
// directory, one headless run per turn, with the owner's own
// agy configuration and sign-in. It runs the unmodified agy binary; the node
// never reads or stores its sign-in.
type agyRuntime struct {
	command string
	// bridge is the agentiscaled executable that serves a run the network
	// (networkBridgeCommand).
	bridge string
	def    *agentDefinition
	// extra goes before the run's own options; tests use it.
	extra []string
}

func (r *agyRuntime) setDefinition(d *agentDefinition) { r.def = d }

// agyOwnerConfigDir is where agy keeps the owner's settings.
func agyOwnerConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "antigravity-cli"), nil
}

// agyNetworkTools are the tools of the network server (newGrant) that a run
// may call without asking: the token is what bounds them.
var agyNetworkTools = []string{"list_agents", "get_agent_card", "send_task", "get_task", "cancel_task"}

func newAgyRuntime(command, _ string, policy agentPolicy) (runtime, error) {
	if command == "" {
		command = "agy"
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("the Antigravity CLI %q was not found: %w", command, err)
	}
	// agy has no configuration home to give an agent of its own, and no
	// setting that leaves out only the directory's instructions or skills.
	if !policy.OwnerConfig {
		return nil, errors.New("agy has no configuration of its own for an agent: run it with your own agy configuration (without -owner-config=false)")
	}
	var off []string
	if !policy.DirInstructions {
		off = append(off, "-dir-instructions=false")
	}
	if !policy.DirSkills {
		off = append(off, "-dir-skills=false")
	}
	if len(off) > 0 {
		return nil, fmt.Errorf("agy cannot leave out the directory's instruction files or skills: run it without %s", strings.Join(off, " and "))
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("%s --version: %w", path, err)
	}
	log.Printf("agy runtime uses %s", strings.TrimSpace(string(out)))
	bridge, err := bridgeExecutable()
	if err != nil {
		return nil, fmt.Errorf("find agentiscaled for the network bridge: %w", err)
	}
	return &agyRuntime{command: path, bridge: bridge}, nil
}

func (r *agyRuntime) Describe(card *a2a.AgentCard) {
	card.Description = "An Antigravity (agy) agent on this node that works in its configured directory under local runtime permissions. Send one text message with the task; it answers with the result, or asks a question (input-required) that you answer with a message on the same task."
}

// Run runs agy headless in dir, with the prompt on stdin, so that it is
// not in the process list:
//   - agy runs as the owner's own would: their sign-in, settings,
//     permissions, skills, plugins and MCP servers, and the directory's
//     instruction files and skills. Nothing can prompt for approval: agy
//     denies what would prompt, and the turn fails when that leaves it
//     with no answer.
//   - With a definition (-definition), its instructions come in front of
//     the task, in a <role> block; its model and tools do not apply.
//   - Each task is an agy conversation, kept in the owner's agy data like
//     any other. The run ends with an answer in turnSchema (--json-schema),
//     and the caller's answer to a question resumes the conversation
//     (--conversation).
//   - Its events stream (--output-format stream-json): each step goes to
//     req.Progress as it happens, and the last event is the result.
//   - With req.Network, the run also has the network's other agents as the
//     MCP server agentiscale-network, which it may use without asking.
//     agy takes an MCP server for one run only from an .agents folder it
//     works in, which the agent can read, and the commands it runs get its
//     environment. So the server is the network bridge, started from a
//     folder of the node's outside the agent's directory (--add-dir), and
//     the folder holds only a one-time ticket, which the bridge removes
//     and spends once agy starts it, and which is revoked when the turn
//     ends if it is still unspent. A server of that name in the directory's
//     or the owner's own agy MCP configuration would stand in for the
//     network's and get its approval, so the run does not start then
//     (agyNetworkShadowed).
//     The run's settings (--settings) allow its tools. A later --settings
//     replaces an earlier one, so they carry the owner's own permissions
//     too (agyPermissions), in case they replace the settings file's.
func (r *agyRuntime) Run(ctx context.Context, dir string, req taskRequest) (turnResult, error) {
	args := append([]string{}, r.extra...)
	args = append(args, "--output-format", "stream-json",
		"--json-schema", turnSchema,
		// The node bounds the turn's time itself.
		"--print-timeout", "0")
	session := req.Session
	if session != "" {
		if err := uuid.Validate(session); err != nil {
			return turnResult{}, fmt.Errorf("agy conversation %q: %w", session, err)
		}
		args = append(args, "--conversation", session)
	}
	if req.Network != nil {
		if err := agyNetworkShadowed(dir); err != nil {
			return turnResult{}, err
		}
		network, cleanup, err := r.networkDir(req.Network)
		if err != nil {
			return turnResult{}, err
		}
		defer cleanup()
		permissions, err := agyPermissions()
		if err != nil {
			return turnResult{}, err
		}
		allow, _ := permissions["allow"].([]any)
		for _, tool := range agyNetworkTools {
			allow = append(allow, "mcp("+networkServerName+"/"+tool+")")
		}
		permissions["allow"] = allow
		settings, err := json.Marshal(map[string]any{"permissions": permissions})
		if err != nil {
			return turnResult{}, err
		}
		args = append(args, "--add-dir", network, "--settings", string(settings))
	}
	cmd := exec.CommandContext(ctx, r.command, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(r.prompt(req))
	cmd.Env = withoutEnv(os.Environ(), []string{networkTokenEnv})
	var result []byte
	reported := map[int]bool{}
	stdout := &lineWriter{line: func(line []byte) {
		var ev struct {
			Event          string          `json:"event"`
			ConversationID string          `json:"conversation_id"`
			Result         json.RawMessage `json:"result"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		switch ev.Event {
		case "init":
			if session == "" && ev.ConversationID != "" {
				session = ev.ConversationID
			}
		case "result":
			result = bytes.Clone(ev.Result)
			if len(result) == 0 {
				result = bytes.Clone(line)
			}
		default:
			// A step is reported as it starts and again as it ends.
			if step, p := agyProgress(line); p != "" && !reported[step] {
				reported[step] = true
				req.progress(p)
			}
		}
	}}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	stdout.Close()
	// A process the agent left running holds agy's output open past
	// WaitDelay; the result agy gave before it exited still stands.
	if errors.Is(runErr, exec.ErrWaitDelay) && result != nil {
		runErr = nil
	}
	if ctx.Err() != nil {
		return turnResult{}, ctx.Err()
	}
	if result == nil {
		if runErr != nil {
			return turnResult{}, fmt.Errorf("agy: %v: %s", runErr, failureLines(stderr.String(), 3))
		}
		return turnResult{}, errors.New("agy: no result")
	}
	return agyResult(result, session, runErr, stderr.String())
}

// agyResult reads the run's result event.
func agyResult(event []byte, session string, runErr error, stderr string) (turnResult, error) {
	var res struct {
		ConversationID string      `json:"conversation_id"`
		Status         string      `json:"status"`
		Response       string      `json:"response"`
		Error          string      `json:"error"`
		FinishedReason string      `json:"finished_reason"`
		Structured     *turnAnswer `json:"structured_output"`
		Denied         []struct {
			Action string `json:"action"`
		} `json:"denied_actions"`
	}
	if err := json.Unmarshal(event, &res); err != nil {
		return turnResult{}, fmt.Errorf("agy: unreadable result: %v", err)
	}
	if session == "" {
		session = res.ConversationID
	}
	if res.Status == "SUCCESS" && runErr != nil {
		return turnResult{}, fmt.Errorf("agy exited with %v after its result: %s", runErr, failureLines(stderr, 3))
	}
	if res.Status != "SUCCESS" {
		status := res.Status
		if !plainName.MatchString(status) {
			status = "an error"
		}
		if plainName.MatchString(res.FinishedReason) && res.FinishedReason != "error" {
			status += " (" + res.FinishedReason + ")"
		}
		detail := res.Error
		if strings.TrimSpace(detail) == "" {
			detail = stderr
		}
		return turnResult{}, fmt.Errorf("agy ended with %s: %s", status, failureLines(detail, 3))
	}
	if res.Structured != nil {
		return res.Structured.result(session)
	}
	if strings.TrimSpace(res.Response) == "" && len(res.Denied) > 0 {
		// agy denies what it cannot ask about and goes on; a turn left
		// with nothing to say was stopped by it. The action kinds are
		// agy's fixed names, such as mcp or command.
		var kinds []string
		for _, d := range res.Denied {
			if plainName.MatchString(d.Action) && !slices.Contains(kinds, d.Action) {
				kinds = append(kinds, d.Action)
			}
		}
		return turnResult{}, fmt.Errorf("the agent stopped at a step that needs an approval nobody can give on this node (%s): allow it in agy's settings (permissions.allow)", strings.Join(kinds, ", "))
	}
	return parseTurn(res.Response, session)
}

// prompt is the turn's prompt (taskPrompt), with the definition's
// instructions in front of a new task.
func (r *agyRuntime) prompt(req taskRequest) string {
	p := taskPrompt(req)
	if req.Session == "" && r.def != nil {
		p = "<role>\n" + r.def.Instructions + "\n</role>\n\n" + p
	}
	return p
}

// networkDir makes the folder from which a run starts the network bridge,
// readable only by the owner: .agents/mcp_config.json naming the bridge,
// and the file it reads a fresh one-time ticket from. cleanup revokes the
// ticket and removes the folder.
func (r *agyRuntime) networkDir(n *networkAccess) (dir string, cleanup func(), err error) {
	if n.ticket == nil {
		return "", nil, errors.New("the network gives agy no ticket")
	}
	ticket, revoke, err := n.ticket()
	if err != nil {
		return "", nil, fmt.Errorf("mint the network bridge's ticket: %w", err)
	}
	dir, err = os.MkdirTemp("", "agentiscale-agy-")
	if err != nil {
		revoke()
		return "", nil, err
	}
	cleanup = func() {
		revoke()
		os.RemoveAll(dir)
	}
	ticketFile := filepath.Join(dir, "ticket")
	config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{networkServerName: map[string]any{
		"command": r.bridge,
		"args":    []string{networkBridgeCommand, "-url", n.URL, "-ticket-file", ticketFile},
	}}})
	if err == nil {
		err = os.WriteFile(ticketFile, []byte(ticket), 0o600)
	}
	if err == nil {
		err = os.Mkdir(filepath.Join(dir, ".agents"), 0o700)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, ".agents", "mcp_config.json"), config, 0o600)
	}
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

// agyProgress is the step an event of `agy --output-format stream-json`
// reports, if any, with the step's index: a step_update of a tool, by the
// tool's name.
func agyProgress(event []byte) (step int, line string) {
	var ev struct {
		Event  string `json:"event"`
		Update struct {
			Index    int    `json:"step_index"`
			StepType string `json:"step_type"`
			ToolName string `json:"tool_name"`
			ToolInfo struct {
				Parameters struct {
					Path         string `json:"Path"`
					AbsolutePath string `json:"AbsolutePath"`
					File         string `json:"TargetFile"`
					Command      string `json:"CommandLine"`
					ServerName   string `json:"ServerName"`
					ToolName     string `json:"ToolName"`
				} `json:"parameters"`
			} `json:"tool_info"`
		} `json:"step_update"`
	}
	if json.Unmarshal(event, &ev) != nil || ev.Event != "step_update" || ev.Update.StepType != "tool" {
		return 0, ""
	}
	in := ev.Update.ToolInfo.Parameters
	path := in.Path
	if path == "" {
		path = in.AbsolutePath
	}
	if path == "" {
		path = in.File
	}
	switch name := ev.Update.ToolName; {
	case name == "":
		return 0, ""
	case name == "call_mcp_tool":
		line = "Using a tool"
		if t := strings.Trim(in.ServerName+"."+in.ToolName, "."); plainName.MatchString(t) {
			line = "Using " + t
		}
	case name == "run_command":
		line = "Running a command"
		if in.Command != "" {
			line = commandStep(in.Command)
		}
	case strings.Contains(name, "fetch") || strings.Contains(name, "url") || strings.Contains(name, "browser"):
		line = "Fetching a page"
	case strings.Contains(name, "search") || strings.Contains(name, "grep") || strings.Contains(name, "find") || strings.HasPrefix(name, "list"):
		line = "Searching"
	case strings.HasPrefix(name, "read") || strings.HasPrefix(name, "view"):
		// agy reads an MCP tool's schema from its own cache before it
		// calls it; the call is the step.
		if strings.Contains(filepath.ToSlash(path), "/.gemini/antigravity-cli/mcp/") {
			return 0, ""
		}
		line = "Reading " + fileName(path)
	case strings.HasPrefix(name, "write") || strings.HasPrefix(name, "edit") || strings.Contains(name, "replace") || strings.HasPrefix(name, "sed"):
		line = "Editing " + fileName(path)
	default:
		line = "Using a tool"
		if plainName.MatchString(name) {
			line = "Using " + name
		}
	}
	return ev.Update.Index, progressLine(line)
}

// agyPermissions is the permissions block of the owner's agy settings, if
// any, for a run's settings to carry over.
func agyPermissions() (map[string]any, error) {
	dir, err := agyOwnerConfigDir()
	if err != nil {
		return nil, err
	}
	file := filepath.Join(dir, "settings.json")
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read agy's settings to keep their permissions for the run: %w", err)
	}
	var settings struct {
		Permissions map[string]any `json:"permissions"`
	}
	if err := json.Unmarshal(b, &settings); err != nil {
		return nil, fmt.Errorf("read agy's settings to keep their permissions for the run: %s: %v", file, err)
	}
	if settings.Permissions == nil {
		settings.Permissions = map[string]any{}
	}
	return settings.Permissions, nil
}

// agyNetworkShadowed fails when the agent's directory or the owner's agy
// configuration has an MCP server of the network server's name, which agy
// could start in the place of the network's, with its tools allowed.
func agyNetworkShadowed(dir string) error {
	files := []string{filepath.Join(dir, ".agents", "mcp_config.json")}
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".gemini", "config", "mcp_config.json"))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil || len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		var config struct {
			Servers map[string]json.RawMessage `json:"mcpServers"`
		}
		if json.Unmarshal(b, &config) != nil {
			continue
		}
		if _, ok := config.Servers[networkServerName]; ok {
			return fmt.Errorf("%s has an MCP server named %s, which would take the place of the network's: rename it", f, networkServerName)
		}
	}
	return nil
}
