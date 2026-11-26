package caller

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/agentmesh/api"
)

// One rule for agent names: the callers list exactly the names a node
// serves, and no name a shell reads as syntax.
func TestValidAgentName(t *testing.T) {
	t.Parallel()
	taken := []string{"codex", "release.bot", "release.bot.", "_bot", "x_y", "Reviewer", "a-1.b-2", strings.Repeat("a", 63)}
	refused := []string{"", "bot/x", "bot/$(id)", `a/b\'c`, "*", "*.x", "x.*", "a b", "a;b", "-a", ".a", "a..b", "a%2F", strings.Repeat("a", 64), strings.Repeat("a.", 127) + "a"}
	for _, name := range taken {
		if !ValidAgentName(name) {
			t.Errorf("%q refused", name)
		}
	}
	for _, name := range refused {
		if ValidAgentName(name) {
			t.Errorf("%q taken", name)
		}
	}
	const peerID = "12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN"
	var providers []*api.DiscoveredProvider
	for _, name := range append(slices.Clone(taken), refused...) {
		providers = append(providers, &api.DiscoveredProvider{SrvName: name, PeerId: peerID})
	}
	var listed []string
	for _, row := range ListedAgents(LocalInfo{}, "", providers).Agents {
		listed = append(listed, row.Service)
	}
	if want := slices.Sorted(slices.Values(taken)); !slices.Equal(listed, want) {
		t.Fatalf("listed %q, want %q", listed, want)
	}
}
