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

package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyFile := filepath.Join(dir, "empty")
	if err := os.WriteFile(emptyFile, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		value   string
		path    string
		want    string
		wantErr bool
	}{
		{"unset", "", "", "", false},
		{"direct value", "tok", "", "tok", false},
		{"file value trimmed", "", tokenFile, "s3cret", false},
		{"both set", "tok", tokenFile, "", true},
		{"missing file", "", filepath.Join(dir, "nope"), "", true},
		{"empty file", "", emptyFile, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve("api-token", tt.value, tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Resolve: err=%v, wantErr=%v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Resolve: got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFromPathOrEnv(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("file wins over env", func(t *testing.T) {
		t.Setenv("AGENTMESH_TEST_SECRET", "env-secret")
		got, err := FromPathOrEnv("api-token", tokenFile, "AGENTMESH_TEST_SECRET")
		if err != nil || got != "file-secret" {
			t.Errorf("got %q, %v; want file-secret", got, err)
		}
		if _, still := os.LookupEnv("AGENTMESH_TEST_SECRET"); still {
			t.Error("env var must be removed from the process environment")
		}
	})

	t.Run("env fallback trimmed", func(t *testing.T) {
		t.Setenv("AGENTMESH_TEST_SECRET", " env-secret \n")
		got, err := FromPathOrEnv("api-token", "", "AGENTMESH_TEST_SECRET")
		if err != nil || got != "env-secret" {
			t.Errorf("got %q, %v; want env-secret", got, err)
		}
		if _, still := os.LookupEnv("AGENTMESH_TEST_SECRET"); still {
			t.Error("env var must be removed from the process environment")
		}
	})

	t.Run("unset means unconfigured", func(t *testing.T) {
		got, err := FromPathOrEnv("api-token", "", "AGENTMESH_TEST_SECRET_UNSET")
		if err != nil || got != "" {
			t.Errorf("got %q, %v; want empty", got, err)
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		if _, err := FromPathOrEnv("api-token", filepath.Join(dir, "nope"), "AGENTMESH_TEST_SECRET"); err == nil {
			t.Error("expected error for missing file")
		}
	})

	t.Run("empty file is an error", func(t *testing.T) {
		empty := filepath.Join(dir, "empty")
		if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := FromPathOrEnv("api-token", empty, "AGENTMESH_TEST_SECRET"); err == nil {
			t.Error("expected error for empty file")
		}
	})
}
