package caller

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p/core/peer"
)

type AgentRow struct {
	Service     AgentName `json:"service"`
	PeerID      string    `json:"peer_id,omitempty"`
	Description string    `json:"description,omitempty"`
	// Draining, when set, says that the agent finishes the tasks it has
	// but takes no new ones, and why.
	Draining *Drain `json:"draining,omitempty"`
}

type AgentList struct {
	Agents []AgentRow `json:"agents"`
}

// ListedAgents combines local services and the remote mesh catalog. Each
// (name, peer) is separately addressable, even when peers share a name.
func ListedAgents(info LocalInfo, self AgentName, providers []*api.DiscoveredProvider) AgentList {
	out := AgentList{Agents: []AgentRow{}}
	seen := make(map[Target]bool)
	add := func(row AgentRow) {
		target := Target{Service: row.Service, PeerID: row.PeerID}
		if !seen[target] {
			out.Agents = append(out.Agents, row)
			seen[target] = true
		}
	}
	for _, name := range info.Services {
		if name != self {
			row := AgentRow{Service: name}
			if d, ok := info.Draining[name]; ok {
				row.Draining = &d
			}
			add(row)
		}
	}
	for _, p := range providers {
		id := p.GetPeerId()
		if id == info.PeerID {
			continue
		}
		if _, err := peer.Decode(id); err != nil {
			continue
		}
		// A peer's name is parsed on its own: one that breaks the rule
		// hides only itself.
		name, err := ParseAgentName(p.GetSrvName())
		if err != nil {
			continue
		}
		add(AgentRow{Service: name, PeerID: id, Description: p.GetSrvDescription(), Draining: DrainFromMetadata(p.GetSrvMetadata())})
	}
	slices.SortFunc(out.Agents, func(a, b AgentRow) int {
		return cmp.Or(cmp.Compare(a.Service, b.Service), cmp.Compare(a.PeerID, b.PeerID))
	})
	return out
}

// The mesh rebuilds the provider list for each request in the order peers
// respond. Walking offsets across such lists can skip peers or count them
// twice. Request one complete snapshot and bound the bytes read from it.
func readDiscovery(ctx context.Context, client *http.Client, base, name string) ([]*api.DiscoveredProvider, error) {
	q := url.Values{"type": {"a2a"}, "limit": {strconv.Itoa(math.MaxInt)}}
	if name != "" {
		q.Set("name", name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/mesh/service/discover?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	const maxBytes = 16 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		return nil, errors.New("agent discovery exceeds 16 MiB")
	}
	var providers []*api.DiscoveredProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, err
	}
	return providers, nil
}
