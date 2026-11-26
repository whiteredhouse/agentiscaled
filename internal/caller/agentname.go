package caller

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/google/agentmesh/api"
)

// agentName is a plain DNS name: labels of letters, digits, - and _, up to
// 63 characters each, joined by dots, with an optional final dot (or _) as
// the service registry takes. The registry takes more (a path after the
// name, wildcards), but an agent's name is shown and pasted into commands,
// so it is kept to what no common shell reads as syntax.
var agentName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,62}(\.[A-Za-z0-9_][A-Za-z0-9_-]{0,62})*[._]?$`)

// ValidAgentName tells whether name can be an agent's name in the network.
// It is the one rule for agent names (AgentName).
func ValidAgentName(name string) bool {
	return len(name) <= 253 && agentName.MatchString(name) && api.ValidateServiceFormat(api.ServiceTypeStringA2A+"://"+name) == nil
}

// AgentName is an agent's name in the network, one that follows the rule
// (ValidAgentName). Names come in as text at a few edges (settings, the
// local catalog, discovery, command arguments, tool calls) and are parsed
// there with ParseAgentName, or by decoding JSON or text, which parses too;
// past those edges, code takes an AgentName, so no name that breaks the
// rule reaches routing, drains or output. Only a parse, or a constant
// written in code, makes one.
type AgentName string

// ParseAgentName gives name as an AgentName, or an error if it breaks the
// rule. The error shows name only escaped (ShownAgentName).
func ParseAgentName(name string) (AgentName, error) {
	if !ValidAgentName(name) {
		return "", fmt.Errorf("agent name %s: the network takes a DNS name: letters, digits, - and _, in labels of up to 63 characters joined by dots", ShownAgentName(name))
	}
	return AgentName(name), nil
}

// ParseAgentNames parses each of names, leaving out those that break the
// rule, so that one bad name reported by another process (a node older
// than the rule, a peer) hides only itself.
func ParseAgentNames(names []string) []AgentName {
	out := make([]AgentName, 0, len(names))
	for _, n := range names {
		if name, err := ParseAgentName(n); err == nil {
			out = append(out, name)
		}
	}
	return out
}

func (n AgentName) String() string { return string(n) }

// UnmarshalText parses, so that JSON and flags give only names that follow
// the rule.
func (n *AgentName) UnmarshalText(b []byte) error {
	name, err := ParseAgentName(string(b))
	if err != nil {
		return err
	}
	*n = name
	return nil
}

// QuotedAgentName is name for a message: a valid name quoted, any other
// as ShownAgentName shows it.
func QuotedAgentName(name string) string {
	if ValidAgentName(name) {
		return strconv.Quote(name)
	}
	return ShownAgentName(name)
}

// ShownAgentName is how text that may not be an agent name is printed: a
// valid name as it is, any other percent-encoded (every byte outside
// letters, digits, '.', '_' and '-' as %XX, an empty name as "%"), so that
// it reaches no shell or terminal as syntax.
func ShownAgentName(name string) string {
	if ValidAgentName(name) {
		return name
	}
	if name == "" {
		return "%"
	}
	b := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
			b = append(b, c)
		} else {
			b = fmt.Appendf(b, "%%%02X", c)
		}
	}
	return string(b)
}
