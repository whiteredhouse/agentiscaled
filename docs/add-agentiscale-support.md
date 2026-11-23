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
4. sends `session/prompt` with the task and reads your `session/update`
   notifications for steps and streamed messages
5. takes your `session/prompt` response as the turn's end
6. when the turn ends in a question, keeps your process open and later sends the
   caller's answer as another `session/prompt` in the same session
7. closes stdin when the task ends

When the owner adds the agent and when the node starts, the node checks what
your agent declares by running `initialize` in the configured working directory.
Relative arguments such as `./agent.py` resolve there on both the check and the
task run.

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
      "resultText": true,
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
- The node sends `_meta.agentiscale.resultText: true` in its `initialize`
  request when it can read the final text from a prompt response. Return
  `resultText: true` to use this option on that connection. With older nodes,
  stream the final message so they can read it.
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

If your agent needs the owner to sign in, answer with ACP's `auth_required`
error (`-32000`). The task caller sees that sign-in is required. The owner can
find the declared `login` command in the node log or in local `runtime check`
diagnostics. For `-owner-config=false`, that command includes the agent's
`AGENTISCALE_CONFIG_DIR`. Your agent manages the credentials there.

Each configured service has its own directory under `<data-dir>/acp-agents`,
including services that use the same launcher. The local sign-in command shows
the exact path. If an earlier development build kept your sign-in under
`<data-dir>/acp/<program>`, sign in once for each service using its new directory.

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

Your `session/prompt` response ends the turn. When both sides have advertised
`resultText`, you can put the final text and its status together in that
response. For example, this reply asks the caller a question.

```json
{
  "stopReason": "end_turn",
  "_meta": {
    "agentiscale": { "status": "needs_input", "text": "Which branch should I review?" }
  }
}
```

If you omit `text` or send `null`, the node reads the final streamed agent
message. Use the same ACP `messageId` on all its `agent_message_chunk`
updates and a different ID for each earlier message. This keeps commentary
out of the answer. If your agent omits IDs, send the final answer together
after its tool calls. Only updates for this prompt count. A complete response
still counts if the agent closes stdout immediately afterward.

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
- The final text may be up to 1 MiB. An explicit empty string is an empty
  result for `done`. A question must contain text. To assemble interleaved
  messages, the node retains up to 1,024 message IDs and 16 MiB of IDs and
  text per prompt. If the stream exceeds either limit, supply negotiated
  final text in the response.

An agent that omits `status: true` at `initialize` gets a prompt asking for
one JSON object, `{"status":"done"|"needs_input","text":"…"}`. The node parses
the final message for this answer. A status in the prompt response takes
precedence. Plain text is returned to the caller as a completed result.

## Cancellation

When the caller cancels the task, or it runs past its time, the node closes the
turn's network access and cancels its in-flight network requests. It sends
`session/cancel`, never before the prompt it cancels. End the turn with
`stopReason` `cancelled`. A permission request that the node answers after the
cancel gets the `cancelled` outcome. The agent has up to five seconds to end
a cancelled prompt. At task or node shutdown, flush session data and exit when
stdin closes. The node waits for active and waiting agents to exit and kills
process groups that exceed the grace period.

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

`runtime check` runs the startup probe and the turns in `-dir`, which defaults
to a new empty directory. It checks `initialize`, a completed turn, and whether
the process that asked a question can use the answer and the earlier context.
If the agent advertises `loadSession`, a separate check starts a fresh process
and verifies that it resumes the session. The cancellation check sends a long
prompt and starts its cancellation timer once the prompt has been written.
If the agent completes that turn before cancellation, the checker reports `WARN`.
A protocol error fails the check. Each turn has a five-minute deadline by default
(`-timeout`).

The turns run your agent's model. Use `-owner-config=false`, `-dir-instructions=false`
or `-dir-skills=false` to exercise a particular policy. To check a configured
service's private sign-in, pass its `-name` and node `-data-dir` together with
`-owner-config=false`.

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
