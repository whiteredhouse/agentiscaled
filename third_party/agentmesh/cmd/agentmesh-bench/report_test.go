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

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/agentmesh/internal/bench"
)

func TestSortByOrdersNumericLabelsNumerically(t *testing.T) {
	// Lexical ordering would put 128 agents between 1 and 16, and a trend
	// read off a table in that order would be nonsense.
	observations := []observation{
		{Labels: map[string]string{"agents": "128"}},
		{Labels: map[string]string{"agents": "1"}},
		{Labels: map[string]string{"agents": "16"}},
		{Labels: map[string]string{"agents": "2"}},
	}
	sortBy(observations, "agents")

	var got []string
	for _, o := range observations {
		got = append(got, o.Labels["agents"])
	}
	want := []string{"1", "2", "16", "128"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestDeltaSumsTheMovementAcrossEverySource(t *testing.T) {
	// Counters are cumulative and there is one per sandbox, so a run's flow
	// count is the movement summed over all of them, not any single value.
	obs := observation{
		Before: map[string]map[string]float64{
			"box-1": {"agentmesh_box_flows_total{outcome=allowed}": 10},
			"box-2": {"agentmesh_box_flows_total{outcome=allowed}": 5},
		},
		After: map[string]map[string]float64{
			"box-1": {"agentmesh_box_flows_total{outcome=allowed}": 60},
			"box-2": {"agentmesh_box_flows_total{outcome=allowed}": 45},
		},
	}

	if got := delta(obs, "agentmesh_box_flows_total{outcome=allowed}"); got != 90 {
		t.Errorf("delta = %v, want 90 (50 + 40)", got)
	}
}

func TestGaugeIsReadAsALevelNotAnAccumulation(t *testing.T) {
	// Memory is a level. Reporting its difference across a run would say a
	// sandbox costs nothing, because it was already resident when the run
	// started, which is the opposite of what the density question asks.
	obs := observation{
		Before: map[string]map[string]float64{
			"box-1": {"process_resident_memory_bytes": 20},
			"box-2": {"process_resident_memory_bytes": 20},
		},
		After: map[string]map[string]float64{
			"box-1": {"process_resident_memory_bytes": 21},
			"box-2": {"process_resident_memory_bytes": 23},
		},
	}

	total, sources := gauge(obs, "process_resident_memory_bytes")
	if total != 44 {
		t.Errorf("total = %v, want 44", total)
	}
	if sources != 2 {
		t.Errorf("sources = %v, want 2", sources)
	}
	if got := delta(obs, "process_resident_memory_bytes"); got == total {
		t.Error("a gauge read as a counter produced the same figure, so the distinction is not being made")
	}
}

func TestGaugeIgnoresSourcesThatNeverReportedIt(t *testing.T) {
	// Averaging over sources that did not export the series would divide by
	// the wrong number and understate the per-sandbox cost.
	obs := observation{After: map[string]map[string]float64{
		"box-1": {"process_resident_memory_bytes": 10},
		"box-2": {"something_else": 99},
	}}

	total, sources := gauge(obs, "process_resident_memory_bytes")
	if total != 10 || sources != 1 {
		t.Errorf("gauge = (%v, %v), want (10, 1)", total, sources)
	}
}

func TestWriteObservationWithNonFiniteMetrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("unavailable NaN\npositive +Inf\nnegative -Inf\nfinite 3\n"))
	}))
	defer server.Close()

	metrics, err := scrapeAll(context.Background(), []string{server.URL})
	if err != nil {
		t.Fatalf("scrapeAll: %v", err)
	}
	path := filepath.Join(t.TempDir(), "observation.json")
	wantReport := &bench.Report{Requests: 1, Succeeded: 1}
	if err := write(path, observation{Report: wantReport, After: metrics}); err != nil {
		t.Fatalf("write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got observation
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.Report == nil || got.Report.Requests != 1 || got.Report.Succeeded != 1 {
		t.Errorf("report = %+v, want %+v", got.Report, wantReport)
	}
	if values := got.After[server.URL]; len(values) != 1 || values["finite"] != 3 {
		t.Errorf("metrics = %v, want only finite=3", values)
	}
}

func TestRenderPutsEveryObservationInTheTable(t *testing.T) {
	observations := []observation{
		{
			Labels: map[string]string{"agents": "1"},
			Report: &bench.Report{Requests: 100, Concurrency: 4, Succeeded: 100, Throughput: 250},
		},
		{
			Labels: map[string]string{"agents": "64"},
			Report: &bench.Report{Requests: 100, Concurrency: 4, Succeeded: 99, Failed: 1, Throughput: 120},
		},
	}

	table := render(observations, "agents", nil, nil)
	lines := strings.Split(strings.TrimSpace(table), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want a header, a rule and two rows:\n%s", len(lines), table)
	}
	if !strings.Contains(lines[0], "agents") {
		t.Errorf("header does not name the varied label: %q", lines[0])
	}
	if !strings.Contains(lines[3], "64") {
		t.Errorf("last row is not the 64-agent run: %q", lines[3])
	}
}
