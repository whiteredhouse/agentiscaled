# agentiscale's fork of Google Agent Mesh

This directory is a copy of [google/agentmesh](https://github.com/google/agentmesh)
at release **v0.1.1** (commit `70fbb22`), kept so that agentiscaled can embed the
node. It is licensed under Apache-2.0; see [LICENSE](LICENSE).

## What differs from upstream

| Change | Why |
|---|---|
| Added `export/node/` | Upstream keeps the node in `internal/node`, which Go forbids other modules to import. `export/node` is inside this module, so it may import it, and re-exports the names agentiscaled uses as type and function aliases. It adds no behaviour. The aliases also expose existing authorization inputs and helpers so agentiscaled can enforce them on its own owner-only local agent API. |
| Added `internal/controlplane/bound_peer.go` and its test, plus two calls in `internal/controlplane/server.go` (marked "agentiscale fork") | A bootstrap token whose description starts with `agentiscale-bound-peer:<peer ID>` admits only that peer: `/enroll` refuses any other key before issuing a credential, and `/admin/bootstrap-tokens` refuses an invalid peer ID. Upstream tokens admit any key, so a node could hand its token to another. The binding lives in the description so that the patch needs no schema or API change. |
| Added this file | Records the fork. |
| Left out `site/`, `sdk/`, `tests/`, `mobile/`, `charts/`, `deploy/`, `development/`, `hack/`, `scripts/`, Dockerfiles and the Makefile | Not needed to build the Go packages. Everything under `api/`, `cmd/`, `internal/` and `agents/` is byte-identical to upstream except the bound-peer patch above. |

The agentiscaled module uses it through
`replace github.com/google/agentmesh => ./third_party/agentmesh` in the root `go.mod`.

## Updating to a new upstream release

1. Check out the upstream tag next to this repository.
2. Replace `go.mod`, `go.sum`, `LICENSE`, `api/`, `cmd/`, `internal/` and `agents/`
   here with the tag's copies. Leave `export/` and this file alone, keep
   `internal/controlplane/bound_peer*.go`, and re-apply the two "agentiscale fork"
   calls in `internal/controlplane/server.go` (in `HandleEnroll` after the
   proof-of-possession check, and in `HandleAdminBootstrapTokens`).
3. Run `go build ./... && go vet ./export/...` here, then `go build ./...` at the
   repository root. If an aliased name was renamed or removed upstream, fix
   `export/node/node.go`.
4. Update the release and commit at the top of this file.

To check that nothing else drifted, diff this directory against the upstream tag:
only `export/`, `AGENTISCALE.md`, `internal/controlplane/bound_peer*.go` and the
two marked blocks in `internal/controlplane/server.go` should differ.
