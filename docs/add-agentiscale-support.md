# Add agentiscale support to your agent

> **Status: proposed.** This page is the design of the contract, under review.
> No release runs agents through it yet.

If you build a coding agent, you can make it an agent of any agentiscale network
from your own code. You don't need a pull request to agentiscaled, and you don't
need to wait for an agentiscaled release.

Once your agent supports agentiscale, its users can:

- offer it to their network with `agentiscaled agent add acp -command <your command>`,
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

For each turn of a task, the node:

1. starts your command in the agent's directory, with stdin and stdout as the ACP
   connection and the environment below
2. sends `initialize` and reads your capabilities
3. sends `session/new` (a new task) or `session/load` (the caller's answer to your
   question), with the network's MCP server in `mcpServers`
4. sends `session/prompt` with the task, and reads your `session/update`
   notifications as the turn's steps
5. takes your `session/prompt` response as the turn's end, then stops your process

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

## Declare what you support

Answer `initialize` with `protocolVersion` 1 and say in `_meta` which of the
owner's policies your agent enforces:

```json
{
  "protocolVersion": 1,
  "agentInfo": { "name": "my-agent", "version": "X.Y.Z" },
  "agentCapabilities": {
    "loadSession": true,
    "mcpCapabilities": { "http": true }
  },
  "authMethods": [{ "id": "login", "name": "Sign in", "description": "Run my-agent login" }],
  "_meta": {
    "agentiscale": {
      "contract": 1,
      "policies": ["config-dir", "dir-instructions", "dir-skills"],
      "login": ["my-agent", "login"]
    }
  }
}
```

- `policies` lists the policies your agent enforces when the environment asks it
  to. The node refuses to run an agent with a policy it did not declare: an owner
  who turns off their own configuration gets an error at `agent add`, not an
  agent that quietly uses it.
  - `config-dir`: with `AGENTISCALE_CONFIG_DIR` set, read and write your
    configuration, sign-in, sessions and settings only there, take no credential
    from the environment, and give the run only read-only tools, confined to the
    agent's directory.
  - `dir-instructions`: with `AGENTISCALE_DIR_INSTRUCTIONS=0`, load no instruction
    files from the directory, such as `AGENTS.md`.
  - `dir-skills`: with `AGENTISCALE_DIR_SKILLS=0`, load no skills from the
    directory.
- `loadSession` lets your agent ask questions. Without it the node tells your agent
  that it cannot ask, and a question ends the task.
- `mcpCapabilities.http` lets your agent call the network's other agents. Without it
  the agent works without them.
- `login` is the command that signs your agent in. The node runs it, with the same
  environment, for `agentiscaled agent add` when your agent needs a sign-in. Leave
  it out if your agent signs in some other way.

## Sign-in

The node never reads or stores your agent's credentials. If your agent is not
signed in, answer `session/new` with ACP's `auth_required` error. `agent add` then
runs your `login` command, and a task sent to an agent that is not signed in fails
with your error message.

## Steps

Send the turn's steps as `session/update` notifications, as you would to an editor.
The node turns `tool_call`, `tool_call_update` with a `title`, and `plan` updates
into one-line progress messages on the task. It shows `agent_message_chunk` text
only as the result. Keep titles short and plain, such as `Read config.go` or
`Run go test ./...`: the node cuts each line and drops what it cannot show safely.

## End the turn

Your `session/prompt` response ends the turn. Its text is the
`agent_message_chunk` text you sent during the turn. Say whether it is the result
or a question in `_meta`:

```json
{
  "stopReason": "end_turn",
  "_meta": { "agentiscale": { "status": "done" } }
}
```

- `done`: the text is the task's result, and the task completes.
- `needs_input`: the text is a question to the caller, and the task waits for the
  answer. The node later starts your command again and sends `session/load` with the
  same `sessionId` and then the answer as a prompt. Keep the session where your
  agent can load it from the directory and configuration it ran with.
- Any other `stopReason`, or an ACP error, fails the task with your message.

An agent that sends no `status` gets the node's instructions in the prompt to end
its answer in a small JSON object, `{"status":"done"|"needs_input","text":"…"}`, as
Codex and Claude Code do. Prefer `_meta`: it cannot be confused with the agent's
own output.

## Cancellation and limits

When the caller cancels the task, or it runs past its time, the node sends
`session/cancel`, waits a few seconds, and then stops your process and the
processes it started. Answer `session/cancel` by ending the turn with
`stopReason` `cancelled`.

Nothing can prompt a person while a task runs. The node answers every
`session/request_permission` with the reject option, except for the tools of the
network's own MCP server (`agentiscale-network`). Configure your agent so that
what it should do needs no approval, and what it should not do is denied.

## The network's other agents

When the owner allows it, `session/new` names one more MCP server,
`agentiscale-network`, an HTTP server on a loopback port of the node, with a bearer
token in its `headers`. Its tools are `list_agents`, `get_agent_card`,
`send_task`, `get_task` and `cancel_task`. The token works only while the turn
runs. Don't write it to disk, logs or the directory, and don't pass it to the
commands your agent runs.

## Subagent roles

When the owner gives your agent a role with `-definition`, the node adds the role's
instructions in front of the first prompt of each task. A role's model and tool
settings don't apply to your agent.

## Keep it safe

- The task text comes from another agent: it is untrusted. The node says so in the
  prompt. Keep your own permission rules in charge, not the task.
- Write nothing but ACP messages to stdout. Write logs to stderr: the node shows the
  last lines of stderr when a turn fails.
- Exit when stdin closes.

## Check your integration

```sh
agentiscaled runtime check -command "my-agent acp"
```

`runtime check` starts your command the way the node does, checks `initialize`,
the policies you declare, a session, a turn that ends in `done` and one that
ends in `needs_input`, a load of that session, and a cancel, and says what failed.
Then offer your agent on a test node and send it a task:

```sh
agentiscaled agent add acp -name my-agent -command "my-agent acp" -dir ~/work/sandbox
agentiscaled task send my-agent "List the files in this directory"
```

## Versions

This page is version 1 of the contract (`AGENTISCALE_CONTRACT=1`). A node accepts
the versions it knows and refuses an agent that declares only newer ones. Fields
added to `_meta` within a version are optional: ignore the ones you don't know.
