package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"go.yaml.in/yaml/v3"
)

// maxDefinitionSize bounds an agent definition file. Its instructions
// go to the runtime as one argument, which Linux limits to 128 KiB.
const maxDefinitionSize = 96 << 10

// agentDefinition is a subagent definition that a hosted agent takes its
// role from (-definition): a Claude Code agent file (Markdown with YAML
// front matter, as in .claude/agents) or a Codex agent file (TOML, as in
// .codex/agents). Its instructions are added to the system prompt of every
// task the agent runs. Its model and tool settings apply only when the
// agent's runtime is the one the file was written for, and only ever narrow
// what the agent's policy allows.
type agentDefinition struct {
	// WrittenFor is the runtime the file was written for, by its format:
	// claude or codex. It does not pick the agent's runtime.
	WrittenFor   string
	Name         string
	Description  string
	Instructions string
	// Model is the model the file asks for, empty for the runtime's.
	Model string
	// Tools (claude) is the built-in tools the agent may use; nil for all
	// that the policy allows.
	Tools []string
	// DisallowedTools (claude) is tools the agent may not use.
	DisallowedTools []string
	// ReasoningEffort (codex) is model_reasoning_effort.
	ReasoningEffort string
	// ReadOnly (codex) is sandbox_mode = "read-only".
	ReadOnly bool
}

// loadDefinition reads the agent definition at path, whose format its
// extension names: .md for Claude Code, .toml for Codex.
func loadDefinition(path string) (*agentDefinition, error) {
	b, err := readDefinition(path)
	if err != nil {
		return nil, err
	}
	return parseDefinition(path, b)
}

func readDefinition(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("agent definition: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxDefinitionSize+1))
	if err != nil {
		return nil, fmt.Errorf("agent definition %s: %w", path, err)
	}
	if len(b) > maxDefinitionSize {
		return nil, fmt.Errorf("agent definition %s: larger than %d KiB", path, maxDefinitionSize>>10)
	}
	return b, nil
}

// parseDefinition reads the content b of the definition at path.
func parseDefinition(path string, b []byte) (d *agentDefinition, err error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md":
		d, err = parseClaudeDefinition(b)
	case ".toml":
		d, err = parseCodexDefinition(b)
	default:
		return nil, fmt.Errorf("agent definition %s: use a Claude Code agent file (.md) or a Codex agent file (.toml)", path)
	}
	if err != nil {
		return nil, fmt.Errorf("agent definition %s: %w", path, err)
	}
	if d.Instructions == "" {
		return nil, fmt.Errorf("agent definition %s has no instructions", path)
	}
	return d, nil
}

// definitionCopy is where the node keeps its copy of agent name's
// definition, of the format of path's extension: in the agent's own state in
// the data directory, out of reach of the agent's directory, so that no task
// can change the agent's role, and no change in the project changes it
// unseen.
func definitionCopy(dataDir, name, path string) string {
	return filepath.Join(dataDir, "agents", name, "definition"+strings.ToLower(filepath.Ext(path)))
}

// copyDefinition copies the definition at source to agent name's state and
// returns the copy's path.
func copyDefinition(dataDir, name, source string) (string, error) {
	b, err := readDefinition(source)
	if err != nil {
		return "", err
	}
	if _, err := parseDefinition(source, b); err != nil {
		return "", err
	}
	dst := definitionCopy(dataDir, name, source)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	removeDefinitionCopies(dataDir, name)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// removeDefinitionCopies removes agent name's copy of a definition, of
// either format.
func removeDefinitionCopies(dataDir, name string) {
	for _, ext := range []string{".md", ".toml"} {
		_ = os.Remove(filepath.Join(dataDir, "agents", name, "definition"+ext))
	}
}

// parseClaudeDefinition reads a Claude Code agent file: YAML front matter
// between --- lines, then the agent's system prompt.
func parseClaudeDefinition(b []byte) (*agentDefinition, error) {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	rest, ok := bytes.CutPrefix(b, []byte("---\n"))
	if !ok {
		return nil, errors.New("no front matter: a Claude Code agent file starts with a --- line")
	}
	front, body, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		if front, ok = bytes.CutSuffix(rest, []byte("\n---")); !ok {
			return nil, errors.New("front matter has no closing --- line")
		}
	}
	var m struct {
		Name            string    `yaml:"name"`
		Description     string    `yaml:"description"`
		Model           string    `yaml:"model"`
		Tools           toolNames `yaml:"tools"`
		DisallowedTools toolNames `yaml:"disallowedTools"`
	}
	if err := yaml.Unmarshal(front, &m); err != nil {
		return nil, fmt.Errorf("front matter: %w", err)
	}
	d := &agentDefinition{WrittenFor: "claude", Name: m.Name, Description: m.Description,
		Instructions: strings.TrimSpace(string(body)), Model: m.Model,
		Tools: m.Tools, DisallowedTools: m.DisallowedTools}
	if d.Model == "inherit" {
		d.Model = ""
	}
	return d, nil
}

// toolNames is a Claude Code tool list, written as "Read, Grep" or as a
// YAML list.
type toolNames []string

func (t *toolNames) UnmarshalYAML(n *yaml.Node) error {
	var list []string
	if n.Kind == yaml.SequenceNode {
		if err := n.Decode(&list); err != nil {
			return err
		}
	} else {
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		list = strings.Split(s, ",")
	}
	names := toolNames{}
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			names = append(names, s)
		}
	}
	*t = names
	return nil
}

// parseCodexDefinition reads a Codex agent file.
func parseCodexDefinition(b []byte) (*agentDefinition, error) {
	var m struct {
		Name                  string `toml:"name"`
		Description           string `toml:"description"`
		DeveloperInstructions string `toml:"developer_instructions"`
		Model                 string `toml:"model"`
		ModelReasoningEffort  string `toml:"model_reasoning_effort"`
		SandboxMode           string `toml:"sandbox_mode"`
	}
	if _, err := toml.Decode(string(b), &m); err != nil {
		return nil, err
	}
	return &agentDefinition{WrittenFor: "codex", Name: m.Name, Description: m.Description,
		Instructions: strings.TrimSpace(m.DeveloperInstructions), Model: m.Model,
		ReasoningEffort: m.ModelReasoningEffort, ReadOnly: m.SandboxMode == "read-only"}, nil
}

// definitionRoot is the directory a definition belongs to when it sits in
// a project's .claude/agents or .codex/agents: the project's, which an agent
// with that definition works in unless -dir says otherwise. It is empty for
// a definition anywhere else.
func definitionRoot(path string) string {
	agents := filepath.Dir(path)
	if filepath.Base(agents) != "agents" {
		return ""
	}
	tool := filepath.Dir(agents)
	if b := filepath.Base(tool); b != ".claude" && b != ".codex" {
		return ""
	}
	return filepath.Dir(tool)
}

// claudeTools is the --tools value of a claude run with definition d: the
// definition's built-in tools, of those that allowed permits when allowed
// is set. It is empty, with ok false, when nothing narrows the run.
func claudeTools(d *agentDefinition, allowed []string) (tools string, ok bool) {
	var want []string
	if d != nil && d.WrittenFor == "claude" && d.Tools != nil {
		for _, t := range d.Tools {
			// MCP tools are not built-in tools; --tools cannot name them.
			if !strings.HasPrefix(t, "mcp__") {
				want = append(want, t)
			}
		}
		if allowed != nil {
			want = slices.DeleteFunc(want, func(t string) bool { return !slices.Contains(allowed, t) })
		}
	} else if allowed != nil {
		want = allowed
	} else {
		return "", false
	}
	return strings.Join(want, ","), true
}

// definedRuntime is a runtime that runs its tasks with a definition.
type definedRuntime interface {
	setDefinition(*agentDefinition)
}

// applyDefinition gives rt the definition at path and returns it.
func applyDefinition(rt runtime, path string) (*agentDefinition, error) {
	d, err := loadDefinition(path)
	if err != nil {
		return nil, err
	}
	dr, ok := rt.(definedRuntime)
	if !ok {
		return nil, errors.New("its runtime cannot run with a definition")
	}
	dr.setDefinition(d)
	return d, nil
}

// ignoredOn names the settings of d that runtime rt ignores because d was
// written for another runtime; empty when it ignores none.
func (d *agentDefinition) ignoredOn(rt string) []string {
	if d.WrittenFor == rt {
		return nil
	}
	var names []string
	if d.Model != "" {
		names = append(names, "model")
	}
	if d.Tools != nil {
		names = append(names, "tools")
	}
	if len(d.DisallowedTools) > 0 {
		names = append(names, "disallowedTools")
	}
	if d.ReasoningEffort != "" {
		names = append(names, "model_reasoning_effort")
	}
	if d.ReadOnly {
		names = append(names, "sandbox_mode")
	}
	return names
}
