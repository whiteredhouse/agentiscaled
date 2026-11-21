package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const hostedPrefix = "hosted:"
const maxHostedResponse = 2 << 20

func canonicalID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

func hostedRequest(ctx context.Context, client *http.Client, method, path string, body []byte, dst any) (int, error) {
	if client == nil {
		return 0, errors.New("the running node has no hosted agent API")
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agentiscaled"+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	noRedirects := *client
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirects.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("hosted agent service: HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxHostedResponse+1))
	if err != nil {
		return resp.StatusCode, err
	}
	if len(raw) > maxHostedResponse || json.Unmarshal(raw, dst) != nil {
		return resp.StatusCode, errors.New("invalid hosted service response")
	}
	return resp.StatusCode, nil
}

func (t *a2aTasks) hostedCatalog(ctx context.Context) ([]agentRow, error) {
	var catalog struct {
		Agents []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"agents"`
	}
	status, err := hostedRequest(ctx, t.local, http.MethodGet, hostedPath, nil, &catalog)
	if status == http.StatusNotFound {
		return []agentRow{}, nil
	}
	if err != nil {
		return nil, err
	}
	rows := make([]agentRow, 0, len(catalog.Agents))
	for _, a := range catalog.Agents {
		if !canonicalID(a.ID) || !serviceName.MatchString(a.Name) {
			return nil, errors.New("invalid hosted catalog entry")
		}
		rows = append(rows, agentRow{Service: a.Name, PeerID: hostedPrefix + a.ID})
	}
	return rows, nil
}

// Reserve time for native discovery and dispatch when hosted control is slow.
func (t *a2aTasks) optionalHostedCatalog(ctx context.Context) ([]agentRow, error) {
	budget := 2 * time.Second
	if end, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(end)/4)
	}
	lookup, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return t.hostedCatalog(lookup)
}

func (t *a2aTasks) listHostedAgents(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listAgentsOut, error) {
	rows, err := t.hostedCatalog(ctx)
	if err != nil {
		return nil, listAgentsOut{}, err
	}
	out := listAgentsOut{Agents: rows}
	b, _ := json.Marshal(out)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, out, nil
}

func (t *a2aTasks) hostedAgent(ctx context.Context, target taskTarget) (agentRow, error) {
	if !serviceName.MatchString(target.Service) || !canonicalID(strings.TrimPrefix(target.PeerID, hostedPrefix)) {
		return agentRow{}, errors.New("invalid hosted agent target")
	}
	rows, err := t.hostedCatalog(ctx)
	if err != nil {
		return agentRow{}, err
	}
	for _, row := range rows {
		if row.Service == target.Service && row.PeerID == target.PeerID {
			return row, nil
		}
	}
	return agentRow{}, fmt.Errorf("hosted agent %s is unavailable or not accessible as %s", target.PeerID, target.Service)
}

func (t *a2aTasks) hostedCard(ctx context.Context, target taskTarget) (*mcp.CallToolResult, any, error) {
	row, err := t.hostedAgent(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	card := map[string]any{"name": row.Service, "peer_id": row.PeerID, "transport": "agentiscale-hosted", "description": "Review the latest release, commits, and changed paths in the connected repository.", "skills": []string{"release-review"}}
	b, _ := json.Marshal(card)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

type hostedSubmission struct {
	RequestID string `json:"request_id"`
	TaskID    string `json:"task_id,omitempty"`
	Text      string `json:"text"`
}

func (t *a2aTasks) hostedTask(ctx context.Context, target taskTarget, id string, method string, cancel bool, input *hostedSubmission) (taskOut, error) {
	agentID := strings.TrimPrefix(target.PeerID, hostedPrefix)
	if !canonicalID(agentID) || !serviceName.MatchString(target.Service) {
		return taskOut{}, errors.New("invalid hosted agent target")
	}
	path := hostedPath + "/" + agentID + "/tasks"
	var body []byte
	if input != nil {
		if !canonicalID(input.RequestID) || (input.TaskID != "" && !canonicalID(input.TaskID)) {
			return taskOut{}, errors.New("invalid hosted request or task ID")
		}
		body, _ = json.Marshal(input)
	} else {
		if !canonicalID(id) {
			return taskOut{}, errors.New("invalid hosted task ID")
		}
		path += "/" + id
		if cancel {
			path += "/cancel"
		}
	}
	var wire struct {
		TaskID  string `json:"task_id"`
		State   string `json:"state"`
		Message string `json:"message"`
	}
	status, err := hostedRequest(ctx, t.local, method, path, body, &wire)
	if input != nil && err != nil && uncertainSubmission(status) && ctx.Err() == nil {
		// A lost response can mean the task was accepted. Reuse the exact
		// request ID so the durable service can return that acceptance.
		status, err = hostedRequest(ctx, t.local, method, path, body, &wire)
	}
	if err != nil {
		if status == 404 && id != "" {
			return taskOut{}, taskNotFound(target, id)
		}
		if input != nil && uncertainSubmission(status) {
			return taskOut{}, fmt.Errorf("hosted submission %s could not be confirmed: %w", input.RequestID, err)
		}
		return taskOut{}, err
	}
	if !canonicalID(wire.TaskID) || (id != "" && wire.TaskID != id) || (input != nil && input.TaskID != "" && input.TaskID != wire.TaskID) {
		return taskOut{}, errors.New("invalid hosted task response")
	}
	out := taskOut{TaskID: wire.TaskID, State: wire.State, Message: wire.Message}
	switch wire.State {
	case "submitted", "working", "input_required":
	case "completed":
		out.Result = wire.Message
		out.Terminal = wire.State
	case "cancelled", "failed", "timeout", "rejected":
		out.Terminal = wire.State
	default:
		return taskOut{}, errors.New("unknown hosted task state")
	}
	return out, nil
}

// A failed transport, unreadable success response, or server error may have
// happened after the service committed the submission.
func uncertainSubmission(status int) bool {
	return status == 0 || (status >= 200 && status < 300) || status >= 500
}

func (t *a2aTasks) watchHosted(ctx context.Context, target taskTarget, id string, wait time.Duration, notify func(string)) (taskOut, error) {
	until := time.Now().Add(wait)
	interval := time.Second
	var last taskOut
	for {
		current, err := t.hostedTask(ctx, target, id, http.MethodGet, false, nil)
		if err != nil {
			return last, err
		}
		if current.Terminal != "" || current.State == "input_required" || time.Until(until) <= 0 {
			return current, nil
		}
		if notify != nil && current.Message != "" && current.Message != last.Message {
			notify(current.Message)
		}
		last = current
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(min(interval, time.Until(until))):
			interval = min(5*time.Second, 2*interval)
		}
	}
}
