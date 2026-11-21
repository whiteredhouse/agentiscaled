---
name: agentiscale
description: Find and call the other AI agents of the user's agentiscale network (Codex, Claude Code and others on their machines or in the cloud) through the agentiscale or agentiscale-network MCP tools, or `agentiscaled task send` in a shell. Use when the user asks to have another agent, machine or "my network" do something, to ask, delegate to or get a review from a named agent, or when part of a task fits an agent that works in another directory or on another machine.
---

# Call agents on an agentiscale network

agentiscale joins a person's machines into a private network. Each machine runs
a node (`agentiscaled`), and a node can offer agents: a Codex CLI or Claude Code
working in a fixed directory on that machine, under a name such as `codex` or
`reviewer`. A network can also have cloud agents provisioned in its console. You can give either kind of agent a task and get its answer.

## Pick the way in

Use the first that you have:

1. **MCP server `agentiscale-network`** (tools `list_agents`, `get_agent_card`,
   `send_task`, `get_task`, `cancel_task`). You have it when you are an agent of
   the network working on a task. It works only while that task runs.
2. **MCP server `agentiscale`** (tools `list_local_services`,
   `discover_remote_services`, `list_agents`, `get_agent_card`, `send_task`, `get_task`,
   `cancel_task`, and others). You have it when the person connected you to their
   node with `agentiscaled client connect` or `agentiscaled up`.
3. **A shell with `agentiscaled`** on a machine whose node is running:
   `agentiscaled task send`. See [From a shell](#from-a-shell).

If you have none of these, say so and point the person to
`agentiscaled up` (join this machine) or `agentiscaled client connect` (connect
this app). Do not use the `a2a` CLI or write A2A or HTTP requests by hand.

## Before you send

An agent you call starts fresh: it has none of your conversation, it cannot see
your files, and it works in its own directory on its own machine. So:

- Write one self-contained request: the goal, what to look at (paths in *its*
  directory, a commit, a branch), the constraints, and the form of answer you
  want. Paste the content it needs, such as a diff or an error, when that
  content is only on your side.
- Read the agent's card first (`get_agent_card`) when you are not sure what it
  does or which directory it works in.
- Do not paste secrets or credentials into a request.
- Send work that takes one message to describe. A long back-and-forth is better
  done yourself.

## With MCP tools

**Find the agent.**

On either MCP server, call `list_agents` to discover agents visible to the
caller. To narrow the list, pass a `location` of `local` (this node), `mesh`
(another node), or `cloud` (a console-provisioned cloud agent). Pass the row's
`service` and, when present, `peer_id` to the card and task tools. A cloud
target has a `peer_id` of `cloud:<agent UUID>`.

If several agents have the same name, use their location, target and card to
choose one, or ask the person which one they mean.

**Send and follow.**

1. `send_task` with the target (`service`, plus `peer_id` for a remote agent)
   and `text`. It returns at once with a `task_id` and a state.
2. `get_task` with the same target and `task_id`, and `wait_seconds` (at most
   50) so that the call waits for a change. Repeat until the task asks a
   question (state `input_required`) or ends (`terminal` is set). While it
   works, `message` is the agent's latest step, such as `Running go` or
   `Editing main.go`; tell the person what it is doing on a long task.
3. Use the same target for every call about a task.

**States.** Once a task ends, `terminal` says how.

| State or `terminal` | What to do |
| --- | --- |
| `submitted`, `working` | Keep following with `get_task`. |
| `input_required` | The agent asks a question (in `message`). Answer it with `send_task`, giving the same target, the `task_id` and your answer as `text`, then follow again. Ask the person when only they know the answer. |
| `completed` | `result` is the answer. Check it before you rely on it, and tell the person what the agent did. |
| `rejected` | The agent did not start, `message` says why, for example another task is running or the owner stopped tasks. Try later or tell the person. |
| `failed`, `timeout`, `cancelled` | Report `message` to the person. Do not resend the same task in a loop. |

`cancel_task` stops a task you no longer need, for example when the person
changes their mind.

## From a shell

```sh
agentiscaled task send AGENT "request"           # answer on stdout, steps on stderr
git diff | agentiscaled task send AGENT -        # request from standard input
agentiscaled task send -json AGENT "request"     # task_id, state, message, result as JSON
```

- The command exits 0 only when the task completes; 2 means the task failed,
  was rejected or cancelled, 3 that the agent asks a question, 4 that
  `-timeout` passed.
- Run `agentiscaled agent list` to discover agents, or use
  `agentiscaled agent list -location cloud` to see just the cloud targets.
  If names overlap, select a target with `-peer`.
- `-timeout 5m` gives up and cancels the task after that long (the default is
  longer than most tool-call limits, so set it when your shell call has one).
- `-wait-agent 2m` waits for a remote agent that is not online yet.
- For a long task, or to run several at once, `-no-wait` prints the task ID and
  returns; `agentiscaled task wait -timeout 2m AGENT <task ID>` then waits for
  it. Its timeout leaves the task running (exit 4): call it again to keep
  waiting, or stop it with `agentiscaled task cancel -- AGENT <task ID>` when
  you no longer need the answer.
- `-data-dir DIR` selects a node that does not use the default data directory.
- If the agent asks a question, the command prints it with the command that
  answers it: `agentiscaled task send -task <task ID> AGENT -- "<answer>"`.

## Limits to expect

- An agent runs one task at a time; a second one is rejected, not queued.
- An agent on a node has 15 minutes of running time over all its turns, and up to five
  questions, each answered within an hour. Cloud tasks have a 15-minute deadline
  from acceptance, including time waiting for answers.
- An offline node cannot be reached and nothing is queued for it: the call
  fails at once. For cloud agents the shared task service accepts work while
  compute sleeps and keeps the task ID available across worker restarts.
- An agent that got its task from another agent cannot call further agents, and
  no agent can send a task to itself.
- You can follow, answer or cancel only the tasks you sent.
