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

package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentIsAValidSkill(t *testing.T) {
	// Agent runtimes require the skill directory to match the declared name.
	if !strings.HasPrefix(Content, "---\nname: "+Name+"\n") {
		t.Errorf("skill must start with frontmatter declaring name %q, got %.40q", Name, Content)
	}
	if !strings.Contains(Content, "description:") {
		t.Error("skill frontmatter must carry a description, agents use it to decide when to load the skill")
	}
}

func TestTargetPaths(t *testing.T) {
	user := UserTargets("/home/u")
	want := map[string]bool{
		filepath.Join("/home/u", ".claude", "skills", Name, "SKILL.md"):           true,
		filepath.Join("/home/u", ".gemini", "config", "skills", Name, "SKILL.md"): true,
	}
	for _, target := range user {
		if !want[target.Path()] {
			t.Errorf("unexpected user target path %q", target.Path())
		}
		delete(want, target.Path())
	}
	if len(want) != 0 {
		t.Errorf("missing user target paths: %v", want)
	}

	project := ProjectTargets("/work/repo")
	wantProject := map[string]bool{
		filepath.Join("/work/repo", ".claude", "skills", Name, "SKILL.md"): true,
		filepath.Join("/work/repo", ".agents", "skills", Name, "SKILL.md"): true,
	}
	for _, target := range project {
		if !wantProject[target.Path()] {
			t.Errorf("unexpected project target path %q", target.Path())
		}
		delete(wantProject, target.Path())
	}
	if len(wantProject) != 0 {
		t.Errorf("missing project target paths: %v", wantProject)
	}

	if got := CustomTarget("/opt/skills").Path(); got != filepath.Join("/opt/skills", Name, "SKILL.md") {
		t.Errorf("custom target path: got %q", got)
	}
}

func TestInstallLifecycle(t *testing.T) {
	target := CustomTarget(filepath.Join(t.TempDir(), "skills"))

	if state, err := Inspect(target); err != nil || state != StateMissing {
		t.Fatalf("fresh directory: got %q, %v; want %q, nil", state, err, StateMissing)
	}

	if state, err := Install(target); err != nil || state != StateMissing {
		t.Fatalf("first install: got %q, %v; want %q, nil", state, err, StateMissing)
	}
	data, err := os.ReadFile(target.Path())
	if err != nil {
		t.Fatalf("reading installed skill: %v", err)
	}
	if string(data) != Content {
		t.Error("installed skill does not match the embedded content")
	}

	if state, err := Install(target); err != nil || state != StateCurrent {
		t.Fatalf("reinstall must be a no-op: got %q, %v; want %q, nil", state, err, StateCurrent)
	}

	if err := os.WriteFile(target.Path(), []byte("stale skill"), 0644); err != nil {
		t.Fatalf("seeding stale skill: %v", err)
	}
	if state, err := Inspect(target); err != nil || state != StateOutdated {
		t.Fatalf("stale skill: got %q, %v; want %q, nil", state, err, StateOutdated)
	}
	if state, err := Install(target); err != nil || state != StateOutdated {
		t.Fatalf("upgrade: got %q, %v; want %q, nil", state, err, StateOutdated)
	}
	data, err = os.ReadFile(target.Path())
	if err != nil {
		t.Fatalf("reading upgraded skill: %v", err)
	}
	if string(data) != Content {
		t.Error("upgrade must overwrite a stale skill")
	}
}

func TestInstallReportsUnwritableTarget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(root, 0500); err != nil {
		t.Fatalf("creating read-only root: %v", err)
	}
	if _, err := Install(CustomTarget(filepath.Join(root, "skills"))); err == nil {
		t.Error("install into an unwritable directory must return an error")
	}
}
