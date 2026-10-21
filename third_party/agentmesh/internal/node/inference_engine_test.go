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
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/google/agentmesh/api"
)

func TestOpenAIEngine_Models(t *testing.T) {
	tests := []struct {
		name       string
		basePath   string
		status     int
		body       string
		want       []string
		wantErr    bool
		wantedPath string
	}{
		{
			name:       "plain backend",
			status:     http.StatusOK,
			body:       `{"object":"list","data":[{"id":"llama-3"},{"id":"qwen-2.5"}]}`,
			want:       []string{"llama-3", "qwen-2.5"},
			wantedPath: "/v1/models",
		},
		{
			name:       "backend with path prefix",
			basePath:   "/proxy",
			status:     http.StatusOK,
			body:       `{"data":[{"id":"m1"}]}`,
			want:       []string{"m1"},
			wantedPath: "/proxy/v1/models",
		},
		{
			name:    "backend error status",
			status:  http.StatusInternalServerError,
			body:    `boom`,
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			status:  http.StatusOK,
			body:    `not-json`,
			wantErr: true,
		},
		{
			name:   "empty and missing IDs skipped",
			status: http.StatusOK,
			body:   `{"data":[{"id":""},{"id":"m2"},{}]}`,
			want:   []string{"m2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer backend.Close()

			u, err := url.Parse(backend.URL + tt.basePath)
			if err != nil {
				t.Fatalf("parse URL: %v", err)
			}
			engine := newOpenAIEngine(u, nil)
			got, err := engine.Models(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatal("Models: expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Models: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Models: got %v, want %v", got, tt.want)
			}
			if tt.wantedPath != "" && gotPath != tt.wantedPath {
				t.Errorf("backend path: got %q, want %q", gotPath, tt.wantedPath)
			}
		})
	}
}

func TestInferenceService_Models_ProbesBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gemma-3"}]}`))
	}))
	defer backend.Close()

	svc := &InferenceService{baseService: baseService{
		info:    &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_INFERENCE, Name: "llm"},
		backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: backend.URL},
	}}
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	got, err := svc.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if want := []string{"gemma-3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Models: got %v, want %v", got, want)
	}
}

func TestInferenceService_Models_Uninitialized(t *testing.T) {
	svc := &InferenceService{baseService: baseService{
		info: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_INFERENCE, Name: "llm"},
	}}
	if _, err := svc.Models(context.Background()); err == nil {
		t.Fatal("Models: expected error for uninitialized service, got nil")
	}
}

func TestSingleJoiningSlash(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want string
	}{
		{name: "root backend with /v1 path", a: "", b: "/v1/chat/completions", want: "/v1/chat/completions"},
		{name: "slash backend with /v1 path", a: "/", b: "/v1/chat/completions", want: "/v1/chat/completions"},
		{name: "prefix backend with /v1 path", a: "/proxy", b: "/v1/chat/completions", want: "/proxy/v1/chat/completions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := singleJoiningSlash(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("singleJoiningSlash(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
