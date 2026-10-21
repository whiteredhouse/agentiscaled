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

// Package agentmeshskill embeds the Agent Mesh agent skill document so it ships inside the
// binaries that install it. SKILL.md stays the single source of truth.
package agentmeshskill

import _ "embed"

// Markdown is the SKILL.md document handed to local AI agents.
//
//go:embed SKILL.md
var Markdown string
