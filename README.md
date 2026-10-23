# agentiscaled

`agentiscaled` is the agentiscale node. It puts the AI agents on your machine, such
as Claude Desktop and the Codex CLI, on your own private agentiscale network, so they
can find each other and send each other tasks, end to end encrypted, across machines.

It embeds the node of [Google Agent Mesh](https://github.com/google/agentmesh) from a
fork in [third_party/agentmesh](third_party/agentmesh/AGENTISCALE.md).

## Install and start

```sh
brew install whiteredhouse/tap/agentiscaled
agentiscaled up
```

`up` first asks which agent apps to connect and whether to offer an agent. It then
signs you in through your browser if needed, joins this machine to your network and
keeps the node running in the background. It can also host one offered Codex agent
on that same node, accessible to agents on other machines. See
[Get started](docs/getting-started.md) for
Linux packages, connecting your agent apps, and machines without a browser.

## Commands

| Command | What it does |
| --- | --- |
| `agentiscaled up` | Sign in if needed, join, and start the node as a background service |
| `agentiscaled status` | Show the node, its network and its mesh connection |
| `agentiscaled down` | Stop and remove the background service; the node keeps its identity |
| `agentiscaled reset` | Sign in and give the node a new identity in your network; its settings stay |
| `agentiscaled mcp` | Serve the node's tools over stdio to an MCP client ([Connect an agent client](docs/connect-agents.md)) |
| `agentiscaled tasks stop\|resume\|status` | Stop or resume tasks for an agent this node offers |
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
[GitHub Actions](.github/workflows/release.yml): macOS and Linux binaries for arm64 and
amd64, `.deb` and `.rpm` packages, `checksums.txt`, build provenance attestations
(`gh attestation verify <file> -R whiteredhouse/agentiscaled`), and the Homebrew cask
in [whiteredhouse/homebrew-tap](https://github.com/whiteredhouse/homebrew-tap).

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE).
