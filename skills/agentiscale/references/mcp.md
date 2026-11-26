# Call agents with the MCP tools

**Find the agent.**

- `agentiscale-network`: call `list_agents`. Each row has `service`, a
  `peer_id` for an agent on another node (none for one on this node) and its
  `description`.
- `agentiscale`: call `list_local_services` with type `a2a` for agents on this
  node (use the name as `service`, no `peer_id`), and
  `discover_remote_services` with type `a2a` for agents on other nodes (use its
  `peer_id`, and `srv_name` as `service`).

If several nodes offer an agent of the same name, pick by `peer_id` and its
card, or ask the person which one they mean.

An agent whose owner is taking it out of service is draining: it refuses new
tasks. `list_agents` gives it `draining` with the owner's `reason`; Agent Mesh's
tools give the same mark as JSON under the `agentiscale.mark.drain` key of its
`metadata` (`list_local_services`) or `srv_metadata`
(`discover_remote_services`). Pick another agent, or tell the person.

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

**Remote MCP tools.** Only the `agentiscale` server has these. Other nodes can
also offer MCP services; use them only when the person asks for one or a task
needs a tool that only another node has.

1. `find_remote_tools` lists them, narrowed by `service_name`, `tool_name` or
   `peer_id`. Each row has a `peer_id` and a full `tool_name` such as
   `mcp://code-reviewer/review_pr`. A row with `error` and no `tool_name` is
   a node whose tools could not be read: skip it, and use only rows with a
   `tool_name`.
2. `describe_remote_tool` with that `peer_id` and `tool_name` returns its input
   schema. Read it before you call.
3. `call_remote_tool` with the same `peer_id` and `tool_name`, and `arguments`
   as a JSON object matching the schema. Pass integers above 2^53 as strings if
   the tool accepts them: the node rounds them.
