package caller

import (
	"regexp"

	"github.com/google/agentmesh/api"
)

// agentName is a plain DNS name: labels of letters, digits, - and _, up to
// 63 characters each, joined by dots, with an optional final dot (or _) as
// the service registry takes. The registry takes more (a path after the
// name, wildcards), but an agent's name is shown and pasted into commands,
// so it is kept to what no common shell reads as syntax.
var agentName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,62}(\.[A-Za-z0-9_][A-Za-z0-9_-]{0,62})*[._]?$`)

// ValidAgentName tells whether name can be an agent's name in the network.
// It is the one rule for agent names: the node takes only these for the
// agents it serves, and callers list, card and send to only these.
func ValidAgentName(name string) bool {
	return len(name) <= 253 && agentName.MatchString(name) && api.ValidateServiceFormat(api.ServiceTypeStringA2A+"://"+name) == nil
}
