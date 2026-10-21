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

package api

import (
	"strings"
	"testing"
)

func TestValidateLabelPattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		wantErr bool
	}{
		{name: "any label", pattern: "*"},
		{name: "any value of a key", pattern: "region=*"},
		{name: "exact pair", pattern: "region=us-east-1"},

		{name: "empty", pattern: "", wantErr: true},
		{name: "no separator", pattern: "region", wantErr: true},
		{name: "empty key", pattern: "=us-east-1", wantErr: true},
		{name: "empty value", pattern: "region=", wantErr: true},
		{name: "invalid key charset", pattern: "reg ion=us", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateLabelPattern(tt.pattern)
			if tt.wantErr && err == nil {
				t.Errorf("ValidateLabelPattern(%q) = nil, want an error", tt.pattern)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ValidateLabelPattern(%q) = %v, want nil", tt.pattern, err)
			}
		})
	}
}

// TestLabelPatternsAllow covers the gate that turns a self-declared label into
// one the control plane is willing to sign. Fail-closed: no grant means no
// label, because otherwise a node could declare region="us-east-1" and satisfy
// every peer that requires it.
func TestLabelPatternsAllow(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		labels   map[string]string
		wantErr  bool
	}{
		{
			name:   "no labels declared needs no grant",
			labels: nil,
		},
		{
			name:    "no grant refuses any label",
			labels:  map[string]string{"region": "us-east-1"},
			wantErr: true,
		},
		{
			name:     "exact grant admits the exact pair",
			patterns: []string{"region=us-east-1"},
			labels:   map[string]string{"region": "us-east-1"},
		},
		{
			name:     "exact grant refuses another value",
			patterns: []string{"region=us-east-1"},
			labels:   map[string]string{"region": "eu-west-1"},
			wantErr:  true,
		},
		{
			name:     "key wildcard admits any value",
			patterns: []string{"region=*"},
			labels:   map[string]string{"region": "eu-west-1"},
		},
		{
			name:     "key wildcard does not admit another key",
			patterns: []string{"region=*"},
			labels:   map[string]string{"team": "platform"},
			wantErr:  true,
		},
		{
			name:     "bare wildcard admits everything",
			patterns: []string{"*"},
			labels:   map[string]string{"region": "eu-west-1", "team": "platform"},
		},
		{
			// Every declared label must be covered, not just one of them.
			name:     "one uncovered label refuses the whole set",
			patterns: []string{"region=*"},
			labels:   map[string]string{"region": "eu-west-1", "team": "platform"},
			wantErr:  true,
		},
		{
			name:     "grants from several roles combine",
			patterns: []string{"region=*", "team=platform"},
			labels:   map[string]string{"region": "eu-west-1", "team": "platform"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := LabelPatternsAllow(tt.patterns, tt.labels)
			if tt.wantErr && err == nil {
				t.Errorf("LabelPatternsAllow(%v, %v) = nil, want an error", tt.patterns, tt.labels)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("LabelPatternsAllow(%v, %v) = %v, want nil", tt.patterns, tt.labels, err)
			}
		})
	}
}

// The error names the label that was refused, so an operator can tell which
// grant is missing rather than being told only that something was.
func TestLabelPatternsAllowNamesTheRefusedLabel(t *testing.T) {
	err := LabelPatternsAllow([]string{"region=*"}, map[string]string{"team": "platform"})
	if err == nil {
		t.Fatal("LabelPatternsAllow accepted an ungranted label")
	}
	if !strings.Contains(err.Error(), "team") || !strings.Contains(err.Error(), "platform") {
		t.Errorf("error %q does not name the refused label", err)
	}
}
