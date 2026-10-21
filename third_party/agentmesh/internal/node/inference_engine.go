// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// InferenceEngine is the connector between Agent Mesh and an inference backend.
// Implementations adapt one backend family; the service and facade layers
// stay backend-agnostic.
type InferenceEngine interface {
	// Models returns the model IDs the backend currently serves.
	Models(ctx context.Context) ([]string, error)
}

// openAIEngine adapts any OpenAI-compatible HTTP backend
// (vLLM, ollama, llama.cpp, MaaS providers).
type openAIEngine struct {
	baseURL *url.URL
	client  *http.Client
}

func newOpenAIEngine(baseURL *url.URL, client *http.Client) *openAIEngine {
	if client == nil {
		client = http.DefaultClient
	}
	return &openAIEngine{baseURL: baseURL, client: client}
}

func (e *openAIEngine) Models(ctx context.Context) ([]string, error) {
	u := *e.baseURL
	u.Path = singleJoiningSlash(e.baseURL.Path, "/v1/models")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend %s returned status %d for /v1/models", e.baseURL.Host, resp.StatusCode)
	}
	return decodeModelIDs(resp.Body)
}

// decodeModelIDs parses an OpenAI-style /v1/models list response.
func decodeModelIDs(r io.Reader) ([]string, error) {
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&list); err != nil {
		return nil, fmt.Errorf("invalid models response: %w", err)
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}
