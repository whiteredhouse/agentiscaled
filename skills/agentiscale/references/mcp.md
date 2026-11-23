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
