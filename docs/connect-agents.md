# Connect an agent client to your node

New to agentiscale? Start with [Get started](getting-started.md): `agentiscaled up`
joins your machine and adds it to Claude Desktop and the Codex CLI itself, and
`agentiscaled clients connect` does the same later, for example for an app installed
after `up`. Use this page to connect a client by hand.

Once agentiscaled has joined your network and is running, a local agent reaches
the network through the node's MCP server. Clients that start MCP servers as a
local command use `agentiscaled mcp`. It relays MCP over standard input and
output to the running node, through the node's Unix socket in its data
directory. Only your user account can open that socket, so the client's
configuration holds no token.

Use the absolute path of your `agentiscaled` binary in the examples below. If
you run the node with `-data-dir`, pass the same `-data-dir` to
`agentiscaled mcp` and give the server its own name, such as
`agentiscale-agent`, as `up` does.

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
agent tools `get_agent_card`, `send_task`, `get_task` and `cancel_task`) then appear in a new chat. Ask it to "show my agentiscale mesh info" to check the connection.

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

## Send a task from Claude Desktop to Codex

With the default setup, Claude Desktop and the hosted Codex agent use the same
node. Offer the agent with `agentiscaled agent add codex -dir /absolute/path/to/workspace`,
then quit and reopen Claude Desktop to load the current MCP bridge. Use
`list_local_services` with type `a2a`, read the agent's card with `get_agent_card`,
and call `send_task` with its name as `service`; omit `peer_id` for local calls.
Use the same target for `get_task` and `cancel_task`.

An explicit self peer ID also selects this local path. Card reads and task calls
check the node's current credential, revocation, publish/invocation policy and
configured label floor through the owner-only Unix socket. An unavailable or
refused agent returns an error. This path shares the ordinary agent handler and
task store and does not change remote Mesh routes.

For existing separate agent directories, follow [the consolidation guidance](getting-started.md#more-than-one-node-on-a-machine).

### Use separate nodes explicitly

Here Claude Desktop sends a task to a Codex CLI agent working in a configured
directory under your runtime permissions. The task crosses your network's
encrypted mesh between two nodes, which can both run on the same Mac. Tasks can
include implementation, code review, explaining code or summarising notes,
according to the permissions you configure locally in Codex.

**Node A** is the one you already run, with Claude Desktop connected as above.
It serves no services; Claude Desktop only sends. Node B is not connected to
your agent apps, so Claude Desktop keeps using node A's `agentiscale` server.

**Node B** serves the agent. Give it its own data directory and the directory
the agent works in:

```sh
agentiscaled agent add codex \
  -data-dir ~/Library/Application\ Support/agentiscale-codex \
  -dir /absolute/path/to/the/directory
```

`agent add` signs in once more for node B, joins it to the same network and runs it as
its own background service; node A offers more agents with plain `agent add`, with
no node of their own. The first argument picks the runtime that does the work:
`codex` (the Codex CLI) or `claude` (Claude Code, see
[Claude Code as the agent](#claude-code-as-the-agent)). Node B checks that `codex` runs (`-command` takes another path), then
serves the A2A 1.0 agent `a2a://codex` to the other nodes of your network.
`-name` picks another name, and `-description` replaces the
description other agents see on its card. Each task runs:

```sh
codex exec --ephemeral -c shell_environment_policy.inherit="core" \
  -C <agent-dir> --output-last-message <file> -
```

with the request on standard input, framed as an untrusted task: the agent
does it and answers with the result. With the default `-owner-config=true`,
Codex uses your configured filesystem and network permissions. A writable policy can
allow it to implement changes; a read-only policy can limit it to inspection.
The remote task cannot select another permission policy or change the receiving
node's working directory. `codex exec` is non-interactive: human approval prompts
are unavailable and escalation is refused. An owner-configured automatic reviewer
can use Codex's supported unattended approval flow. The adapter does not turn that
reviewer on or approve requests on your behalf. Codex runs as yours
does, with your `CODEX_HOME` (`~/.codex`, or the `CODEX_HOME` set when you ran `agent add`,
which `agent add` keeps for node B's service): your sign-in, skills, plugins, settings and MCP
servers, agentiscale included if you connected Codex, so the agent can call your
network's other agents. Node B's background service starts with almost no
environment, so when it starts it reads your login shell's (`$SHELL -l -i`, as your
terminal does, for at most 10 seconds) and adds the variables it lacks, such as a
`GITHUB_TOKEN` that an MCP server's `env_vars` passes on. Variables your shell does not
set go in `<data-dir>/agent.env`, one `NAME=value` per line, readable only by you
(`chmod 600`); they win over both. agentiscaled never writes either. The node's log
says how many variables came from each. With `-owner-config=false` it instead runs with its own
`CODEX_HOME` and `HOME` (`<data-dir>/codex`) and `--ignore-user-config`, and signs in
separately (`CODEX_HOME=<data-dir>/codex codex login`; `agent add` tells you): none of your
own Codex skills, plugins, settings or MCP servers exist for it, so it cannot call
back into the network, and it gets none of the API keys or tokens in node B's
environment (`CODEX_API_KEY` included), only `PATH`, locale, proxy and CA settings
(`CODEX_CA_CERTIFICATE`, `SSL_CERT_FILE`). The directory's `AGENTS.md` files and
skills load as in any Codex run in it. For a directory whose content may come from
someone you don't trust, `-dir-instructions=false` passes `-c project_doc_max_bytes=0` so no
`AGENTS.md` is loaded, and `-dir-skills=false` turns off every skill in or
above the directory (`.agents/skills`, `.codex/skills`), the machine's
`/etc/codex/skills` and Codex's bundled skills. Commands Codex
runs see only basic environment variables (`HOME`, `PATH`, `USER` and the like)
and their filesystem/network access follows your selected runtime policy. With
`-owner-config=false`, the isolated runtime instead enforces the existing
read-only profile with approvals off: it can read only the agent's directory,
minimal system files and the Codex install, with its sign-in directory denied.
It cannot write files. The adapter does not force that profile onto your own
configuration.
To check this on your machine without signing in:
`AGENTISCALE_TEST_CODEX=$(command -v codex) go test ./cmd/agentiscaled -run 'TestCodexAgainstAHostileCheckout|TestCodexOwnerWorkspaceWrite' -v`.

### Claude Code as the agent

`agent add claude` runs each task with `claude -p` in the agent's directory, the
unmodified Claude Code CLI signed in through its own `claude auth login`. The node
never reads or stores that sign-in. With your configuration (the default) it runs as
your own Claude Code does: your sign-in, settings, permissions, skills, plugins and
MCP servers. Nothing can prompt for approval, so anything that would prompt is
denied (`--permission-prompts none`, or the print mode's own denial on a Claude Code
too old for that flag). With `-owner-config=false` it uses its own `CLAUDE_CONFIG_DIR`
(`<data-dir>/claude`, signed in with `CLAUDE_CONFIG_DIR=<data-dir>/claude claude auth login`)
and runs `--restricted` (a Claude Code without it is refused: update it with `claude update`) with only the Read, Grep and Glob tools, confined to the
agent's directory, no MCP servers, and none of your credentials from the environment
(`ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`). `-dir-instructions=false`
stops `CLAUDE.md` files from loading. `-dir-skills=false` turns off every
skill and command of the run, since Claude Code cannot turn off only the directory's.

### Give an agent a subagent's role

A project's subagent definitions, Claude Code's `.claude/agents/<name>.md` or
Codex's `.codex/agents/<name>.toml`, can each become an agent of your network, so
that any client on any of your machines can call it, Codex calling a Claude Code
subagent included:

```sh
agentiscaled agent add codex -definition ~/work/my-project/.claude/agents/security-reviewer.md
```

`-definition` reads the file's name, description and instructions. You pick the
runtime, as with any agent: either runtime runs either format. Without other
flags the agent takes its name and card description from the file and works in
the project the file is in (`~/work/my-project`); `-name`, `-description` and
`-dir` override each. Each task then runs with the file's instructions added to the system prompt:
`--append-system-prompt` for `claude -p`, `-c developer_instructions=…` for
`codex exec`.

`agent add` copies the file into the agent's own state in the node's data
directory (`<data-dir>/agents/<name>/definition.md` or `.toml`), which the
agent's directory may not overlap, so no task can change the agent's role and
no edit, branch switch or delete in the project changes it unseen. The node
runs the agent with that copy. `agent list` shows each agent's source file and
marks it "source changed" or "source gone" when it no longer matches the copy;
run the same `agent add` again to take the new version. `agent remove`, or
`agent add` of the same name without `-definition`, deletes the copy.

The rest of the file applies only when the agent's runtime is the one it was
written for, and never widens the agent's permissions; on the other runtime
`agent add` says which settings it ignores. A Claude Code agent's
`model` and `disallowedTools` apply, and its `tools` narrows the built-in tools
(to those of Read, Grep and Glob it names, with `-owner-config=false`); MCP tools
in that list are not built-in and are left out. A Codex agent's `model` and
`model_reasoning_effort` apply, and `sandbox_mode = "read-only"` makes a run with
your configuration read-only; other sandbox modes are ignored.

Each task starts a fresh session with none of the caller's conversation, so it
suits a task you can describe in one message. When the agent cannot go on
without something only the caller knows, it asks one question: the task waits
in state `input_required` with the question as its message, and the caller
answers with `send_task` giving the task's `task_id`. The agent then carries on
in the same session. The session is kept in the runtime's configuration like
any other Claude Code or Codex session (yours with your configuration, so it
shows in your session history). Offer the few roles you call from other
machines or clients rather than every definition in a project: each one is
another entry in your agents' service list.

Each task belongs to the node that sent it: no other node can read its result,
answer its question or cancel it. Limits: one task at a time (others are
refused, not queued; a task waiting for an answer does not count, and an
answer that comes while another task runs is refused with the question
repeated, so send it again), 15 minutes of running time per task over all its
turns, five questions per task, an hour to answer each, and a request that says
it was delegated is refused. Tasks waiting for answers do not survive a
restart of the node. With your configuration the agent can itself send tasks to your other
agents through your agentiscale MCP server; nothing counts those calls as delegation,
so use `-owner-config=false` where that must not happen. Calling itself while
its task is running is rejected by the same one-task-at-a-time gate. That gate is
not a general delegation boundary: outgoing `send_task` calls use depth zero;
the hosted runtime's delegation provenance is not automatically propagated.
A repeated message ID from the same node is refused instead of run
again, also after node B restarts (`<data-dir>/agents/<name>/tasks-delivered.jsonl`). Incomplete
final log writes are repaired before new tasks start. A malformed completed
record prevents the service from starting because its duplicate history cannot
be trusted. Preserve that file and restore a verified complete copy or reconcile its
history before restarting; deleting it removes duplicate protection.

**In Claude Desktop**, ask for example "Use agentiscale to ask codex to
review the last commit", or "… to explain how requests are authenticated".
The `agentiscale` server also has `get_agent_card`, `send_task`, `get_task` and `cancel_task`: Claude finds the agent with
`discover_remote_services` (type `a2a`), reads its current card with `get_agent_card`
(using the discovered `peer_id` and `srv_name` as `service`), sends the task and follows it to one
of `completed`, `rejected`, `cancelled`, `timeout` or `failed`. A review of one
commit takes about a minute. The agent always works in node B's `-dir`,
never on files on the sender's machine; with both nodes on one Mac, that is
your checkout as it is on disk. If node B is offline the call fails at once;
the mesh keeps no mailbox. A stopped, refused or offline service also makes
`get_agent_card` return a visible error; an earlier card is never reused as a
current availability result.

Claude Desktop starts `agentiscaled mcp` when it opens and keeps that process,
so after rebuilding `agentiscaled`, quit Claude Desktop (Cmd+Q) and open it
again, or it keeps the old tools. Restart the nodes too. On macOS, build to a
new file and move it into place (`go build -o agentiscaled.new ./cmd/agentiscaled
&& mv agentiscaled.new agentiscaled`) rather than writing over the binary the
nodes are running from, which can kill them.

**When a task does not work:**

- Claude has no `send_task`: Claude Desktop is still running the old
  connector; restart it as above.
- Node B will not start and says Codex is not signed in: run the `codex login`
  line it prints (yours, or with `-owner-config=false` the agent's own).
  With your configuration a `CODEX_API_KEY` in your shell counts as signed in, but
  the background service does not get it: `agent add` asks you to store it first with
  `printenv CODEX_API_KEY | codex login --with-api-key` (the wizard does that for you).
- The task ends `failed`: its message carries Codex's error, for example
  "You've hit your usage limit …" when the account the agent signed in with is
  out of usage. Wait for the time it names, or sign the agent in with another
  account.
- The task is `rejected`: its message says why, for example another task is
  still running or task execution is stopped.

**Stop control:** `agentiscaled tasks stop -data-dir <node B's data dir>`
cancels the running task within a second, refuses new tasks and withdraws
the agent from discovery, also across restarts, until `agentiscaled tasks
resume`. `agentiscaled tasks status` says which.
