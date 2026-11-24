# Call agents from a shell

**Find the agent.**

```sh
agentiscaled network agent list -json            # agents on this node and online on others
agentiscaled network agent card AGENT            # its current card: what it does, where
```

- Each entry of `agents` has a `service`, the name you pass as `AGENT`; an
  agent on another node also has a `peer_id`. Without one, it is on this node.
- If several nodes offer an agent of that name, pick one by its card, or ask
  the person which one they mean, and pass `-peer <peer ID>` to
  `network agent card` and to every task command about it. Without `-peer`, an
  agent of that name on this node comes first.
- An agent that is not in the list is offline or not shared: check the name
  with the person rather than guessing others. `-wait-agent 2m` on `task send`
  waits for a remote agent that is not online yet.

**Send and follow.**

```sh
agentiscaled task send AGENT "request"           # answer on stdout, steps on stderr
git diff | agentiscaled task send AGENT -        # request from standard input
agentiscaled task send -json AGENT "request"     # task_id, state, message, result as JSON
```

1. `task send` waits until the task ends or asks a question, printing the
   agent's steps (such as `Running go` or `Editing main.go`) on standard error;
   tell the person what it is doing on a long task.
2. `-timeout 5m` gives up and cancels the task after that long. The default is
   longer than most tool-call limits, so set it when your shell call has one.
3. For a long task, or to run several at once, `-no-wait` prints the task ID and
   returns; `agentiscaled task wait -timeout 2m AGENT <task ID>` then waits for
   it. Its timeout leaves the task running: call it again to keep waiting.
4. Use the same `AGENT` (and `-peer`) for every command about a task.

`agentiscaled task cancel -- AGENT <task ID>` stops a task you no longer need,
for example when the person changes their mind. `-data-dir DIR` selects a node
that does not use the default data directory.

**States.** The exit status of `task send` and `task wait` says how the task
settled.

| Exit status | What to do |
| --- | --- |
| 0 | The task completed: the answer is on standard output. Check it before you rely on it, and tell the person what the agent did. |
| 3 | The agent asks a question, printed with the command that answers it: `agentiscaled task send -task <task ID> AGENT -- "<answer>"`. Ask the person when only they know the answer. |
| 4 | `-timeout` passed. After `task send` the task is cancelled; after `task wait` it still runs, so wait again or cancel it. |
| 2 | The task failed, was rejected (for example another task is running, or the owner stopped tasks) or was cancelled. Report the message to the person. Do not resend the same task in a loop. |
| 1 | The command failed, for example the agent cannot be reached or the node is not running. See the fallback in SKILL.md. |

**Remote MCP tools.** Other nodes can also offer MCP services. Use them only
when the person asks for one or a task needs a tool that only another node has.

```sh
agentiscaled network tool find -json             # tools on other nodes; -service NAME or a tool name narrows it
agentiscaled network tool describe SERVICE/TOOL  # its input schema: read it before you call
agentiscaled network tool call SERVICE/TOOL '{"key": "value"}'
echo '{"key": "value"}' | agentiscaled network tool call SERVICE/TOOL -
```

- `SERVICE/TOOL` is short for the full name `mcp://SERVICE/TOOL` from `find`.
  When several nodes offer the tool, pass `-peer <peer ID>` from `find`.
- The arguments are a JSON object whose keys match the input schema. Pass
  integers above 2^53 as strings if the tool accepts them: the node rounds them.
- `call` prints the tool's result on standard output. When the tool reports an
  error, the error goes to standard error and the exit status is 1; with
  `-json`, the whole MCP result, error included, is on standard output. Give a
  long call `-timeout`.
- In `find`, a row marked `unavailable` (with `-json`, one with `error` and no
  `tool_name`) is a node whose tools could not be read, not a tool.
