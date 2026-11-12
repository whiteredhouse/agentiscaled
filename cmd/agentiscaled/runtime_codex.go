package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
)

// codexRuntime does tasks with the Codex CLI in the agent's directory, one
// session per task and one run per turn, under the selected local runtime
// configuration.
type codexRuntime struct {
	command string
	// installDir holds the Codex executable, which its Linux sandbox runs
	// again inside the sandbox, so the run must be able to read it.
	installDir string
	// home is the run's CODEX_HOME, which holds its Codex sign-in: the
	// owner's own with policy.OwnerConfig, else the agent's own in the
	// node's data directory, with home/home as its HOME, so that no skill,
	// plugin or setting of the owner's Codex reaches a remote request.
	home   string
	policy agentPolicy
	// def, when set, is the agent's definition (-definition).
	def *agentDefinition
	// extra goes before the prompt argument; tests point Codex at a fake
	// model with it.
	extra []string
}

func newCodexRuntime(command, dataDir string, policy agentPolicy) (runtime, error) {
	if command == "" {
		command = "codex"
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("the Codex CLI %q was not found: %w", command, err)
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("%s --version: %w", path, err)
	}
	log.Printf("codex runtime uses %s", strings.TrimSpace(string(out)))
	home, err := codexHome(dataDir, policy)
	if err != nil {
		return nil, err
	}
	if policy.OwnerConfig {
		if !codexSignedIn(path, dataDir, policy) && !codexEnvSignedIn(policy) {
			return nil, fmt.Errorf("your Codex is not signed in (CODEX_HOME %s): run\n  %s login", home, path)
		}
	} else {
		if err := os.MkdirAll(filepath.Join(home, "home"), 0o700); err != nil {
			return nil, err
		}
		if !codexSignedIn(path, dataDir, policy) {
			return nil, fmt.Errorf("the agent's Codex is not signed in. With -agent-owner-config=false it signs in apart from your own Codex, so that a remote request cannot reach your Codex skills, plugins or settings: run\n  CODEX_HOME=%s %s login", home, path)
		}
	}
	return &codexRuntime{command: path, installDir: codexInstallDir(path), home: home, policy: policy}, nil
}

func (r *codexRuntime) setDefinition(d *agentDefinition) { r.def = d }

// codexHome is the run's CODEX_HOME: with policy.OwnerConfig the owner's,
// as Codex finds it ($CODEX_HOME, else ~/.codex), else the agent's own in
// the node's data directory.
func codexHome(dataDir string, policy agentPolicy) (string, error) {
	if !policy.OwnerConfig {
		return filepath.Abs(filepath.Join(dataDir, "codex"))
	}
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Abs(home)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

func codexSignedIn(_, dataDir string, policy agentPolicy) bool {
	home, err := codexHome(dataDir, policy)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, "auth.json"))
	return err == nil
}

// codexEnvSignedIn says whether the owner's Codex has an API key in the
// environment, which Codex uses before its sign-in. It counts only with the
// owner's configuration: the agent's own never gets the node's credentials.
func codexEnvSignedIn(policy agentPolicy) bool {
	return policy.OwnerConfig && os.Getenv("CODEX_API_KEY") != ""
}

// codexLogin is `codex login`: the owner's own with policy.OwnerConfig, which
// stores their CODEX_API_KEY when they have one, so that the background
// service, which does not get it from their shell, signs in with it too;
// else for the agent's own CODEX_HOME, which it creates first.
func codexLogin(ctx context.Context, command, dataDir string, policy agentPolicy) (*exec.Cmd, string) {
	if policy.OwnerConfig {
		if key := os.Getenv("CODEX_API_KEY"); key != "" {
			cmd := exec.CommandContext(ctx, command, "login", "--with-api-key")
			cmd.Stdin = strings.NewReader(key + "\n")
			return cmd, "printenv CODEX_API_KEY | " + shellQuote(command) + " login --with-api-key"
		}
		return exec.CommandContext(ctx, command, "login"), shellQuote(command) + " login"
	}
	home, err := codexHome(dataDir, policy)
	if err == nil {
		err = os.MkdirAll(home, 0o700)
	}
	if err != nil {
		home = filepath.Join(dataDir, "codex")
	}
	cmd := exec.CommandContext(ctx, command, "login")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	return cmd, "CODEX_HOME=" + shellQuote(home) + " " + shellQuote(command) + " login"
}

// codexInstallDir is the directory that holds the Codex executable at path
// and what it needs: the node_modules directory of an npm install, whose
// wrapper starts a binary from a sibling package; for an executable in a
// bin directory, the install that bin belongs to; or else the executable's
// own directory. It never names the home directory, an ancestor of it or a
// shared prefix such as /usr/local, where a remotely prompted command could
// read far more than Codex: then it names only the executable's directory,
// or nothing if that is one of them too.
func codexInstallDir(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if i := strings.LastIndex(path, string(filepath.Separator)+"node_modules"+string(filepath.Separator)); i >= 0 {
		if dir := path[:i+len("/node_modules")]; !tooBroad(dir) {
			return dir
		}
	}
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "bin" && !tooBroad(filepath.Dir(dir)) {
		return filepath.Dir(dir)
	}
	if tooBroad(dir) {
		return ""
	}
	return dir
}

// sharedPrefixes hold many packages besides Codex.
var sharedPrefixes = []string{"/usr", "/usr/local", "/opt", "/opt/homebrew", "/opt/local", "/home/linuxbrew/.linuxbrew", "/snap"}

// tooBroad says whether granting reads on dir would reach beyond a Codex
// install: the root, the user's home or an ancestor of it, a top-level
// directory, or a shared prefix.
func tooBroad(dir string) bool {
	dir = filepath.Clean(dir)
	// Compared with the home directory as its real path, so a path through
	// a symlink (macOS's /var is /private/var) is too.
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	if dir == "/" || filepath.Dir(dir) == "/" {
		return true
	}
	for _, p := range sharedPrefixes {
		if dir == p {
			return true
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		home = filepath.Clean(home)
		if real, err := filepath.EvalSymlinks(home); err == nil {
			home = real
		}
		if dir == home || strings.HasPrefix(home, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (r *codexRuntime) Describe(card *a2a.AgentCard) {
	card.Description = "A Codex CLI agent on this node that works in its configured directory under local runtime permissions. Send one text message with the task; it answers with the result, or asks a question (input-required) that you answer with a message on the same task."
	card.Skills = []a2a.AgentSkill{{
		ID:          "task",
		Name:        "Task",
		Description: "Does a task in the agent's directory within its configured permissions.",
		Tags:        []string{"codex"},
		Examples:    []string{"Implement the change described in the task", "Review the uncommitted changes", "Explain how requests are authenticated"},
	}}
}

// Run runs `codex exec` in dir:
//   - With the owner's configuration (agentPolicy.OwnerConfig, the
//     default), Codex runs as the owner's own would: their CODEX_HOME,
//     sign-in, skills, plugins, settings and MCP servers, agentiscale
//     included, so the run can call the network's other agents. Otherwise
//     CODEX_HOME and HOME are the agent's own, so none of the owner's
//     skills (~/.agents/skills, ~/.codex/skills), plugins or settings
//     exist for the run, and it uses the agent's own sign-in: no API key or
//     token in the node's environment reaches it (codexEnv). Its
//     configuration is ignored too, so the run has no MCP servers and
//     cannot delegate.
//   - The checkout's AGENTS.md files and skills load as in any Codex run
//     in it, unless the owner's policy turns them off (agentPolicy): then
//     no project instruction file is loaded, and every skill in or above the
//     checkout, and Codex's bundled ones, are disabled.
//   - With a definition (-definition), its instructions are the run's
//     developer instructions; a Codex agent file's model and reasoning
//     effort apply, and its sandbox_mode only when it is read-only.
//   - Commands Codex runs get only the core environment variables (HOME,
//     PATH, USER and the like), not the owner's tokens and keys.
//   - With the owner's configuration, its filesystem/network permissions
//     control command access. The adapter does not replace that profile. Exec
//     cannot present human approval prompts and refuses escalation; it can use
//     an automatic reviewer only when the owner configured one.
//     With the isolated configuration, a read-only profile confines reads to
//     the workspace, minimal system files and the Codex install; CODEX_HOME
//     stays denied and approvals are off.
//   - Each task is a Codex session, kept in the run's CODEX_HOME like any
//     other. The run ends with an answer in turnSchema (--output-schema),
//     and the caller's answer to a question resumes the session.
func (r *codexRuntime) Run(ctx context.Context, dir string, req taskRequest) (turnResult, error) {
	tmp, err := os.MkdirTemp("", "agentiscale-codex-")
	if err != nil {
		return turnResult{}, err
	}
	defer os.RemoveAll(tmp)
	last, schema := filepath.Join(tmp, "last-message.txt"), filepath.Join(tmp, "turn-schema.json")
	if err := os.WriteFile(schema, []byte(turnSchema), 0o600); err != nil {
		return turnResult{}, err
	}
	// A new task starts a session (codex exec); the caller's answer to the
	// agent's question resumes it (codex exec resume), which takes the same
	// options except -C, -s and --color.
	args := []string{"exec"}
	if req.Session != "" {
		if err := uuid.Validate(req.Session); err != nil {
			return turnResult{}, fmt.Errorf("codex session %q: %w", req.Session, err)
		}
		args = append(args, "resume")
	}
	if !r.policy.OwnerConfig {
		args = append(args, "--ignore-user-config")
	}
	// The agent's directory is the one its owner chose for it, often an
	// empty folder the setup created: Codex's refusal to run outside a Git
	// repository or a folder it trusts would stop every task there.
	args = append(args, "--skip-git-repo-check")
	if req.Session == "" {
		args = append(args, "--color", "never")
	}
	if !r.policy.OwnerConfig {
		args = append(args, "-c", `approval_policy="never"`)
	}
	if !r.policy.DirInstructions {
		args = append(args, "-c", "project_doc_max_bytes=0")
	}
	if !r.policy.DirSkills {
		skills, err := checkoutSkills(dir)
		if err != nil {
			return turnResult{}, fmt.Errorf("look for skills in %s: %w", dir, err)
		}
		// No skill list in the instructions, and the checkout's own skills
		// cannot be loaded, even when the request names one.
		args = append(args,
			"-c", "skills.include_instructions=false",
			"-c", "skills.bundled.enabled=false",
			"-c", "skills.config="+disabledSkills(skills))
	}
	args = append(args, "-c", `shell_environment_policy.inherit="core"`)
	if !r.policy.OwnerConfig {
		args = append(args, "-c", `default_permissions="agentiscale-read-only"`,
			"-c", "permissions.agentiscale-read-only="+r.permissions())
	}
	if d := r.def; d != nil {
		args = append(args, "-c", "developer_instructions="+tomlString(d.Instructions))
		if d.WrittenFor == "codex" {
			if d.Model != "" {
				args = append(args, "-m", d.Model)
			}
			if d.ReasoningEffort != "" {
				args = append(args, "-c", "model_reasoning_effort="+tomlString(d.ReasoningEffort))
			}
			// The isolated configuration is read-only already.
			if d.ReadOnly && r.policy.OwnerConfig && req.Session == "" {
				args = append(args, "-s", "read-only")
			} else if d.ReadOnly && r.policy.OwnerConfig {
				args = append(args, "-c", `sandbox_mode="read-only"`)
			}
		}
	}
	if req.Session == "" {
		args = append(args, "-C", dir)
	}
	args = append(args, "--json", "--output-schema", schema, "--output-last-message", last)
	args = append(args, r.extra...)
	if req.Session != "" {
		args = append(args, req.Session)
	}
	args = append(args, "-")
	cmd := exec.CommandContext(ctx, r.command, args...)
	cmd.Dir = dir
	if r.policy.OwnerConfig {
		// The owner's Codex as they run it, whose MCP servers may need
		// variables of theirs.
		cmd.Env = os.Environ()
	} else {
		cmd.Env = codexEnv(os.Environ(), r.home)
	}
	cmd.Stdin = strings.NewReader(taskPrompt(req))
	// Its events stream on stdout: each step goes to req.Progress as it
	// happens, and thread.started names the session.
	var thread, failure string
	stdout := &lineWriter{line: func(line []byte) {
		if t := codexThread(line); t != "" && thread == "" {
			thread = t
		} else if f := codexFailure(line); f != "" {
			failure = f
		} else if p := codexProgress(line); p != "" {
			req.progress(p)
		}
	}}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	// Codex starts helpers; end the whole process group with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	stdout.Close()
	if runErr != nil {
		if ctx.Err() != nil {
			return turnResult{}, ctx.Err()
		}
		// Why the turn failed, such as a model the account cannot use, is
		// in the event stream; stderr has only Codex's own warnings then.
		if failure != "" {
			return turnResult{}, fmt.Errorf("codex exec: %v: %s", runErr, failure)
		}
		return turnResult{}, fmt.Errorf("codex exec: %v: %s", runErr, failureLines(stderr.String(), 3))
	}
	b, err := os.ReadFile(last)
	if err != nil {
		return turnResult{}, err
	}
	session := req.Session
	if session == "" {
		session = thread
	}
	return parseTurn(string(b), session)
}

// codexFailure is the message of a turn.failed or error event of `codex
// exec --json`, on one line, or "".
func codexFailure(event []byte) string {
	var ev struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(event, &ev) != nil {
		return ""
	}
	switch ev.Type {
	case "turn.failed":
		return progressLine(ev.Error.Message)
	case "error":
		return progressLine(ev.Message)
	}
	return ""
}

// codexThread is the session id of a `codex exec --json` run, from its
// thread.started event.
func codexThread(events []byte) string {
	for _, line := range bytes.Split(events, []byte("\n")) {
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.Type == "thread.started" && uuid.Validate(ev.ThreadID) == nil {
			return ev.ThreadID
		}
	}
	return ""
}

// codexEnv is the run's environment: of the node's, only what a process
// needs to run and reach the network, so that no credential of the owner's,
// such as CODEX_API_KEY, which Codex prefers to its sign-in, comes along;
// and the agent's own CODEX_HOME and HOME.
func codexEnv(environ []string, home string) []string {
	keep := map[string]bool{
		"PATH": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true,
		"LANG": true, "TERM": true, "TZ": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
		"CODEX_CA_CERTIFICATE": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	}
	env := []string{"CODEX_HOME=" + home, "HOME=" + filepath.Join(home, "home")}
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if keep[strings.ToUpper(k)] || strings.HasPrefix(k, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}

// skillRoots are the directories, relative to the working directory and each
// directory above it, from which Codex 0.144 loads repository skills.
var skillRoots = []string{filepath.Join(".agents", "skills"), filepath.Join(".codex", "skills")}

const adminSkills = "/etc/codex/skills"

// checkoutSkills finds every SKILL.md that Codex could load from dir: any in
// the tree under dir, any in a skill root of dir or a directory above it,
// and the admin skills.
// Each is listed as found and with symlinks resolved, since either path may be
// the one Codex records.
func checkoutSkills(dir string) ([]string, error) {
	seen := map[string]bool{}
	var found []string
	add := func(path string) {
		paths := []string{path}
		if real, err := filepath.EvalSymlinks(path); err == nil {
			paths = append(paths, real)
		}
		for _, p := range paths {
			if !seen[p] {
				seen[p] = true
				found = append(found, p)
			}
		}
	}
	isSkill := func(name string) bool { return strings.EqualFold(name, "SKILL.md") }
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && isSkill(d.Name()) {
			add(path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Skill folders can be symlinks, which the walk does not follow.
	scan := func(root string) {
		for _, pattern := range []string{"*", filepath.Join("*", "*")} {
			matches, _ := filepath.Glob(filepath.Join(root, pattern))
			for _, m := range matches {
				entries, err := os.ReadDir(m)
				if err != nil {
					continue
				}
				for _, e := range entries {
					if isSkill(e.Name()) {
						add(filepath.Join(m, e.Name()))
					}
				}
			}
		}
	}
	for d := dir; ; d = filepath.Dir(d) {
		for _, root := range skillRoots {
			scan(filepath.Join(d, root))
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	// Admin skills, which every Codex on the machine loads.
	scan(adminSkills)
	return found, nil
}

// disabledSkills is the skills.config value that turns off each skill file.
func disabledSkills(paths []string) string {
	entries := make([]string, len(paths))
	for i, p := range paths {
		entries[i] = "{path=" + tomlString(p) + ", enabled=false}"
	}
	return "[" + strings.Join(entries, ", ") + "]"
}

// permissions is the run's permission profile, a TOML inline table. It
// extends Codex's read-only profile, which writes nothing, and takes away
// reads outside the agent's directory.
func (r *codexRuntime) permissions() string {
	fs := `":root"="deny", ":minimal"="read", ":workspace_roots"="read"`
	if r.installDir != "" {
		fs += ", " + tomlString(r.installDir) + `="read"`
	}
	// The agent's sign-in stays out of reach even if a granted directory
	// contains it; the node also refuses an agent directory that overlaps
	// its data directory (agentDirApart).
	if r.home != "" {
		fs += ", " + tomlString(r.home) + `="deny"`
	}
	return `{extends=":read-only", filesystem={` + fs + `}}`
}

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, c)
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
