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

// Package secrets resolves secret configuration for Agent Mesh binaries.
//
// Daemon-lifetime secrets are never accepted as flag values: command lines
// leak via /proc/<pid>/cmdline (world-readable), shell history, CI logs, and
// pod specs. They are accepted from a file (--<name>-path, the recommended
// production channel — e.g. a Kubernetes Secret volume) or an environment
// variable (developer convenience; /proc/<pid>/environ is owner-only).
package secrets

import (
	"fmt"
	"os"
	"strings"
)

// FromPathOrEnv resolves a daemon-lifetime secret: the file at path wins,
// the environment variable is the fallback. File and env contents are
// whitespace-trimmed; a configured but empty file is an error. An empty
// result means the secret was not configured at all.
//
// The environment variable is removed from this process's environment in
// either case, so subprocesses (MCP command backends, re-executed daemons)
// do not inherit it.
func FromPathOrEnv(name, path, envVar string) (string, error) {
	fromEnv := strings.TrimSpace(os.Getenv(envVar))
	_ = os.Unsetenv(envVar)
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read --%s-path: %w", name, err)
		}
		secret := strings.TrimSpace(string(data))
		if secret == "" {
			return "", fmt.Errorf("--%s-path file %s is empty", name, path)
		}
		return secret, nil
	}
	return fromEnv, nil
}

// Resolve returns a one-shot secret configured either directly (value) or
// via a file (path). Setting both is an error; file contents are
// whitespace-trimmed and must be non-empty. Direct values are tolerated for
// short-lived interactive flows (e.g. enrollment); daemon-lifetime secrets
// use FromPathOrEnv instead.
func Resolve(name, value, path string) (string, error) {
	switch {
	case value != "" && path != "":
		return "", fmt.Errorf("--%s and --%s-path are mutually exclusive", name, name)
	case path != "":
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read --%s-path: %w", name, err)
		}
		secret := strings.TrimSpace(string(data))
		if secret == "" {
			return "", fmt.Errorf("--%s-path file %s is empty", name, path)
		}
		return secret, nil
	default:
		return value, nil
	}
}
