package main

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
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type agentRow struct {
	Service     string `json:"service"`
	Location    string `json:"location"`
	PeerID      string `json:"peer_id,omitempty"`
	Description string `json:"description,omitempty"`
}

type listAgentsIn struct {
	Location string `json:"location,omitempty" jsonschema:"Filter by local (this node), mesh (other nodes), or cloud. Omit to list all three."`
}

type listAgentsOut struct {
	Agents  []agentRow `json:"agents"`
	Warning string     `json:"warning,omitempty"`
}

func validateAgentLocation(location string) error {
	switch location {
	case "", "local", "mesh", "cloud":
		return nil
	default:
		return fmt.Errorf("unknown agent location %q; choose local, mesh, or cloud", location)
	}
}

func (t *a2aTasks) listAgents(ctx context.Context, _ *mcp.CallToolRequest, input listAgentsIn) (*mcp.CallToolResult, listAgentsOut, error) {
	rows, err := t.discoverAgents(ctx, input.Location)
	if err != nil && len(rows) == 0 {
		return nil, listAgentsOut{}, err
	}
	out := listAgentsOut{Agents: rows}
	if err != nil {
		out.Warning = err.Error()
	}
	for i := range out.Agents {
		if out.Agents[i].Location == "local" {
			// Keep the local MCP task target consistent across discovery,
			// submission and subsequent calls under the same task grant.
			out.Agents[i].PeerID = ""
		}
	}
	b, _ := json.Marshal(out)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, out, nil
}

// discoverAgents returns partial rows with an error when a source is unavailable.
// Cloud and local views use the caller's socket clients. For a delegated run
// only, the node supplies the same A2A directory metadata its owner can see.
// Invocation still uses the run's A2A-only credential on every target.
func (t *a2aTasks) discoverAgents(ctx context.Context, location string) ([]agentRow, error) {
	if err := validateAgentLocation(location); err != nil {
		return nil, err
	}
	if location == "cloud" {
		return t.hostedCatalog(ctx)
	}
	rows := []agentRow{}
	var info localAgentInfo
	var failures []error
	if t.local != nil {
		var err error
		info, err = t.localInfo(ctx)
		switch {
		case err == nil && location != "mesh":
			for _, name := range info.Services {
				if name != t.self {
					rows = append(rows, agentRow{Location: "local", Service: name, PeerID: info.PeerID})
				}
			}
		case err != nil && location != "mesh" && !errors.Is(err, os.ErrNotExist):
			failures = append(failures, fmt.Errorf("list local agents: %w", err))
		}
	}
	if location == "" && info.Hosted {
		cloud, err := t.optionalHostedCatalog(ctx)
		if err != nil {
			failures = append(failures, fmt.Errorf("list cloud agents: %w", err))
		}
		rows = append(rows, cloud...)
	}
	if location != "local" {
		lookup, cancel := context.WithTimeout(ctx, 30*time.Second)
		var found []agentRow
		var err error
		if t.meshCatalog != nil {
			var providers []*api.DiscoveredProvider
			providers, err = t.meshCatalog(lookup)
			found = meshAgentRows(providers, "")
		} else {
			found, err = meshAgents(lookup, t.http, t.base, "")
		}
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("list mesh agents: %w", err))
		}
		for _, row := range found {
			if row.PeerID != info.PeerID {
				rows = append(rows, row)
			}
		}
	}
	slices.SortFunc(rows, compareAgentRows)
	rows = slices.CompactFunc(rows, func(a, b agentRow) bool {
		return a.Service == b.Service && a.Location == b.Location && a.PeerID == b.PeerID
	})
	return rows, errors.Join(failures...)
}

func compareAgentRows(a, b agentRow) int {
	return cmp.Or(cmp.Compare(a.Service, b.Service), cmp.Compare(a.Location, b.Location), cmp.Compare(a.PeerID, b.PeerID))
}

// The node discovers a fresh, unordered snapshot on every GET. Read one
// bounded snapshot, as offsets into successive snapshots can omit agents.
func meshAgents(ctx context.Context, client *http.Client, base, name string) ([]agentRow, error) {
	if client == nil {
		return nil, errors.New("mesh API unavailable")
	}
	q := url.Values{"type": {"a2a"}, "limit": {strconv.Itoa(math.MaxInt)}}
	if name != "" {
		q.Set("name", name)
		q.Set("limit", "100")
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
		return nil, errors.New(resp.Status)
	}
	const limit = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, errors.New("mesh discovery response exceeds size limit")
	}
	var providers []*api.DiscoveredProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, err
	}
	return meshAgentRows(providers, name), nil
}

func meshAgentRows(providers []*api.DiscoveredProvider, name string) []agentRow {
	rows := make([]agentRow, 0, len(providers))
	for _, p := range providers {
		service, rawPeer := p.GetSrvName(), p.GetPeerId()
		id, err := peer.Decode(rawPeer)
		if err == nil && serviceName.MatchString(service) && (name == "" || name == service) {
			rows = append(rows, agentRow{Service: service, Location: "mesh", PeerID: id.String(), Description: p.GetSrvDescription()})
		}
	}
	return rows
}
