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

package node

import (
	"slices"
	"strings"
	"testing"
)

func TestBackendEnvStripsNodeSecrets(t *testing.T) {
	t.Setenv("AGENTMESH_API_TOKEN", "node-secret")
	t.Setenv("AGENTMESH_CLIENT_SECRET", "oidc-secret")
	t.Setenv("AGENTMESH_API_TOKEN_SUFFIX", "kept") // only exact names are stripped
	t.Setenv("HARMLESS", "kept")

	env := backendEnv(map[string]string{"BACKEND_KEY": "value"})

	for _, kv := range env {
		if strings.HasPrefix(kv, "AGENTMESH_API_TOKEN=") || strings.HasPrefix(kv, "AGENTMESH_CLIENT_SECRET=") {
			t.Errorf("node secret leaked into backend env: %s", kv)
		}
	}
	for _, want := range []string{"HARMLESS=kept", "AGENTMESH_API_TOKEN_SUFFIX=kept", "BACKEND_KEY=value"} {
		if !slices.Contains(env, want) {
			t.Errorf("backend env missing %q", want)
		}
	}
}
