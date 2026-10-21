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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/agentmesh/internal/skill"
)

func TestSkillCmdInstallAndList(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")

	var out bytes.Buffer
	cmd := newSkillCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"install", "--dir", root})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("skill install: %v", err)
	}

	installed := filepath.Join(root, skill.Name, "SKILL.md")
	data, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("reading installed skill: %v", err)
	}
	if string(data) != skill.Content {
		t.Error("installed skill does not match the embedded content")
	}
	if !strings.Contains(out.String(), "Next steps:") {
		t.Errorf("install must guide the user to the next step, got:\n%s", out.String())
	}

	out.Reset()
	cmd = newSkillCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"list", "--dir", root})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("skill list: %v", err)
	}
	if !strings.Contains(out.String(), string(skill.StateCurrent)) {
		t.Errorf("list must report the installed skill as current, got:\n%s", out.String())
	}
}

func TestSkillCmdRejectsConflictingScopes(t *testing.T) {
	cmd := newSkillCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"install", "--dir", t.TempDir(), "--project"})
	if err := cmd.Execute(); err == nil {
		t.Error("--dir and --project must be mutually exclusive")
	}
}

func TestDisplayPath(t *testing.T) {
	home := filepath.Join("/home", "u")
	if got := displayPath(home, filepath.Join(home, ".claude", "skills")); got != filepath.Join("~", ".claude", "skills") {
		t.Errorf("paths under home should be shortened: got %q", got)
	}
	if got := displayPath(home, "/opt/skills"); got != "/opt/skills" {
		t.Errorf("paths outside home should be left alone: got %q", got)
	}
	if got := displayPath("", "/opt/skills"); got != "/opt/skills" {
		t.Errorf("unknown home should be left alone: got %q", got)
	}
}
