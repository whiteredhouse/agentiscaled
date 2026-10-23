# agentiscale's fork of Google Agent Mesh

This directory is a copy of [google/agentmesh](https://github.com/google/agentmesh)
at release **v0.1.1** (commit `70fbb22`), kept so that agentiscaled can embed the
node. It is licensed under Apache-2.0; see [LICENSE](LICENSE).

## What differs from upstream

| Change | Why |
|---|---|
| Added `export/node/` | Upstream keeps the node in `internal/node`, which Go forbids other modules to import. `export/node` is inside this module, so it may import it, and re-exports the names agentiscaled uses as type and function aliases. It adds no behaviour. |
| Added `internal/controlplane/bound_peer.go` and its test, plus two calls in `internal/controlplane/server.go` (marked "agentiscale fork") | A bootstrap token whose description starts with `agentiscale-bound-peer:<peer ID>` admits only that peer: `/enroll` refuses any other key before issuing a credential, and `/admin/bootstrap-tokens` refuses an invalid peer ID. Upstream tokens admit any key, so a node could hand its token to another. The binding lives in the description so that the patch needs no schema or API change. |
| Local A2A discovery and dispatch in `internal/node/local_a2a.go`, shared ingress in `node.go`, provider checks in `labels_gate.go`, and transport wiring in `sidecar.go` (with tests and MCP description) | Allows the default node to host an agent used by its own local clients. Self-directed A2A requests retain ingress authorization and streaming without a libp2p self-dial. |
| Added this file | Records the fork. |
| Left out `site/`, `sdk/`, `tests/`, `mobile/`, `charts/`, `deploy/`, `development/`, `hack/`, `scripts/`, Dockerfiles and the Makefile | Not needed to build the Go packages. The Go code includes the patches listed above. |

The agentiscaled module uses it through
`replace github.com/google/agentmesh => ./third_party/agentmesh` in the root `go.mod`.

## Updating to a new upstream release

1. Check out the upstream tag next to this repository.
2. Replace `go.mod`, `go.sum`, `LICENSE`, `api/`, `cmd/`, `internal/` and `agents/`
   here with the tag's copies. Leave `export/` and this file alone, keep
   `internal/controlplane/bound_peer*.go`, and re-apply the two "agentiscale fork"
   calls in `internal/controlplane/server.go` (in `HandleEnroll` after the
   proof-of-possession check, and in `HandleAdminBootstrapTokens`). Preserve the
   local A2A discovery/dispatch changes and their tests in `internal/node/`.
3. Run `go build ./... && go vet ./export/...` here, then `go build ./...` at the
   repository root. If an aliased name was renamed or removed upstream, fix
   `export/node/node.go`.
4. Update the release and commit at the top of this file.

To check that nothing else drifted, diff this directory against the upstream tag:
include the local A2A changes in `internal/node/` alongside `export/`,
`AGENTISCALE.md`, `internal/controlplane/bound_peer*.go` and the two marked blocks
in `internal/controlplane/server.go`.
