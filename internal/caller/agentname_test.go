package caller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
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
		listed = append(listed, row.Service.String())
	}
	if want := slices.Sorted(slices.Values(taken)); !slices.Equal(listed, want) {
		t.Fatalf("listed %q, want %q", listed, want)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A name outside the rule in the local catalog, as a node older than the
// rule may report, is left out on its own: the node's other agents stay
// reachable.
func TestLocalCatalogDropsOnlyInvalidNames(t *testing.T) {
	t.Parallel()
	const peerID = "12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN"
	body, _ := json.Marshal(map[string]any{"peer_id": peerID, "services": []string{"codex", strings.Repeat("a", 64), "bot/x"}, "draining": map[string]Drain{"bot/x": {}, "codex": {Reason: "r"}}})
	client := &Client{Local: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}, nil
	})}}
	info, err := client.LocalInfo(context.Background())
	if _, drained := info.Draining["codex"]; err != nil || !slices.Equal(info.Services, []AgentName{"codex"}) || len(info.Draining) != 1 || !drained {
		t.Fatalf("local info: %+v %v", info, err)
	}
	endpoint, _, err := client.selectRoute(context.Background(), Target{Service: "codex"})
	if err != nil || endpoint != "http://agentiscaled/a2a/"+peerID+"/codex" {
		t.Fatalf("route to codex: %q %v", endpoint, err)
	}
}
