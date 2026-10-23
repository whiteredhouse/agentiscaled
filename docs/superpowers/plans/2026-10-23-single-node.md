# Single-node default implementation plan

**Goal:** Configure one daemon for local clients and one hosted agent.
**Architecture:** Keep the current settings schema. Configure the offered agent in the selected node. Add A2A-only local discovery and an in-process streaming transport through the shared ingress authorization handler.
**Spec:** docs/superpowers/specs/2026-10-23-single-node-design.md

## Global Constraints
- Existing nodes are never migrated or removed automatically.
- Alternate data directories remain independent.
- Preserve runtime policies, agent settings and caller/provider authorization.

## Review Focus
- Repeated up must preserve explicit empty/false settings and agent environment.
- Local requests must not trust caller-supplied identity headers.
- Local routes must honor publish permissions, label floors and revocation.
- Streaming and cancellation must avoid buffered responses or leaked goroutines.
- Local discovery must work when the directory is unavailable and avoid duplicates.

### Task 1: Configure the selected node
Files: cmd/agentiscaled/up.go, wizard.go, clients.go and their tests.
- [x] Add failing tests for same-node wizard setup, saved settings, default clients.
- [x] Merge saved flags before validation; prepare/login the offer on the selected data directory; save before a single service start. Remove recursive up and directory allocation.
- [x] Run focused CLI tests.

### Task 2: Local A2A discovery and dispatch
Files: third_party/agentmesh/internal/node/node.go, sidecar.go, labels_gate.go, local_a2a.go and tests.
- [x] Add failing local discovery and authorized invocation tests, including refusals and streaming cancellation.
- [x] Include registered local A2A providers. Extract shared ingress handler. Dispatch self-directed requests through it with a streaming transport; verify current local provider identity without a self dial.
- [x] Run node tests.

### Task 3: Lifecycle and documentation
Files: cmd/agentiscaled integration/recovery tests, docs/getting-started.md, README.md.
- [x] Verify restart/reset settings and local invocation with the real agent service and a fake runtime.
- [x] Update default and explicit multi-node documentation.
- [x] Run both Go module suites and review the diff.

## Progress
- Design approved by user with “do this”. Existing checkout was clean; created codex/single-node-default.

- CLI RED tests reproduced erased agent settings and disabled default clients; mesh RED tests reproduced missing local discovery and self-dial failure.
- Review RED tests reproduced changed unset owner config, revoked standing provider credential acceptance, and nil request-body panic. All now pass.
- Lifecycle integration exercises the real saved settings, agent handler, authenticated router/socket, MCP discovery, agent card, send/get/cancel, delegation rejection, restart and reset with a fixture runtime.
- Final verification: root `go test ./... -count=1` passes; targeted local node tests pass with `-race`. Embedded mesh `go test ./...` initially failed the missing TAR fixture, enrollment cooldown test and a macOS Unix socket path limit.
- Baseline HEAD independently reproduces `TestTARConformanceVectors` (vendored sdk fixture omitted) and `TestRecoverEnrollmentTrustsOnlyKnownKeys/one_attempt_per_cooldown` (3 enrollments, expected 1). No unrelated code was changed. The remaining mesh suite passes using TMPDIR=/tmp and skipping those two failures.
- Independent review verified the fixes and reported no remaining important findings.
- CI runs the new local A2A and provider regressions alongside the bound-peer fork tests. No running node, Homebrew installation or existing separate agent directory was changed.
