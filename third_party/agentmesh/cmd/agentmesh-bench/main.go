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

// Command agentmesh-bench measures what an agent experiences when it reaches the
// mesh through a sandbox boundary.
//
// It issues a fixed workload rather than an interesting one. The chaos agent
// in this repo is driven by a real model and is the right tool for asking
// whether the mesh survives contact with an autonomous caller; it is the wrong
// tool for asking how long something takes, because it never asks twice for
// the same thing. This asks for exactly the same thing every time, so two runs
// differ only where the mesh differs.
//
// One invocation is one observation: it records the workload, the latencies it
// saw, and what the processes involved reported about themselves before and
// after, into a single JSON document meant to be kept.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/bench"
	"github.com/google/agentmesh/internal/version"
)

// observation is one complete, self-describing measurement.
type observation struct {
	// Labels record the conditions the operator varied, such as how many
	// agents were running. Nothing infers them, because nothing can.
	Labels map[string]string `json:"labels,omitempty"`

	Started time.Time     `json:"started"`
	Report  *bench.Report `json:"report"`

	// Before and after are keyed by scrape source. Counters are cumulative,
	// so a report quotes the difference, and keeping both ends means the
	// difference can be recomputed rather than trusted.
	Before map[string]map[string]float64 `json:"metrics_before,omitempty"`
	After  map[string]map[string]float64 `json:"metrics_after,omitempty"`
}

func main() {
	var (
		socket      string
		unixTarget  string
		target      string
		method      string
		body        string
		headers     []string
		requests    int
		concurrency int
		warmup      int
		newFlow     bool
		timeout     time.Duration
		scrape      []string
		labels      []string
		out         string
	)

	rootCmd := &cobra.Command{
		Use:     "agentmesh-bench",
		Version: version.String(),
		Short:   "Measure the mesh from where an agent stands",
	}

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Issue a fixed workload and record what it cost",
		Long: "Issues a fixed workload through a sandbox boundary and records what it cost,\n" +
			"alongside what the processes involved reported about themselves.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			header, err := parseHeaders(headers)
			if err != nil {
				return err
			}
			tags, err := parseLabels(labels)
			if err != nil {
				return err
			}

			obs := observation{Labels: tags, Started: time.Now()}

			obs.Before, err = scrapeAll(cmd.Context(), scrape)
			if err != nil {
				return err
			}

			obs.Report, err = bench.Run(cmd.Context(), bench.Options{
				Socket:            socket,
				UnixTarget:        unixTarget,
				Target:            target,
				Method:            method,
				Body:              []byte(body),
				Header:            header,
				Requests:          requests,
				Concurrency:       concurrency,
				Warmup:            warmup,
				NewFlowPerRequest: newFlow,
				Timeout:           timeout,
			})
			if err != nil {
				return err
			}

			obs.After, err = scrapeAll(cmd.Context(), scrape)
			if err != nil {
				return err
			}

			return write(out, obs)
		},
	}

	flags := runCmd.Flags()
	flags.StringVar(&socket, "socket", "", "Sandbox boundary Unix socket (HTTP CONNECT) to measure through; omit to measure the same workload without a boundary, which is the baseline")
	flags.StringVar(&unixTarget, "target-unix", "", "Dial the target over this Unix socket rather than resolving its host, so a baseline needs no relay in the path")
	flags.StringVar(&target, "target", "", "URL to request, e.g. http://mesh.alt/v1/models (required)")
	flags.StringVar(&method, "method", "GET", "HTTP method")
	flags.StringVar(&body, "body", "", "Request body")
	flags.StringArrayVar(&headers, "header", nil, "Request header as Name: value, repeatable")
	flags.IntVar(&requests, "requests", 100, "Requests to issue after warmup")
	flags.IntVar(&concurrency, "concurrency", 1, "Requests in flight at once")
	flags.IntVar(&warmup, "warmup", 10, "Requests issued before measurement starts, reported separately")
	flags.BoolVar(&newFlow, "new-flow-per-request", false, "Open a fresh boundary flow per request, measuring admission rather than transfer")
	flags.DurationVar(&timeout, "timeout", 30*time.Second, "Bound on a single request")
	flags.StringArrayVar(&scrape, "scrape", nil, "Metrics endpoint to record before and after the run, repeatable")
	flags.StringArrayVar(&labels, "label", nil, "Condition to record with the observation as name=value, repeatable")
	flags.StringVar(&out, "out", "", "File to write the observation to; default stdout")
	if err := runCmd.MarkFlagRequired("target"); err != nil {
		panic(err)
	}
	rootCmd.AddCommand(runCmd, newReportCmd(), newSTSCmd(), newJoinCmd())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "agentmesh-bench: %v\n", err)
		os.Exit(1)
	}
}

func newSTSCmd() *cobra.Command {
	var (
		opts bench.STSOptions
		out  string
	)
	cmd := &cobra.Command{
		Use:          "sts",
		Short:        "Measure Control Plane /token/exchange and /sts/token throughput, latency, and node cache hit rates",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, err := bench.RunSTS(cmd.Context(), opts)
			if err != nil {
				return err
			}
			encoded, err := json.MarshalIndent(rep, "", "  ")
			if err != nil {
				return err
			}
			encoded = append(encoded, '\n')
			if out == "" {
				_, err = os.Stdout.Write(encoded)
				return err
			}
			return os.WriteFile(out, encoded, 0o600)
		},
	}
	flags := cmd.Flags()
	flags.IntVar(&opts.Requests, "requests", 100, "Requests to issue per phase")
	flags.IntVar(&opts.Concurrency, "concurrency", 4, "Concurrent workers")
	flags.IntVar(&opts.Warmup, "warmup", 10, "Warmup requests before uncached phases")
	flags.IntVar(&opts.Workloads, "workloads", 8, "Simulated active workloads on the node")
	flags.IntVar(&opts.RequestsPerMinute, "requests-per-minute", 60, "Per-workload request rate for 5m SVID and 1h projected token cache simulation")
	flags.StringVar(&out, "out", "", "File to write the JSON report to; default stdout")
	return cmd
}

// joinObservation is one fleet's journey, with what the mesh's own processes
// reported before and after it, in the same shape as an observation.
type joinObservation struct {
	Labels  map[string]string             `json:"labels,omitempty"`
	Started time.Time                     `json:"started"`
	Join    *bench.JoinReport             `json:"join"`
	Before  map[string]map[string]float64 `json:"metrics_before,omitempty"`
	After   map[string]map[string]float64 `json:"metrics_after,omitempty"`
}

func newJoinCmd() *cobra.Command {
	var (
		opts   bench.JoinOptions
		marker string
		scrape []string
		labels []string
		out    string
	)
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Start a fleet of members and record what joining cost each of them",
		Long: "Starts N agentmesh-node processes from one bootstrap token and times each one from start\n" +
			"to an authenticated router connection to its first call of a service through the\n" +
			"mesh. With --hold the fleet then stays resident and every member's readiness is\n" +
			"sampled, so a router rollout or a key rotation that happens meanwhile shows up as\n" +
			"outage windows. The processes all share this host's source address.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tags, err := parseLabels(labels)
			if err != nil {
				return err
			}
			if opts.Dir == "" {
				if opts.Dir, err = os.MkdirTemp("", "agentmesh-join-"); err != nil {
					return err
				}
			}
			opts.Log = os.Stderr

			obs := joinObservation{Labels: tags, Started: time.Now()}
			// A hold can run for a day; the join numbers are written as soon
			// as they exist and the marker tells a script the fleet is up.
			opts.OnResident = func(r *bench.JoinReport) {
				obs.Join = r
				if out != "" {
					if err := write(out, obs); err != nil {
						fmt.Fprintf(os.Stderr, "agentmesh-bench: %v\n", err)
					}
				}
				if marker != "" {
					stamp := []byte(time.Now().UTC().Format(time.RFC3339) + "\n")
					if err := os.WriteFile(marker, stamp, 0o600); err != nil {
						fmt.Fprintf(os.Stderr, "agentmesh-bench: %v\n", err)
					}
				}
			}
			obs.Before, err = scrapeAll(cmd.Context(), scrape)
			if err != nil {
				return err
			}
			obs.Join, err = bench.RunJoin(cmd.Context(), opts)
			if err != nil {
				return err
			}
			// The fleet is gone by now; a scrape that fails here would lose the
			// report over a metrics endpoint, so it is reported and not fatal.
			if obs.After, err = scrapeAll(context.Background(), scrape); err != nil {
				fmt.Fprintf(os.Stderr, "agentmesh-bench: metrics after the run: %v\n", err)
			}
			return write(out, obs)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.NodeBin, "node-bin", "agentmesh-node", "agentmesh-node binary to start one process of per member")
	flags.IntVar(&opts.Count, "count", 10, "Members to start")
	flags.StringVar(&opts.Dir, "dir", "", "Empty directory for member state, sockets and logs; default a new temporary one")
	flags.StringVar(&opts.ControlPlane, "control-plane", "", "Control plane URL every member enrolls with (required)")
	flags.StringVar(&opts.BootstrapTokenPath, "bootstrap-token-path", "", "File holding the bootstrap token the members enroll with; it needs --count usages left")
	flags.StringVar(&opts.Service, "service", "everything", "MCP service each member finds and calls once ready; empty ends the journey at readiness")
	flags.Float64Var(&opts.Ramp, "ramp", 0, "Members started per second; 0 starts them all at once")
	flags.DurationVar(&opts.ReadyTimeout, "ready-timeout", 180*time.Second, "Bound on one member's enrollment and router authentication")
	flags.DurationVar(&opts.CallTimeout, "call-timeout", 120*time.Second, "Bound on one member's discovery and first call, from readiness")
	flags.DurationVar(&opts.Hold, "hold", 0, "Keep the fleet resident this long after the journeys end, sampling readiness; 0 tears it down at once")
	flags.DurationVar(&opts.SampleInterval, "sample-interval", 10*time.Second, "How often each resident member's readiness is read during --hold")
	flags.StringArrayVar(&opts.NodeArgs, "node-arg", nil, "Extra argument for every agentmesh-node, repeatable (e.g. --node-arg=--insecure-control-plane)")
	flags.StringVar(&marker, "resident-marker", "", "File to create once the fleet is resident and the hold has begun")
	flags.StringArrayVar(&scrape, "scrape", nil, "Metrics endpoint to record before and after the run, repeatable")
	flags.StringArrayVar(&labels, "label", nil, "Condition to record with the observation as name=value, repeatable")
	flags.StringVar(&out, "out", "", "File to write the observation to; default stdout")
	if err := cmd.MarkFlagRequired("control-plane"); err != nil {
		panic(err)
	}
	return cmd
}

// scrapeAll records every endpoint, refusing to continue if one is missing:
// an observation with a hole in it is worse than no observation, because it
// still looks like data.
func scrapeAll(ctx context.Context, urls []string) (map[string]map[string]float64, error) {
	if len(urls) == 0 {
		return nil, nil
	}

	out := make(map[string]map[string]float64, len(urls))
	for _, url := range urls {
		snap, err := bench.Scrape(ctx, url)
		if err != nil {
			return nil, err
		}
		out[url] = snap.Flatten()
	}
	return out, nil
}

func parseHeaders(raw []string) (map[string][]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	header := map[string][]string{}
	for _, h := range raw {
		name, value, found := strings.Cut(h, ":")
		if !found {
			return nil, fmt.Errorf("header %q is not Name: value", h)
		}
		header[strings.TrimSpace(name)] = append(header[strings.TrimSpace(name)], strings.TrimSpace(value))
	}
	return header, nil
}

func parseLabels(raw []string) (map[string]string, error) {
	return api.ParseLabelFlags(raw)
}

func write(path string, obs any) error {
	encoded, err := json.MarshalIndent(obs, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	if path == "" {
		_, err = os.Stdout.Write(encoded)
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}
