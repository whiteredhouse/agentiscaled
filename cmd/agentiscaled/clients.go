package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
)

// mcpEntry is a client's MCP server entry that starts `agentiscaled mcp`.
type mcpEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
	// Disabled is an entry the client keeps but does not start.
	Disabled bool `json:"-"`
}

// agentClient is an agent client whose MCP configuration `up` can add this
// node to (docs/connect-agents.md).
type agentClient interface {
	name() string
	installed() bool
	// entries reads the client's MCP servers that run a command.
	entries(ctx context.Context) (map[string]mcpEntry, error)
	// add adds a server the client does not have yet, and says what it did.
	add(ctx context.Context, name string, e mcpEntry) (string, error)
	// step is how to add the server by hand.
	step(name string, e mcpEntry) string
}

const clientsAuto = "auto"

// clientNames are the -clients values, in the order `up` connects them.
var clientNames = []string{"claude-desktop", "codex"}

// checkClients checks a -clients value before anything is enrolled.
func checkClients(value string) error {
	if value == clientsAuto || value == "none" {
		return nil
	}
	for _, n := range strings.Split(value, ",") {
		if !contains(clientNames, strings.TrimSpace(n)) {
			return fmt.Errorf("-clients %q: unknown client %q (have auto, none, %s)", value, n, strings.Join(clientNames, ", "))
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// selectClients turns a checked -clients value into the clients to connect.
// auto connects every client this system can have, including on a node
// that hosts an agent.
func selectClients(value string) []agentClient {
	var names []string
	switch value {
	case "none":
		return nil
	case clientsAuto:
		for _, n := range clientNames {
			if n != "claude-desktop" || goruntime.GOOS == "darwin" {
				names = append(names, n)
			}
		}
	default:
		for _, n := range strings.Split(value, ",") {
			names = append(names, strings.TrimSpace(n))
		}
	}
	var clients []agentClient
	for _, n := range names {
		switch n {
		case "claude-desktop":
			clients = append(clients, newClaudeDesktop())
		case "codex":
			clients = append(clients, newCodexClient())
		}
	}
	return clients
}

// clientEntryName names this node's entry in a client's configuration:
// agentiscale for the default data directory, so the documented setup and
// the written one agree, and after the data directory otherwise, so two
// nodes on one machine each get their own.
func clientEntryName(dataDir string) string {
	if realAbs(dataDir) == realAbs(defaultDataDir()) {
		return "agentiscale"
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(filepath.Base(dataDir)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	base := strings.TrimSuffix(b.String(), "-")
	switch {
	case base == "" || base == "agentiscale":
		return "agentiscale-node"
	case strings.HasPrefix(base, "agentiscale-"):
		return base
	default:
		return "agentiscale-" + base
	}
}

func mcpEntryFor(exe, dataDir string) mcpEntry {
	args := []string{"mcp"}
	if realAbs(dataDir) != realAbs(defaultDataDir()) {
		args = append(args, "-data-dir", dataDir)
	}
	return mcpEntry{Command: exe, Args: args}
}

// servesNode tells whether an entry runs `agentiscaled mcp` for the node in
// dataDir, whichever binary and name it has.
func servesNode(e mcpEntry, dataDir string) bool {
	if !strings.HasPrefix(filepath.Base(e.Command), "agentiscaled") || len(e.Args) == 0 || e.Args[0] != "mcp" {
		return false
	}
	dir := ""
	for i := 1; i < len(e.Args); i++ {
		a := strings.TrimPrefix(strings.TrimPrefix(e.Args[i], "-"), "-")
		if a == "data-dir" && i+1 < len(e.Args) {
			i++
			dir = e.Args[i]
		} else if v, ok := strings.CutPrefix(a, "data-dir="); ok {
			dir = v
		}
	}
	if dir == "" {
		// The default data directory comes from the environment, and an entry
		// that sets its own may mean another node, so only a default that
		// this process would see counts.
		for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
			if _, ok := e.Env[k]; ok {
				return false
			}
		}
		dir = defaultDataDir()
	}
	return realAbs(dir) == realAbs(dataDir)
}

// connectClients adds this node to each client's MCP configuration. It never
// changes an entry that is there: when this node already has one, or its name
// is taken, or the configuration cannot be read, it says so and leaves the
// step to the owner.
func connectClients(ctx context.Context, out io.Writer, clients []agentClient, exe, dataDir string) {
	name, e := clientEntryName(dataDir), mcpEntryFor(exe, dataDir)
	fmt.Fprintln(out, "\nAgent clients:")
	for _, c := range clients {
		if !c.installed() {
			fmt.Fprintf(out, "  %s: not installed. To connect it later:\n%s", c.name(), c.step(name, e))
			continue
		}
		have, err := c.entries(ctx)
		if err != nil {
			fmt.Fprintf(out, "  %s: could not read its configuration (%v); left it as it is. Connect it yourself:\n%s", c.name(), err, c.step(name, e))
			continue
		}
		if found := entryForNode(have, dataDir); found != "" && have[found].Disabled {
			fmt.Fprintf(out, "  %s: has %q for this node, but it is turned off; left it as it is. Turn it on again in its configuration.\n", c.name(), found)
			continue
		} else if found != "" {
			fmt.Fprintf(out, "  %s: already connected to this node as %q.\n", c.name(), found)
			continue
		}
		if _, taken := have[name]; taken {
			fmt.Fprintf(out, "  %s: has another server named %q; left it as it is. Connect this node under another name:\n%s", c.name(), name, c.step(name+"-2", e))
			continue
		}
		did, err := c.add(ctx, name, e)
		if err != nil {
			fmt.Fprintf(out, "  %s: %v. Connect it yourself:\n%s", c.name(), err, c.step(name, e))
			continue
		}
		fmt.Fprintf(out, "  %s: %s\n", c.name(), did)
	}
}

// entryForNode is the name of an entry that serves the node: the first in
// name order that is turned on, or else the first that is turned off.
func entryForNode(have map[string]mcpEntry, dataDir string) string {
	found := ""
	for n, e := range have {
		if !servesNode(e, dataDir) {
			continue
		}
		if f := have[found]; found == "" || (f.Disabled && !e.Disabled) || (f.Disabled == e.Disabled && n < found) {
			found = n
		}
	}
	return found
}

// backupFile copies a configuration file next to itself before it is changed.
func backupFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	backup := path + ".agentiscaled-" + time.Now().UTC().Format("20060102T150405Z") + ".bak"
	return backup, os.WriteFile(backup, b, 0o600)
}

// claudeDesktop is Claude Desktop on macOS, configured through
// claude_desktop_config.json.
type claudeDesktop struct {
	path string   // claude_desktop_config.json
	apps []string // where the app may be installed
}

func newClaudeDesktop() *claudeDesktop {
	c := &claudeDesktop{apps: []string{"/Applications/Claude.app"}}
	if dir, err := os.UserConfigDir(); err == nil {
		c.path = filepath.Join(dir, "Claude", "claude_desktop_config.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		c.apps = append(c.apps, filepath.Join(home, "Applications", "Claude.app"))
	}
	return c
}

func (c *claudeDesktop) name() string { return "Claude Desktop" }

func (c *claudeDesktop) installed() bool {
	if c.path == "" {
		return false
	}
	if _, err := os.Stat(filepath.Dir(c.path)); err == nil {
		return true
	}
	for _, app := range c.apps {
		if _, err := os.Stat(app); err == nil {
			return true
		}
	}
	return false
}

func (c *claudeDesktop) read() (top []field, servers []field, err error) {
	b, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil, nil
	}
	if top, err = parseObject(b); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", c.path, err)
	}
	if raw, ok := lookup(top, "mcpServers"); ok && string(raw) != "null" {
		if servers, err = parseObject(raw); err != nil {
			return nil, nil, fmt.Errorf("%s: mcpServers: %w", c.path, err)
		}
	}
	return top, servers, nil
}

func (c *claudeDesktop) entries(context.Context) (map[string]mcpEntry, error) {
	_, servers, err := c.read()
	if err != nil {
		return nil, err
	}
	have := map[string]mcpEntry{}
	for _, f := range servers {
		var e mcpEntry
		// An entry of another shape still takes its name.
		_ = json.Unmarshal(f.value, &e)
		have[f.key] = e
	}
	return have, nil
}

func (c *claudeDesktop) add(_ context.Context, name string, e mcpEntry) (string, error) {
	top, servers, err := c.read()
	if err != nil {
		return "", err
	}
	if _, taken := lookup(servers, name); taken {
		return "", fmt.Errorf("%s changed: it has a server named %q now", c.path, name)
	}
	entry, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	servers = append(servers, field{name, entry})
	top = set(top, "mcpServers", encodeObject(servers))
	var b bytes.Buffer
	if err := json.Indent(&b, encodeObject(top), "", "  "); err != nil {
		return "", err
	}
	b.WriteByte('\n')

	mode := os.FileMode(0o644)
	backup := ""
	if fi, err := os.Stat(c.path); err == nil {
		mode = fi.Mode().Perm()
		if backup, err = backupFile(c.path); err != nil {
			return "", fmt.Errorf("back up %s: %w", c.path, err)
		}
	} else if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return "", err
	}
	tmp := c.path + ".agentiscaled.tmp"
	if err := os.WriteFile(tmp, b.Bytes(), mode); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	did := fmt.Sprintf("added %q to %s", name, c.path)
	if backup != "" {
		did += fmt.Sprintf(" (the file before: %s)", backup)
	}
	return did + ". Quit Claude Desktop (Cmd+Q) and open it again to load it.", nil
}

func (c *claudeDesktop) step(name string, e mcpEntry) string {
	cfg, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{name: e}}, "", "  ")
	return fmt.Sprintf("    Settings > Developer > Edit Config, add to claude_desktop_config.json,\n    then restart Claude Desktop:\n%s\n", indent(string(cfg), "      "))
}

// codexClient is the Codex CLI, configured through its own `codex mcp`
// commands, which keep the rest of its config.toml as it is.
type codexClient struct {
	command string // codex, as found on PATH; empty if not installed
	home    string // CODEX_HOME, where config.toml is
}

func newCodexClient() *codexClient {
	c := &codexClient{home: os.Getenv("CODEX_HOME")}
	if path, err := exec.LookPath("codex"); err == nil {
		c.command = path
	}
	if c.home == "" {
		if home, err := os.UserHomeDir(); err == nil {
			c.home = filepath.Join(home, ".codex")
		}
	}
	return c
}

func (c *codexClient) name() string    { return "Codex CLI" }
func (c *codexClient) installed() bool { return c.command != "" }

func (c *codexClient) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.command, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndex(msg, "\n"); i >= 0 && strings.HasPrefix(msg, "WARNING") {
			msg = msg[i+1:]
		}
		return nil, fmt.Errorf("codex %s: %w: %s", strings.Join(args[:2], " "), err, msg)
	}
	return stdout, nil
}

func (c *codexClient) entries(ctx context.Context) (map[string]mcpEntry, error) {
	b, err := c.run(ctx, "mcp", "list", "--json")
	if err != nil {
		return nil, err
	}
	var list []struct {
		Name      string `json:"name"`
		Enabled   *bool  `json:"enabled"`
		Transport struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"transport"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("codex mcp list --json: %w", err)
	}
	have := map[string]mcpEntry{}
	for _, s := range list {
		have[s.Name] = mcpEntry{
			Command:  s.Transport.Command,
			Args:     s.Transport.Args,
			Env:      s.Transport.Env,
			Disabled: s.Enabled != nil && !*s.Enabled,
		}
	}
	return have, nil
}

func (c *codexClient) add(ctx context.Context, name string, e mcpEntry) (string, error) {
	backup := ""
	if c.home != "" {
		cfg := filepath.Join(c.home, "config.toml")
		if _, err := os.Stat(cfg); err == nil {
			if backup, err = backupFile(cfg); err != nil {
				return "", fmt.Errorf("back up %s: %w", cfg, err)
			}
		}
	}
	args := append([]string{"mcp", "add", name, "--", e.Command}, e.Args...)
	if _, err := c.run(ctx, args...); err != nil {
		return "", err
	}
	did := fmt.Sprintf("added %q with codex mcp add", name)
	if backup != "" {
		did += fmt.Sprintf(" (config.toml before: %s)", backup)
	}
	return did + "; new Codex sessions have it.", nil
}

func (c *codexClient) step(name string, e mcpEntry) string {
	quoted := []string{shellQuote(e.Command)}
	for _, a := range e.Args {
		quoted = append(quoted, shellQuote(a))
	}
	return fmt.Sprintf("    codex mcp add %s -- %s\n", shellQuote(name), strings.Join(quoted, " "))
}

// field is one member of a JSON object, kept in the file's order so that a
// changed configuration reads like the one the owner wrote.
type field struct {
	key   string
	value json.RawMessage
}

func parseObject(b []byte) ([]field, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil {
		return nil, err
	} else if t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var fields []field
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		fields = append(fields, field{t.(string), v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON object")
	}
	return fields, nil
}

func lookup(fields []field, key string) (json.RawMessage, bool) {
	for _, f := range fields {
		if f.key == key {
			return f.value, true
		}
	}
	return nil, false
}

func set(fields []field, key string, value json.RawMessage) []field {
	for i, f := range fields {
		if f.key == key {
			fields[i].value = value
			return fields
		}
	}
	return append(fields, field{key, value})
}

func encodeObject(fields []field) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(f.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}
