# Connect an agent client to your node

New to agentiscale? Start with [Get started](getting-started.md): `agentiscaled up`
joins your machine and adds it to Claude Desktop and the Codex CLI itself. Use this
page to connect a client by hand, for example one installed after `up`, or a node set
up with `-clients none`.

Once agentiscaled has joined your network and is running, a local agent reaches
the network through the node's MCP server. Clients that start MCP servers as a
local command use `agentiscaled mcp`. It relays MCP over standard input and
output to the running node, through the node's Unix socket in its data
directory. Only your user account can open that socket, so the client's
configuration holds no token.

Use the absolute path of your `agentiscaled` binary in the examples below. If
you run the node with `-data-dir`, pass the same `-data-dir` to
`agentiscaled mcp` and give the server its own name, such as
`agentiscale-review`, as `up` does.

## Claude Desktop (macOS)

Open Settings, Developer, Edit Config, which opens
`~/Library/Application Support/Claude/claude_desktop_config.json`, and add:

```json
{
  "mcpServers": {
    "agentiscale": {
      "command": "/absolute/path/to/agentiscaled",
      "args": ["mcp"]
    }
  }
}
```

Quit and reopen Claude Desktop. The `agentiscale` server's tools
(`get_mesh_info`, `discover_remote_services`, `find_remote_tools`,
`describe_remote_tool`, `call_remote_tool`, `list_local_services`, and the A2A
task tools `send_task`, `get_task` and `cancel_task`) then appear in a new chat. Ask it to "show my agentiscale mesh info" to check the connection.

## Codex CLI

```sh
codex mcp add agentiscale -- /absolute/path/to/agentiscaled mcp
```

or add to `~/.codex/config.toml`:

```toml
[mcp_servers.agentiscale]
command = "/absolute/path/to/agentiscaled"
args = ["mcp"]
```

`codex mcp list` shows the server; `/mcp` inside a session lists its tools.

## Clients that speak Streamable HTTP

The node also serves MCP at `http://127.0.0.1:8080/mcp` (its `-bind-addr`),
with the header `X-Mesh-Authentication: Bearer <token>`, where the token is in
`<data-dir>/api-token`. Prefer `agentiscaled mcp`, which keeps the token out of
client configuration files.

## If it does not connect

- `agentiscaled mcp` exits with "no running agentiscaled": start the node first.
- The node answers 503 until it is connected to its mesh; check its log for
  "agentiscaled running".

## Send a code review from Claude Desktop to Codex

Here Claude Desktop asks the Codex CLI for a read-only review of a
repository, and the task crosses your network's encrypted mesh between two
nodes. Both nodes can run on the same Mac.

**Node A** is the one you already run, with Claude Desktop connected as above.
It serves no services; Claude Desktop only sends. Since node B serves an agent,
`up` does not add it to your agent clients, so Claude Desktop sees one
`agentiscale` server.

**Node B** serves the review. Give it its own data directory and the repository
it may review:

```sh
# once: the review agent's own Codex sign-in, kept in node B's data directory
CODEX_HOME=~/Library/Application\ Support/agentiscale-review/codex codex login
agentiscaled up \
  -data-dir ~/Library/Application\ Support/agentiscale-review \
  -agent codex \
  -agent-dir /absolute/path/to/the/repository
```

`up` signs in once more for node B, joins it to the same network and runs it as
its own background service. `-agent` picks the runtime that does the work;
`codex` is the only one today, and others can be added behind the same flags.
Node B checks that `codex` runs (`-agent-command` takes another path), then
serves the A2A 1.0 agent `a2a://codex-review` to the other nodes of your
network. Each task runs:

```sh
codex exec --ignore-user-config --ephemeral -c approval_policy="never" \
  -c project_doc_max_bytes=0 -c shell_environment_policy.inherit="core" \
  -c skills.include_instructions=false \
  -c 'skills.config=[{path="<each SKILL.md in or above agent-dir>", enabled=false}, ...]' \
  -c default_permissions="agentiscale-review" \
  -c 'permissions.agentiscale-review={extends=":read-only", filesystem={":root"="deny", ":minimal"="read", ":workspace_roots"="read", "<codex install>"="read"}}' \
  -C <agent-dir> --output-last-message <file> -
```

with the request on standard input, framed as untrusted text. Codex runs with
its own `CODEX_HOME` and `HOME` (`<data-dir>/codex`), which is why it signs in
separately: none of your own Codex skills, plugins or settings exist for it.
It gets none of the API keys or tokens in node B's environment (`CODEX_API_KEY`
included), only `PATH`, locale, proxy and CA settings (`CODEX_CA_CERTIFICATE`,
`SSL_CERT_FILE`). Its configuration, and so its MCP servers, are not loaded, so
the review cannot call back into the network. Since the code under review may
come from someone else, `AGENTS.md` files in the repository are not loaded, and
every skill in or above it (`.agents/skills`, `.codex/skills`), the machine's
`/etc/codex/skills` and Codex's bundled skills are turned off. For a repository you
trust, `-agent-repo-instructions` loads its `AGENTS.md` files and `-agent-repo-skills`
its skills (and Codex's bundled ones), as in any Codex run in it; the rest below stays
the same. Commands Codex
runs see only basic environment variables (`HOME`, `PATH`, `USER` and the like)
and can read only the repository, the system files a shell needs and the Codex
install: not your home directory, and not the agent's `CODEX_HOME` with its
sign-in. They cannot write anything.
To check this on your machine without signing in:
`AGENTISCALE_TEST_CODEX=$(command -v codex) go test ./cmd/agentiscaled -run TestCodexAgainstAHostileCheckout -v`.

Each task belongs to the node that sent it: no other node can read its result or
cancel it. Limits: one review at a time (others are refused, not
queued), 15 minutes per review, and a request that says it was delegated is
refused. A repeated message ID from the same node is refused instead of run
again, also after node B restarts (`<data-dir>/tasks-delivered.jsonl`).

**In Claude Desktop**, ask for example "Use agentiscale to ask codex-review to
review the last commit" (or "the uncommitted changes", or "the commits on this
branch against main"). The `agentiscale` server also has `send_task`,
`get_task` and `cancel_task`: Claude finds the agent with
`discover_remote_services` (type `a2a`), sends the task and follows it to one
of `completed`, `rejected`, `cancelled`, `timeout` or `failed`. A review of one
commit takes about a minute. The review is always of node B's `-agent-dir`,
never of files on the sender's machine; with both nodes on one Mac, that is
your checkout as it is on disk. If node B is offline the call fails at once;
the mesh keeps no mailbox.

Claude Desktop starts `agentiscaled mcp` when it opens and keeps that process,
so after rebuilding `agentiscaled`, quit Claude Desktop (Cmd+Q) and open it
again, or it keeps the old tools. Restart the nodes too. On macOS, build to a
new file and move it into place (`go build -o agentiscaled.new ./cmd/agentiscaled
&& mv agentiscaled.new agentiscaled`) rather than writing over the binary the
nodes are running from, which can kill them.

**When a review does not work:**

- Claude has no `send_task`: Claude Desktop is still running the old
  connector; restart it as above.
- Node B will not start and says the agent's Codex is not signed in: run the
  `CODEX_HOME=… codex login` line above.
- The task ends `failed`: its message carries Codex's error, for example
  "You've hit your usage limit …" when the account the agent signed in with is
  out of usage. Wait for the time it names, or sign the agent in with another
  account.
- The task is `rejected`: its message says why, for example another review is
  still running or task execution is stopped.

**Stop control:** `agentiscaled tasks stop -data-dir <node B's data dir>`
cancels the running review within a second, refuses new tasks and withdraws
the agent from discovery, also across restarts, until `agentiscaled tasks
resume`. `agentiscaled tasks status` says which.
