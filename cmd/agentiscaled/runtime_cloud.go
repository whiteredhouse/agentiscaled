// The hosted node keeps the normal A2A service, sender checks, delivery
// journal, cancellation and deadlines while its worker sleeps between turns.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
)

type cloudRuntime struct {
	url    string
	token  string
	client *http.Client
}

func newCloudRuntime(path string) (*cloudRuntime, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0o077 != 0 || stat.Size() > 8192 {
		return nil, errors.New("agent runner requires a private, owner-only configuration file")
	}
	var config struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) > 8192 || json.Unmarshal(data, &config) != nil || config.Token == "" || strings.ContainsAny(config.Token, " \r\n\t") {
		return nil, errors.New("invalid agent runner configuration")
	}
	endpoint, err := url.Parse(config.URL)
	if err != nil || endpoint.Scheme != "http" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.Path == "" {
		return nil, errors.New("agent runner needs a loopback HTTP turn endpoint")
	}
	ip, err := netip.ParseAddr(endpoint.Hostname())
	if err != nil || !ip.IsLoopback() {
		return nil, errors.New("agent runner needs a loopback HTTP turn endpoint")
	}
	return &cloudRuntime{
		url: strings.TrimSuffix(config.URL, "/"), token: config.Token,
		client: &http.Client{
			Transport:     &http.Transport{MaxIdleConnsPerHost: 2, IdleConnTimeout: time.Minute},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Timeout:       taskDeadline,
		},
	}, nil
}

func (r *cloudRuntime) Describe(card *a2a.AgentCard) {
	card.Description = "A hosted Claude Code agent that reviews changes since the latest release of the owner's selected GitHub repository."
	card.Skills = []a2a.AgentSkill{{
		ID: "task", Name: "Release review", Tags: []string{"claude", "github", "release"},
		Description: "Use the read-only GitHub connector to summarize the latest release, subsequent commits and changed paths. For a repository without a release, review recent commits.",
		Examples:    []string{"What changed since the last release, and what should a maintainer review?"},
	}}
}

func (r *cloudRuntime) Run(ctx context.Context, _ string, turn taskRequest) (turnResult, error) {
	if err := ctx.Err(); err != nil {
		return turnResult{}, err
	}
	ctx, stop := context.WithTimeout(ctx, taskDeadline)
	defer stop()
	deadline, _ := ctx.Deadline()
	body, err := json.Marshal(struct {
		DeadlineMS int64  `json:"deadline_ms"`
		Sender     string `json:"sender"`
		Text       string `json:"text"`
		Session    string `json:"session,omitempty"`
	}{deadline.UnixMilli(), turn.Sender, turn.Text, turn.Session})
	if err != nil {
		return turnResult{}, err
	}
	endpoint := r.url + "/" + uuid.NewString()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return turnResult{}, errors.New("invalid cloud turn endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	turn.progress("Connecting to the cloud worker")
	resp, err := r.client.Do(req)
	if err != nil {
		// The supervisor may already have woken the worker and started a
		// turn. Use an independent context when the caller has gone away.
		r.cancel(endpoint)
		if ctx.Err() != nil {
			return turnResult{}, ctx.Err()
		}
		return turnResult{}, errors.New("cloud turn interrupted")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return turnResult{}, fmt.Errorf("cloud worker could not run the turn (HTTP %d)", resp.StatusCode)
	}
	const maxResult = 1 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResult+1))
	if err != nil {
		r.cancel(endpoint)
		return turnResult{}, errors.New("cloud turn interrupted")
	}
	var out turnResult
	if len(data) > maxResult || json.Unmarshal(data, &out) != nil || strings.TrimSpace(out.Text) == "" || (out.Question && out.Session == "") {
		return turnResult{}, errors.New("invalid result from cloud worker")
	}
	return out, nil
}

func (r *cloudRuntime) cancel(endpoint string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	if resp, err := r.client.Do(req); err == nil {
		resp.Body.Close()
	}
}
