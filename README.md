# agentiscaled

`agentiscaled` is the agentiscale node. It puts the AI agents on your machine, such
as Claude Desktop and the Codex CLI, on your own private agentiscale network, so they
can find each other and send each other tasks, end to end encrypted, across machines.

It embeds the node of [Google Agent Mesh](https://github.com/google/agentmesh) from a
fork, [whiteredhouse/agentmesh](https://github.com/whiteredhouse/agentmesh/blob/agentiscale/AGENTISCALE.md).

## Install and start

```sh
brew install whiteredhouse/tap/agentiscaled
agentiscaled up
```

Without Homebrew, on Linux or macOS (x64 or ARM64):

```sh
curl -fsSL https://raw.githubusercontent.com/whiteredhouse/agentiscaled/main/install.sh | sh
```

The script installs the latest release after checking it against the release's
`checksums.txt`. `sh -s -- -version v0.11.0 -dir ~/bin` pins a release and picks
where the binary goes (default `/usr/local/bin` if you can write to it, else
`~/.local/bin`).

`up` first asks which agent apps to connect and whether to offer an agent. It then
signs you in through your browser if needed, joins this machine to your network and
keeps the node running in the background. Later, `agentiscaled client connect`
connects an app installed after `up`, and `agentiscaled agent add` offers another
agent, accessible to local clients and other machines. See
[Get started](docs/getting-started.md) for
Linux packages, connecting your agent apps, and machines without a browser.

## In GitHub Actions

A job joins your network with
[agentiscale-action](https://github.com/whiteredhouse/agentiscale-action), using
GitHub's OIDC token instead of a stored secret, once your network trusts the workflow:

```yaml
permissions:
  id-token: write
steps:
  - uses: whiteredhouse/agentiscale-action@v1
    with:
      network-id: YOUR_NETWORK_ID
```

See [Nodes that join with a workload
token](docs/getting-started.md#nodes-that-join-with-a-workload-token).

## Commands

| Command | What it does |
| --- | --- |
| `agentiscaled up` | Sign in if needed, join, and start the node as a background service; in a terminal it first asks which agent apps to connect and whether to offer an agent |
| `agentiscaled client list\|connect\|disconnect` | Connect your agent apps (Claude Desktop, Codex CLI) to the node, or disconnect them |
| `agentiscaled agent add\|remove\|list` | Offer an agent (Codex, Claude Code) from this machine to your network, or stop offering it |
| `agentiscaled runtime list` | List supported agent runtimes, their executables on `PATH`, and their config directories for the current shell |
| `agentiscaled status` | Show the node, its network and its mesh connection |
| `agentiscaled down` | Stop and remove the background service; the node keeps its identity |
| `agentiscaled reset` | Drop the node's identity so `up` joins it again as a new node; its settings stay |
| `agentiscaled mcp` | Serve the node's tools over stdio to an MCP client ([Connect an agent client](docs/connect-agents.md)) |
| `agentiscaled task stop\|resume\|status` | Show, stop, or resume task execution on this node |
| `agentiscaled run` | Run the node in the foreground with the options saved by `up` |
| `agentiscaled version` | Print the version |

Run `agentiscaled <command> -h` for its flags.

## Build from source

You need Go (see `go.mod` for the version).

```sh
go build -o agentiscaled ./cmd/agentiscaled
go test ./...
```

`go install …@latest` does not work: the module uses a `replace` directive for the
fork.

## Releases

A `v*` tag builds the release with [GoReleaser](.goreleaser.yaml) in
[GitHub Actions](.github/workflows/release.yml). GoReleaser stages macOS and
Linux binaries for arm64 and amd64, `.deb` and `.rpm` packages, and
`checksums.txt` in a draft. The workflow verifies build provenance and checks
that GitHub reports the signed digest for every staged asset before publishing.
Use `gh attestation verify <file> -R whiteredhouse/agentiscaled` to verify a
download.

After GitHub publication, the `homebrew` job updates
[whiteredhouse/homebrew-tap](https://github.com/whiteredhouse/homebrew-tap) for
stable releases when `HOMEBREW_TAP_TOKEN` is configured. If attestation fails,
rerun the failed `release` job to restage the draft. If publication or the tap
update fails, rerun the failed jobs in the original workflow run to reuse the
staged inputs. Resolve a failed tap update before tagging the next stable
version so each release has a corresponding tap revision.

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE).
