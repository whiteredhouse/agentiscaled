// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package skill installs the Agent Mesh agent skill into the directories local AI
// agents scan, so an agent can bootstrap a node and drive the mesh without the
// user pasting instructions by hand.
package skill

import (
	"fmt"
	"os"
	"path/filepath"

	agentmeshskill "github.com/google/agentmesh/agents/skills/agentmesh"
)

const (
	// Name is the directory an installed skill lives in.
	Name = "agentmesh"
	// fileName is the document every agent skill runtime looks for.
	fileName = "SKILL.md"
)

// Content is the skill document installed for local agents.
var Content = agentmeshskill.Markdown

// Target is a directory tree that an agent scans for skills.
type Target struct {
	// Agent names the tool that reads this directory.
	Agent string
	// Root is the skills directory, e.g. ~/.claude/skills.
	Root string
}

// Path is the file this target installs to.
func (t Target) Path() string {
	return filepath.Join(t.Root, Name, fileName)
}

// UserTargets are the per-user skill directories, shared by every project.
func UserTargets(home string) []Target {
	return []Target{
		{Agent: "Claude", Root: filepath.Join(home, ".claude", "skills")},
		{Agent: "Antigravity", Root: filepath.Join(home, ".gemini", "config", "skills")},
	}
}

// ProjectTargets are the skill directories scoped to the project rooted at dir.
func ProjectTargets(dir string) []Target {
	return []Target{
		{Agent: "Claude", Root: filepath.Join(dir, ".claude", "skills")},
		{Agent: "Antigravity", Root: filepath.Join(dir, ".agents", "skills")},
	}
}

// CustomTarget installs into an explicit skills directory.
func CustomTarget(root string) Target {
	return Target{Root: root}
}

// State is what a target holds relative to the embedded skill.
type State string

const (
	// StateMissing means no skill is installed for this target.
	StateMissing State = "not installed"
	// StateOutdated means an older or edited skill is installed.
	StateOutdated State = "outdated"
	// StateCurrent means the installed skill matches this binary's.
	StateCurrent State = "up to date"
)

// Inspect reports the state of t without modifying anything.
func Inspect(t Target) (State, error) {
	existing, err := os.ReadFile(t.Path())
	switch {
	case os.IsNotExist(err):
		return StateMissing, nil
	case err != nil:
		return "", fmt.Errorf("reading %s: %w", t.Path(), err)
	case string(existing) == Content:
		return StateCurrent, nil
	default:
		return StateOutdated, nil
	}
}

// Install writes the skill to t, creating parent directories as needed. It
// reports the state found before writing, so callers can tell an install from
// an upgrade from a no-op.
func Install(t Target) (State, error) {
	before, err := Inspect(t)
	if err != nil {
		return "", err
	}
	if before == StateCurrent {
		return before, nil
	}
	dir := filepath.Dir(t.Path())
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.WriteFile(t.Path(), []byte(Content), 0644); err != nil {
		return "", fmt.Errorf("writing %s: %w", t.Path(), err)
	}
	return before, nil
}
