# Connect an agent client to your node

New to agentiscale? Start with [Get started](getting-started.md): `agentiscaled up`
joins your machine and adds it to Claude Desktop and the Codex CLI itself, and
`agentiscaled client connect` does the same later, for example for an app installed
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

## Call an agent from the command line

Find agents through the running node and read a card before sending work:

```sh
agentiscaled network agent list
agentiscaled network agent card reviewer
```

`network agent list` includes agents available on this node and agents
discovered on other nodes. It identifies the hosting node for each agent.
Use `network agent list -json` for scripts. The `agents` array gives the
service name as `service` and includes a `peer_id` for a remote agent. An agent
whose owner drained it has `draining`, with the owner's `reason` and `since`,
when the drain began: it finishes its tasks but refuses new ones (see [Drain an
agent](getting-started.md#drain-an-agent)).
To see the agents configured on this machine, use `agentiscaled agent list`.

`network agent card` prints the agent's current A2A card as JSON. Both
`card` and `task send` prefer a local agent with the requested name. To choose
a specific remote agent, use the same peer ID for both commands:

```sh
agentiscaled network agent card -peer "$PEER_ID" reviewer
agentiscaled task send -peer "$PEER_ID" reviewer "Review the changes on this branch"
```

`agentiscaled task send` calls an agent of your network without an agent client,
for example from a script or a CI job. It sends the task through the running node,
shows the agent's steps on standard error while it works, and prints its answer on
standard output:

```sh
agentiscaled task send codex "Review the uncommitted changes in ~/src/app"
git diff | agentiscaled task send reviewer -   # the message from standard input
```

When only remote agents match the name and several peers offer it, the command
lists their peer IDs so you can choose one with `-peer`. Pass `-data-dir DIR` to
discovery or task commands when the running node uses a custom data directory.
Other `task send` flags:

- `-timeout` (default 15m): give up after this long. The command then cancels the
  task, so the agent stops too, as it does when you press Ctrl-C.
- `-wait-agent`: keep looking this long for a remote agent that is not on the
  network yet, such as one on a node that is still joining.
- `-json`: print the task's reference, ID, state, last message and result as JSON
  instead of the answer alone.

- `-no-wait`: print the task's reference once the agent has it and return, leaving
  the task to run. Follow it later with `task wait`.

A task's reference, `<peer ID>/<agent>/<task ID>`, says where the task went: the node
that runs it, the agent and the task. `task wait`, `task cancel` and an answer take it,
so they reach the task wherever it went, even when `-peer` or a drain picked a node
other than the one the agent's name would pick now.

If the agent asks a question instead of answering, the command prints it with the
command that answers it, `task send -task <task reference> -- "<answer>"`, which then
follows the task again.

### Wait for a task

`agentiscaled task wait <task reference>` follows a task this node sent, such as one
sent with `task send -no-wait`, until it ends or the agent asks a question. It shows
the agent's steps and prints its answer the way `task send` does, and returns at once
for a task that has already ended. This lets a script start several tasks and collect
their answers later:

```sh
review=$(agentiscaled task send -no-wait reviewer "Review the changes on this branch")
tests=$(agentiscaled task send -no-wait tester "Run the integration tests")
agentiscaled task wait "$review" > review.md
agentiscaled task wait "$tests" > tests.md
```

It takes `-data-dir` and `-json` as `task send` does. `-timeout` (default
15m) bounds the wait only: unlike `task send`, waiting never cancels the task, so when
the time passes or you press Ctrl-C the task goes on and you can wait for it again.
Only the node that sent a task can follow it.

### Cancel a task

`agentiscaled task cancel <task reference>...` cancels tasks this node sent, so that
their agents stop working on them and can take other tasks: an agent runs one task at a
time, and one left running keeps it busy. Cancel the tasks a script stops waiting
for, for example in a cleanup step that runs however the script ended:

```sh
agentiscaled task cancel "$review" "$tests"
```

A task that has already ended is left as it is, and an empty reference is skipped, so
a cleanup step can pass every reference it may have. The tasks are cancelled together, and one
that fails, such as an unknown task, does not stop the others. The command prints one
line for each task (with `-json`, a JSON array whose failed entries have an `error`)
and exits 0 when each task is cancelled or had already ended, else 1. It takes
`-data-dir` and `-timeout` (default 30s).

### Exit status

`task send` and `task wait` say with their exit status how the task went:

| Status | Meaning |
| --- | --- |
| 0 | The task completed; its answer is on standard output. |
| 1 | The command failed, for example the agent cannot be reached or the task is not found. |
| 2 | The task ended without completing: failed, rejected or cancelled. |
| 3 | The agent asks a question (input required); answer it with `task send -task`. |
| 4 | `-timeout` passed first. `task send` has cancelled the task; after `task wait` it still runs. |

## Call a remote MCP tool from the command line

Other nodes of your network can offer MCP services. `agentiscaled network tool` finds,
describes and calls their tools through the running node, the same way the
node's `find_remote_tools`, `describe_remote_tool` and `call_remote_tool` MCP
tools do:

```sh
agentiscaled network tool find                                   # every tool on the network
agentiscaled network tool find -service code-reviewer review_pr  # narrow by service or tool name
agentiscaled network tool describe code-reviewer/review_pr       # its input and output schemas
agentiscaled network tool call code-reviewer/review_pr '{"pr": 42}'
echo '{"pr": 42}' | agentiscaled network tool call code-reviewer/review_pr -
```

A tool's full name is `mcp://SERVICE/TOOL`; `SERVICE/TOOL` is short for it.
`describe` and `call` find the node that offers the tool. When several do, they
list their peer IDs so you can choose one with `-peer`. `call` prints the tool's
text result and fails when the tool reports an error; `-json` prints the whole MCP
result instead. `-labels region=eu` calls a node only if it is attested with every
label given.

The node passes numbers in ARGUMENTS on as 64-bit floating point: integers
above 2^53 (9007199254740992) and decimals with more than about 17 significant
digits arrive rounded. Pass such values as strings if the tool accepts them.

## Teach your agents to call other agents

The agentiscale skill tells an agent when to hand work to another agent of your
network and how: which tools to use, what to put in the request, how to follow
the task and answer its questions, and what to do when it fails. It covers the
`agentiscale` MCP server, the `agentiscale-network` server of a hosted agent and
`agentiscaled task send` in a shell. Install it where Claude Code and the Codex
CLI load your skills:

```sh
agentiscaled skill install          # both; or name one: claude, codex
```

It goes to `~/.claude/skills/agentiscale` (under `$CLAUDE_CONFIG_DIR` if you set
it) for Claude Code and `~/.agents/skills/agentiscale` for Codex. `-dir <project>`
installs it into a project instead (`.claude/skills` and `.agents/skills` there),
for everyone who works in it. Run the command again after an update of
agentiscaled to take the skill's new version.

The skill is a SKILL.md with what every agent needs, and one reference for each
way in (`references/cli.md` for the shell, `references/mcp.md` for the MCP
servers) that an agent reads only when it uses that way. An agent with a shell
and a running node uses `agentiscaled task send`, which adds no tool
definitions to its context, before the `agentiscale` MCP server.

`agentiscaled skill` prints SKILL.md, and `agentiscaled skill print
references/cli.md` one of its references, for an agent to read in one command or
to save wherever your agent loads skills from. The skill is also in this repository
under `skills/`, for skill installers that read a repository:

```sh
npx skills add whiteredhouse/agentiscaled
```

A hosted agent that runs with your configuration (the default) loads the skill
from your user skills like your own runs do.

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

**Node B** serves the agent. Join it with its own data directory, then offer the
agent from it with the directory the agent works in:

```sh
agentiscaled up -data-dir ~/Library/Application\ Support/agentiscale-codex
agentiscaled agent add codex \
  -data-dir ~/Library/Application\ Support/agentiscale-codex \
  -dir /absolute/path/to/the/directory
```

`up` signs in once more for node B, joins it to the same network and runs it as
its own background service; `agent add` then adds the agent to it. `agent add` only
offers agents from a node that has joined: on a data directory that has not, it
stops and names the `up` command to run. Node A offers more agents with plain
`agent add`, with no node of their own. The first argument picks the runtime that does the work:
`codex` (the Codex CLI) or `claude` (Claude Code, see
[Claude Code as the agent](#claude-code-as-the-agent)). Node B checks that `codex` runs (`-command` takes another path), then
serves the A2A 1.0 agent `a2a://codex` to the other nodes of your network.
`-name` picks another name, and `-description` replaces the
description other agents see on its card, which also lists the skills it loads
from its directory (see [Skills on the agent's card](#skills-on-the-agents-card)).
Each task runs:

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
says how many variables came from each.

With `-owner-config=false`, Codex uses the service's [private profile](#private-runtime-profiles)
as `CODEX_HOME` and the `home` subdirectory as `HOME`. It runs with
`--ignore-user-config`, so the owner's Codex skills, plugins, settings and MCP servers
are excluded. The node supplies the MCP server for [calls to other agents](#agents-call-other-agents).
Codex inherits basic process, locale, proxy and CA settings such as `PATH`,
`CODEX_CA_CERTIFICATE` and `SSL_CERT_FILE`. The node excludes credential
variables such as `CODEX_API_KEY` and `GITHUB_TOKEN`. The directory's
`AGENTS.md` files and skills load as in any Codex run in it. For a directory
whose content may come from someone you don't trust, `-dir-instructions=false`
passes `-c project_doc_max_bytes=0` so no
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
`AGENTISCALE_TEST_CODEX=$(command -v codex) go test ./internal/node -run 'TestCodexAgainstAHostileCheckout|TestCodexOwnerWorkspaceWrite' -v`.

### Claude Code as the agent

`agent add claude` runs each task with `claude -p` in the agent's directory, the
unmodified Claude Code CLI signed in through its own `claude auth login`. The node
never reads or stores that sign-in. With your configuration (the default) it runs as
your own Claude Code does: your sign-in, settings, permissions, skills, plugins and
MCP servers. Nothing can prompt for approval, so anything that would prompt is
denied (`--permission-prompts none`, or the print mode's own denial on a Claude Code
too old for that flag). With `-owner-config=false` it uses the service's
[private profile](#private-runtime-profiles) as `CLAUDE_CONFIG_DIR`
and runs `--restricted` (a Claude Code without it is refused: update it with `claude update`) with only the Read, Grep and Glob tools, confined to the
agent's directory, and only the node's MCP server for
[calls to other agents](#agents-call-other-agents). The node excludes Claude
sign-in variables such as `ANTHROPIC_API_KEY` and `CLAUDE_CODE_OAUTH_TOKEN`.
`-dir-instructions=false` stops `CLAUDE.md` files from loading. `-dir-skills=false`
turns off every skill and command of the run, since Claude Code cannot turn off only the directory's.

### Other agents (ACP)

`agent add acp` offers any agent that speaks the
[Agent Client Protocol](https://agentclientprotocol.com) on stdio. Give its command
line, split at spaces:

```sh
agentiscaled agent add acp -command "my-agent acp" -dir ~/work/my-project
```

The agent takes its name from the program (`my-agent`) unless `-name` says
otherwise. Each task runs the command in the agent's directory, and its process
stays open while the task waits for an answer to the agent's question. The node
answers every permission request the agent makes with a rejection, and shows only
fixed summaries of its steps. The agent declares which of `-owner-config=false`,
`-dir-instructions=false` and `-dir-skills=false` it enforces, and `agent add`
refuses one it does not. With `-owner-config=false`, each named service has its
own sign-in, even when several agents use the same launcher. If a caller reports
that the agent needs to sign in, check the node log for the local login command.
Vendors can check their agents with `agentiscaled runtime check -command "my-agent acp"`.
For the capabilities the check exercises and how to enable calls to other agents,
see [Add agentiscale support to your agent](add-agentiscale-support.md).

### Skills on the agent's card

An agent's card lists the skills it loads from its directory, by name and
description, so a caller can see what the agent knows how to do before it sends
work. Each skill is a `SKILL.md` with `name` and `description` in its front matter:

- Codex: the skills in `.agents/skills` and `.codex/skills` of the agent's
  directory and of each directory above it up to the repository's root (only the
  agent's directory outside a repository), named by the file's `name`.
- Claude Code: the skills in `.claude/skills` of the agent's directory and of each
  directory above it up to the repository's root (outside a repository, short of
  your home directory), named by their folder, as Claude Code names them. An agent
  with `-owner-config=false`, or whose definition leaves out the `Skill` tool, uses
  no skills and lists none.

Your own skills, plugins' skills, the machine's and the runtime's bundled skills
are not the directory's and are never listed. Neither are skills only a person can
start (`disable-model-invocation: true`), or a skill without a description. The card
lists at most 64, sorted by name, and is read again for each caller, so a skill you
add shows without restarting the node. With `-dir-skills=false`, or with no skills
in its directory, an agent lists one general `task` skill, as an ACP agent always
does. Every agent and client in your network can read the card, and so these
names and descriptions: keep a skill you don't want shown out of an offered
agent's directory, or offer the agent with `-dir-skills=false`.

### Private runtime profiles

With `-owner-config=false`, each named service gets its own profile at
`<data-dir>/agents/<service-key>/profile`. For example, an agent added with
`-name reviewer` uses `<data-dir>/agents/reviewer/profile`. The service key is
the name used by `agent add`. The node encodes names from saved settings or
foreground flags when they contain other characters, such as a dot. Two
services using the same runtime can sign in to different accounts. If you
switch runtimes under the same service name, the new runtime uses the same
profile and can read the files already there.

All three runtimes inherit the node's proxy settings, including any credentials
in a proxy URL.

If you're upgrading from `<data-dir>/codex`, `<data-dir>/claude` or an older ACP
profile under `<data-dir>/acp` or `<data-dir>/acp-agents`, sign in once for
each service that needs credentials. The new profiles start empty. Interactive
`agent add` shows and runs `codex login` or `claude auth login` with the
service's profile path. For ACP, check the node log for the agent's sign-in
instructions.

On upgrade, a saved or foreground service with an encoded name such as
`review.dev` starts a fresh delivery history in its new state directory,
regardless of `-owner-config`. A task with a message ID delivered before the
upgrade can run again. If this service has a configured definition, restore
its trusted `definition.md` or `definition.toml` in
`<data-dir>/agents/<service-key>/` before starting it. Services named through
`agent add` keep their existing definition and delivery-history paths.

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
directory (`<data-dir>/agents/<service-key>/definition.md` or `.toml`), which the
agent's directory may not overlap, so no task can change the agent's role and
no edit, branch switch or delete in the project changes it unseen. The node
runs the agent with that copy, and `agent list` shows its path. To take a
changed file, run the same `agent add` again. `agent remove`, or `agent add`
of the same name without `-definition`, deletes the copy.

The rest of the file applies only when the agent's runtime is the one it was
written for, and never widens the agent's permissions; on the other runtime
`agent add` says which settings it ignores. A Claude Code agent's
`model` and `disallowedTools` apply, and its `tools` narrows the built-in tools
(to those of Read, Grep and Glob it names, with `-owner-config=false`); MCP tools
in that list are not built-in and are left out. A Codex agent's `model` and
`model_reasoning_effort` apply, and `sandbox_mode = "read-only"` makes a run with
your configuration read-only; other sandbox modes are ignored.

Each task starts a fresh session with none of the caller's conversation, so it
suits a task you can describe in one message. While it works, the task's
message is the agent's latest step (at most one every 3 seconds), so the caller
sees what it is doing when it follows the task with `get_task`; a client that
shows MCP progress notifications also gets each step while `get_task` waits.
`get_task` receives the steps as the agent sends them: the agent's card
declares A2A streaming, so any A2A client can follow a task with
`SubscribeToTask` or send one with `SendStreamingMessage`. The agent ends each
stream after 2 minutes, while the task goes on; subscribe again to keep
following it. Nothing is pushed to a URL of the caller's (no push
notifications).
A step is a fixed summary: the program a command runs, a file's base name, a
tool's name or the todo item's position, such as `Running go`,
`Editing main.go` or `Working on step 2 of 5`. It never carries a command's
arguments, a path, a URL, a search or what the agent says. When the agent cannot go on
without something only the caller knows, it asks one question: the task waits
in state `input_required` with the question as its message, and the caller
answers with `send_task` giving the task's `task_id`. The agent then carries on
in the same session. On the agent's machine, `agentiscaled agent list` shows
the agent as `waiting for an answer` until the answer comes (or `busy` while a
task runs). The session is kept in the runtime's configuration like
any other Claude Code or Codex session (yours with your configuration, so it
shows in your session history). Offer the few roles you call from other
machines or clients rather than every definition in a project: each one is
another entry in your agents' service list.

Each task belongs to the node that sent it, or to the task of a hosted agent
that sent it (see below): no other node or task can read its result,
follow its steps, answer its question or cancel it. Limits: one task at a time (others are
refused, not queued; a task waiting for an answer does not count, and an
answer that comes while another task runs is refused with the question
repeated, so send it again), 15 minutes of running time per task over all its
turns, five questions per task, an hour to answer each, and a request whose token
says it was delegated more than once is refused. Tasks waiting for answers do not survive a
restart of the node. With your configuration the agent can also send tasks through your
own agentiscale MCP server, if your configuration has it; those calls carry your
rights and count as a person's (depth zero), so use `-owner-config=false` where that
must not happen.
A repeated message ID from the same node is refused instead of run
again, also after node B restarts (`<data-dir>/agents/<service-key>/tasks-delivered.jsonl`). Incomplete
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
  still running, or the agent's owner drained it (`reviewer is draining: <reason>`).

To stop node B's agents taking tasks, run `agentiscaled down` on node B: it
stops the node's background service, cancels running work and takes the node's
agents off the network until `agentiscaled up`. A node in the foreground
(`agentiscaled run`, or a container) stops with its process instead. `agentiscaled agent remove NAME` takes one agent off.
`agentiscaled agent drain NAME -reason "..."` lets the agent finish its tasks and
refuse new ones with that reason, until `agent undrain NAME`.

### Agents call other agents

While it works on a task, an agent can give parts of it to your network's other
agents. Each turn of a task gets its own Agent Mesh token, which the node mints
from its own credential (`/oauth/token` on its local API) and seals: it allows only
A2A calls (`a2a://*`), names the node, so it works only through it, and expires
when the task's running time is up. The agent's run reaches it as one more MCP
server, `agentiscale-network`, on a loopback port of the node, with a bearer
credential of its own: a secret of that server's, the same over the task's turns,
which is no credential anywhere else and whose tools run only while a turn runs. The server holds the turn's token and makes the calls with it: `--mcp-config` in a file only you can read, outside the
agent's directory, with its tools allowed, for Claude Code; `-c mcp_servers.agentiscale-network.*`
with the bearer in `AGENTISCALE_NETWORK_TOKEN`, which the commands Codex runs do not
get, and its tools approved, for Codex. Neither run asks before it uses them: the
turn's token is what bounds them. It has `list_agents`, `get_agent_card`, `send_task`, `get_task` and
`cancel_task`. Every call goes to the node with the token, and the node checks it
(signature, expiry, revocation, the A2A-only rule) and sends it on in place of its
own credential. When the turn ends, the node revokes the token and the MCP server
runs no tool call until the task's next turn, if any, and refuses the bearer once
the task ends.

With `-owner-config=false` this server is the run's only way into the network: the
run has no MCP server of yours and cannot reach the node's socket or credential in
the node's data directory, so it cannot use your network's MCP services or mint
tokens of its own.
With your configuration the run also has whatever your configuration gives it, your
own agentiscale MCP server included if you added it.

The token also names the task it is for and how far the tasks it sends are
delegated (`agentiscale-task/1/<task ID>` as the name of its task rule). The run
cannot change that, since the token is sealed, and the node that delivers a call
passes the name from the verified token to the agent it calls, which reads the
delegation and the sender from it, not from the message. So delegation stops
after one hop: the agent that gets a task from another agent works on it without
the network, so it cannot pass it on, also after it asks a question and gets an
answer. An agent cannot send a task to itself.

A task an agent sends belongs to the agent's task, over all its turns, not to the
node: only that task can follow, answer or cancel it, not the node's owner or the
node's other tasks, and the agent's task cannot reach the owner's tasks either.
The node also keeps that record itself, and refuses more than 64 tasks sent by one
task. A node's owner can narrow its own credential and send without the name;
the delegation bound holds hosted agents, not the owners of the nodes they call
from.

A node running v0.9.0 or older takes a task from another agent as one from its
node: its agents have no network, so delegation still stops there, but the task
belongs to the sending node rather than to the sending task. Update the nodes
whose agents take tasks from other agents.
