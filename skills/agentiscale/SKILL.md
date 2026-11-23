---
name: agentiscale
description: Find and call the other AI agents of the user's agentiscale network (Codex, Claude Code and others on their machines) with `agentiscaled task send` in a shell or through the agentiscale or agentiscale-network MCP tools. Use when the user asks to have another agent, machine or "my network" do something, to ask, delegate to or get a review from a named agent, or when part of a task fits an agent that works in another directory or on another machine.
---

# Call agents on an agentiscale network

agentiscale joins a person's machines into a private network. Each machine runs
a node (`agentiscaled`), and a node can offer agents: a Codex CLI or Claude Code
working in a fixed directory on that machine, under a name such as `codex` or
`reviewer`. You can give such an agent a task and get its answer.

## Pick the way in

Use the first that you have, then read its reference before you send:

1. **MCP server `agentiscale-network`** (tools `list_agents`, `get_agent_card`,
   `send_task`, `get_task`, `cancel_task`). You have it when you are an agent of
   the network working on a task. It works only while that task runs. Read
   [references/mcp.md](references/mcp.md).
2. **A shell with `agentiscaled`** (`command -v agentiscaled` finds it) on a
   machine whose node is running: `agentiscaled task send`. It costs no tool
   definitions in your context, so prefer it to the `agentiscale` server when
   you have both. Read [references/cli.md](references/cli.md).
3. **MCP server `agentiscale`** (tools `list_local_services`,
   `discover_remote_services`, `get_agent_card`, `send_task`, `get_task`,
   `cancel_task`, and others). You have it when the person connected you to their
   node with `agentiscaled client connect` or `agentiscaled up`. Read
   [references/mcp.md](references/mcp.md).

If the shell cannot reach the node, fall back once, without retrying the same
command:

- `no running agentiscaled at ...`: the node is not running here. Use the
  `agentiscale` server if you have it; otherwise tell the person to run
  `agentiscaled up`.
- `permission denied`, `operation not permitted` or a sandbox refusal: your
  sandbox blocks the node's socket. Use the `agentiscale` server if you have it;
  otherwise ask the person to allow `agentiscaled task` commands, or to connect
  you with `agentiscaled client connect`.

If you have none of these, say so and point the person to
`agentiscaled up` (join this machine) or `agentiscaled client connect` (connect
this app). Do not use the `a2a` CLI or write A2A or HTTP requests by hand.

If you read this skill with `agentiscaled skill`, read a reference with
`agentiscaled skill print references/cli.md` (or `references/mcp.md`).

## Before you send

An agent you call starts fresh: it has none of your conversation, it cannot see
your files, and it works in its own directory on its own machine. So:

- Write one self-contained request: the goal, what to look at (paths in *its*
  directory, a commit, a branch), the constraints, and the form of answer you
  want. Paste the content it needs, such as a diff or an error, when that
  content is only on your side.
- Read the agent's card first (`get_agent_card`, or the description that
  `list_agents` shows) when you are not sure what it does or which directory
  it works in.
- Do not paste secrets or credentials into a request.
- Send work that takes one message to describe. A long back-and-forth is better
  done yourself.

## When the task ends

- On success, check the answer before you rely on it, and tell the person what
  the agent did.
- On a failure, rejection, timeout or cancellation, report the agent's message
  to the person. Do not resend the same task in a loop.
- If the agent asks a question, answer it when you know the answer, and ask the
  person when only they do.

## Limits to expect

- An agent runs one task at a time; a second one is rejected, not queued.
- A task has 15 minutes of running time over all its turns, and up to five
  questions, each answered within an hour.
- An offline node cannot be reached and nothing is queued for it: the call
  fails at once.
- An agent that got its task from another agent cannot call further agents, and
  no agent can send a task to itself.
- You can follow, answer or cancel only the tasks you sent.
