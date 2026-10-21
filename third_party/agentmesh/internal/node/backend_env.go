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
	"os"
	"strings"
)

// nodeSecretEnvVars are this process's own credentials. A command backend is
// the operator's program, not the node's, so it never needs them.
var nodeSecretEnvVars = []string{"AGENTMESH_API_TOKEN", "AGENTMESH_CLIENT_SECRET"}

// backendEnv builds the environment for a command-backed service: the node's
// environment minus its own secrets, plus the operator-declared extras.
func backendEnv(extra map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		if isNodeSecretEnv(kv) {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func isNodeSecretEnv(kv string) bool {
	for _, name := range nodeSecretEnvVars {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}
