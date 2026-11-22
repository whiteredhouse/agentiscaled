# Add agentiscale support to your agent

If you build a coding agent, you can make it an agent of any agentiscale network
from your own code. You don't need a pull request to agentiscaled, and you don't
need to wait for an agentiscaled release.

Once your agent supports agentiscale, its users can:

- offer it to their network with `agentiscaled agent add acp -command "<your command>"`,
  so that any client or agent on any of their machines can send it tasks over A2A
- follow each task as it works, from the steps your agent reports
- answer the questions your agent asks, on the same task
- let it give parts of a task to their other agents
- give it a subagent's role (`-definition`)

agentiscaled keeps its own integrations for Codex and Claude Code. Every other
agent comes in through this contract, which you own and can fix on your own
release schedule.

## How it works

The contract is the [Agent Client Protocol](https://agentclientprotocol.com) (ACP)
over stdio, plus a few agentiscale conventions in ACP's `_meta` fields and in the
environment. If your agent already speaks ACP for an editor, most of the work is
done.

The owner gives the node your agent's command line, such as `my-agent acp`. The
node splits it at spaces, so neither the program's name nor an argument can hold
one, finds the program on `PATH`, and from then on keeps its path and the arguments
apart. For each task, the node:

1. starts the command in the agent's directory, with stdin and stdout as the ACP
   connection and the environment below
2. sends `initialize` and reads what your agent declares
3. sends `session/new`, with the network's MCP server in `mcpServers`
4. sends `session/prompt` with the task, and reads your `session/update`
   notifications as the turn's steps and result
5. takes your `session/prompt` response as the turn's end
6. when the turn ends in a question, keeps your process open and later sends the
   caller's answer as another `session/prompt` in the same session
7. closes stdin when the task ends

The node also starts your command once, for `initialize` only, when the owner adds
the agent and when the node starts, to check what your agent declares.

The node is the ACP client. It offers no `fs` or `terminal` capability: your agent
works on the directory with its own tools, under its own configuration and the
policy below.

Every process the node starts for your agent gets:

| Variable | Meaning |
| --- | --- |
| `AGENTISCALE_ENV` | `1` when the node runs your agent |
| `AGENTISCALE_CONTRACT` | The contract version, `1` |
| `AGENTISCALE_CONFIG_DIR` | Your agent's own configuration directory in the node's data directory, set only when the owner turned off their own configuration (`-owner-config=false`) |
| `AGENTISCALE_DIR_INSTRUCTIONS` | `0` when the owner turned off the directory's instruction files |
| `AGENTISCALE_DIR_SKILLS` | `0` when the owner turned off the directory's skills |

With `-owner-config=false` the process gets only what a process needs from the
node's environment (`PATH`, `HOME`, locale, proxy and certificate settings) and
none of the owner's credentials.

## Declare what you support

Answer `initialize` with `protocolVersion` 1, and declare agentiscale's
conventions in `_meta.agentiscale`:

```json
{
  "protocolVersion": 1,
  "agentInfo": { "name": "my-agent", "version": "X.Y.Z" },
  "agentCapabilities": {
    "loadSession": true,
    "mcpCapabilities": { "http": true }
  },
  "_meta": {
    "agentiscale": {
      "contract": 1,
      "status": true,
      "policies": ["config-dir", "dir-instructions", "dir-skills"],
      "login": ["my-agent", "login"]
    }
  }
}
```

- `contract` is the version of this page your agent keeps.
- `status: true` says your agent ends each turn with
  `_meta.agentiscale.status` (see [End the turn](#end-the-turn)). The node reads
  it at `initialize`, before it writes the prompt.
- `policies` lists the owner's policies your agent enforces when the environment
  asks it to. The node refuses to run an agent with a policy it did not declare:
  an owner who turns off their own configuration gets an error at `agent add`, not
  an agent that quietly uses it.
  - `config-dir`: with `AGENTISCALE_CONFIG_DIR` set, read and write your
    configuration, sign-in, sessions and settings only there, take no credential
    from the environment, and give the run only read-only tools, confined to the
    agent's directory.
  - `dir-instructions`: with `AGENTISCALE_DIR_INSTRUCTIONS=0`, load no instruction
    files from the directory, such as `AGENTS.md`.
  - `dir-skills`: with `AGENTISCALE_DIR_SKILLS=0`, load no skills from the
    directory.
- `login` is the command that signs your agent in. The node shows it to the owner
  when your agent is not signed in.
- `loadSession` lets your agent take an answer after the process that asked has
  ended (see [End the turn](#end-the-turn)).
- `mcpCapabilities.http` lets your agent call the network's other agents. Without it
  your agent works without them.

An ACP agent that declares no `_meta.agentiscale` still runs with the owner's own
configuration and every policy on, but no other way.

## Sign-in

The node never reads or stores your agent's credentials. If your agent is not
signed in, answer `session/new` with ACP's `auth_required` error (`-32000`). The
task fails with your `login` command, with `AGENTISCALE_CONFIG_DIR` set when the
agent has a configuration of its own, so the owner can run it.

## Steps

Send the turn's steps as `session/update` notifications, as you would to an editor.
A step is shown to the task's caller, who may not be the owner, so the node never
shows your tool call's `title`, its input or a plan entry's text, which can hold a
command's arguments, a path, a search or a credential. It writes a fixed summary
from structured fields instead:

| Update | Step |
| --- | --- |
| `tool_call` of `kind` `read`, `edit`, `delete` or `move` | `Reading`, `Editing`, `Deleting` or `Moving`, and the base name of its first `locations` path, such as `Editing main.go` |
| `tool_call` of `kind` `search`, `execute`, `fetch` or `think` | `Searching`, `Running a command`, `Fetching a page` or `Thinking` |
| any other `tool_call` | `Using a tool` |
| `plan` | the position of the first entry that is not `completed`, such as `Working on step 2 of 5` |

Set `kind` and `locations` on your tool calls to get useful steps.

## End the turn

Your `session/prompt` response ends the turn. Its text is the
`agent_message_chunk` text you sent after the node sent the prompt: the history
that `session/load` replays before it answers is not part of the turn. Say whether
the text is the result or a question in `_meta`:

```json
{
  "stopReason": "end_turn",
  "_meta": { "agentiscale": { "status": "done" } }
}
```

- `done`: the text is the task's result, and the task completes.
- `needs_input`: the text is a question to the caller, and the task waits for the
  answer. The node keeps your process open and sends the answer as the next
  `session/prompt` in the same session. Each hosted agent keeps up to 16 processes
  waiting for answers. The caller has one hour to answer a question. Once the
  node accepts the answer, it keeps the process for that turn, even if the hour
  passes while the turn is starting. When a question expires, the node closes
  stdin. When the process that asked is gone, because it exited or
  more questions wait than the node keeps, an agent with `loadSession` gets the
  answer in a new process: the node starts your command again and sends
  `initialize`, `session/load` with the same `sessionId`, and then the answer as a
  prompt. Keep the session where your agent can load it from the directory and
  configuration it ran with. Without `loadSession` such an answer, or such a
  question, fails the task.
- Any other `stopReason`, or an ACP error, fails the task. The task's caller, who
  may not be the owner, gets only a fixed reason, such as the method and error
  code. Your error message and the last lines of your stderr go to the node's log,
  which only the owner reads.
- The turn's text may be up to 1 MiB. A longer one fails the task.

An agent that does not declare `status: true` at `initialize` gets every prompt
with the node's instructions to answer with only a JSON object,
`{"status":"done"|"needs_input","text":"…"}`, as Codex and Claude Code do. A
`status` in the response wins over the text. Text that is neither is the result as
it stands. Prefer `status`: it cannot be confused with the agent's own output.

## Cancellation

When the caller cancels the task, or it runs past its time, the node sends
`session/cancel`, never before the prompt it cancels. End the turn with
`stopReason` `cancelled`. A permission request that the node answers after the
cancel gets the `cancelled` outcome. After 5 seconds, even
if your agent has stopped reading stdin, and whenever a turn ends and your process
has not exited within 5 seconds of stdin closing, the node stops your process and
every process it started.

## Permissions

Nothing can prompt a person while a task runs. The node answers every
`session/request_permission` with your `reject_once` option, or cancels the request
when you offer none. It never picks `reject_always`, which your agent could keep as
the owner's choice. Configure your agent so that what it should do needs no approval and what it
should not do is denied.

## The network's other agents

When the owner allows it, `session/new` and `session/load` name one more MCP
server, `agentiscale-network`, an HTTP server on a loopback port of the node, with a
bearer token in its `headers`. Its tools are `list_agents`, `get_agent_card`,
`send_task`, `get_task` and `cancel_task`. Allow the tools of this server without
asking: the node configured it, and its token is what bounds them. The token stays
the same over a task's turns, so a process kept for an answer keeps its connection,
but the server runs its tools only while a turn runs. Don't write it to disk, logs or the
directory, and don't pass it to the commands your agent runs.

## Subagent roles

When the owner gives your agent a role with `-definition`, the node adds the role's
instructions in front of the first prompt of each task, in a `<role>` block. A
role's model and tool settings don't apply to your agent.

## Keep it safe

- The task text comes from another agent: it is untrusted. The node says so in the
  prompt. Keep your own permission rules in charge, not the task.
- Write nothing but ACP messages to stdout. Write logs to stderr.
- Exit when stdin closes.

## Check your integration

```sh
agentiscaled runtime check -command "my-agent acp"
```

`runtime check` starts your command the way the node does and checks, in an empty
directory: `initialize` and what you declare, a turn that ends in a result, and a
cancelled turn, a turn that ends in a question and the answer in the process that
asked. With `loadSession` it also checks an answer in a new process, after
`session/load`; without it, it skips that. Each turn has 5
minutes (`-timeout`): a turn that does not end in time fails its step. Add
`-owner-config=false`, `-dir-instructions=false` or `-dir-skills=false` to check
your agent under those policies. The turns run your agent's model, with the
configuration and sign-in the check gives it.

Then offer your agent on a test node and send it a task:

```sh
agentiscaled agent add acp -name my-agent -command "my-agent acp" -dir ~/work/sandbox
agentiscaled task send my-agent "List the files in this directory"
```

## Versions

This page is version 1 of the contract (`AGENTISCALE_CONTRACT=1`), on ACP protocol
version 1. A node refuses an agent that declares a newer contract than it knows.
Fields added to `_meta.agentiscale` within a version are optional: ignore the ones
you don't know.
