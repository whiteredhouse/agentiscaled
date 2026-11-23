// Package skills holds the agent skills agentiscaled ships, in the layout
// that skill installers such as `npx skills add` read (skills/<name>/SKILL.md
// and the files it links to), so that the binary prints and installs the same
// files.
package skills

import (
	"embed"
	"io/fs"
)

// Agentiscale is the agentiscale skill's SKILL.md: how an agent finds and
// calls the other agents of its network.
//
//go:embed agentiscale/SKILL.md
var Agentiscale []byte

//go:embed agentiscale
var agentiscaleDir embed.FS

// AgentiscaleFiles holds every file of the agentiscale skill, SKILL.md and
// the references it links to, at their paths in the skill's directory.
var AgentiscaleFiles = func() fs.FS {
	sub, err := fs.Sub(agentiscaleDir, AgentiscaleName)
	if err != nil {
		panic(err)
	}
	return sub
}()

// AgentiscaleName is the skill's name, the directory it installs into.
const AgentiscaleName = "agentiscale"
