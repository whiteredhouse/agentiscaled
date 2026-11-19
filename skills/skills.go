// Package skills holds the agent skills agentiscaled ships, in the layout
// that skill installers such as `npx skills add` read (skills/<name>/SKILL.md),
// so that the binary prints and installs the same files.
package skills

import _ "embed"

// Agentiscale is the agentiscale skill's SKILL.md: how an agent finds and
// calls the other agents of its network.
//
//go:embed agentiscale/SKILL.md
var Agentiscale []byte

// AgentiscaleName is the skill's name, the directory it installs into.
const AgentiscaleName = "agentiscale"
