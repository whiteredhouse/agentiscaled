package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// usageLimitError is a run that ended because the runtime's sign-in, such
// as the owner's subscription plan, reached a usage limit. The agent may
// then give the task to its fallback runtime (-agent-fallback).
type usageLimitError struct{ error }

func (e *usageLimitError) Unwrap() error { return e.error }

// usageLimitPhrases are what the agent CLIs say when a plan's or account's
// usage limit stops a run.
var usageLimitPhrases = []string{
	"usage limit", "hit your limit", "usage_limit_reached",
	"rate limit", "rate_limit", "quota",
}

// usageLimited says whether a runtime's failure output is a usage limit.
func usageLimited(output string) bool {
	output = strings.ToLower(output)
	for _, p := range usageLimitPhrases {
		if strings.Contains(output, p) {
			return true
		}
	}
	return false
}

// fallbackRuntime runs each task with its first runtime and, only when that
// one stops at a usage limit, again with the next. Both run on this node
// under the owner's own sign-ins, in the same directory and policy; the
// card describes the first.
type fallbackRuntime struct {
	names    []string
	runtimes []runtime
}

func (f *fallbackRuntime) Describe(card *a2a.AgentCard) {
	f.runtimes[0].Describe(card)
	for _, name := range f.names[1:] {
		card.Skills[0].Tags = append(card.Skills[0].Tags, name)
	}
}

func (f *fallbackRuntime) Run(ctx context.Context, dir string, req taskRequest) (string, error) {
	var err error
	for i, rt := range f.runtimes {
		var out string
		out, err = rt.Run(ctx, dir, req)
		var limit *usageLimitError
		if err == nil || !errors.As(err, &limit) || i == len(f.runtimes)-1 {
			return out, err
		}
		log.Printf("%s reached a usage limit (%v); running the task with %s", f.names[i], err, f.names[i+1])
	}
	return "", err
}

// agentRuntimes are the runtimes that run the agent's tasks: its own, then
// its fallback when it has one.
func (c *config) agentRuntimes() []string {
	if c.agent == "" {
		return nil
	}
	if c.agentFallback == "" {
		return []string{c.agent}
	}
	return []string{c.agent, c.agentFallback}
}

// ownerEnvNames are the ownerEnv variables of the named runtimes.
func ownerEnvNames(agents []string) []string {
	var names []string
	for _, agent := range agents {
		names = append(names, runtimes[agent].ownerEnv...)
	}
	return names
}

// checkFallback checks -agent-fallback, as checkAgent does the agent's
// runtime. The fallback runs its runtime's default command, from the PATH
// the node runs with.
func checkFallback(c *config, service bool) error {
	if c.agentFallback == "" {
		return nil
	}
	spec, ok := runtimes[c.agentFallback]
	if !ok {
		return fmt.Errorf("-agent-fallback %q: unknown runtime (have %s)", c.agentFallback, strings.Join(runtimeNames(), ", "))
	}
	if c.agentFallback == c.agent {
		return fmt.Errorf("-agent-fallback %s is the agent's own runtime", c.agentFallback)
	}
	path, err := exec.LookPath(spec.defaultCommand)
	if err != nil {
		return fmt.Errorf("-agent-fallback %s: command %q not found: %w", c.agentFallback, spec.defaultCommand, err)
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if _, err := spec.new(path, c.dataDir, c.agentPolicy); err != nil {
		return fmt.Errorf("-agent-fallback %s: %w", c.agentFallback, err)
	}
	if service && spec.signedIn != nil && !spec.signedIn(path, c.dataDir, c.agentPolicy) {
		_, shown := spec.login(context.Background(), path, c.dataDir, c.agentPolicy)
		return fmt.Errorf("-agent-fallback %s is signed in only through this shell's environment, which the background service does not get: run\n  %s\nor use -foreground and start 'agentiscaled run' from this shell", c.agentFallback, shown)
	}
	return nil
}

// withFallback is rt, or rt and the fallback runtime of c when it has one.
func withFallback(c *config, rt runtime) (runtime, error) {
	if c.agentFallback == "" {
		return rt, nil
	}
	spec, ok := runtimes[c.agentFallback]
	if !ok || c.agentFallback == c.agent {
		return nil, fmt.Errorf("-agent-fallback %q: not a runtime other than -agent (have %s)", c.agentFallback, strings.Join(runtimeNames(), ", "))
	}
	next, err := spec.new("", c.dataDir, c.agentPolicy)
	if err != nil {
		return nil, fmt.Errorf("-agent-fallback %s: %w", c.agentFallback, err)
	}
	return &fallbackRuntime{names: c.agentRuntimes(), runtimes: []runtime{rt, next}}, nil
}
