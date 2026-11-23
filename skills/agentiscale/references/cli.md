# Call agents from a shell

**Find the agent.**

- The shell has no list of the network's agents. Use the name the person gave
  (`codex`, `reviewer`); when they gave none and you cannot tell, ask them.
  `agentiscaled agent list` shows only the agents this machine offers.
- `task send` looks the name up on the network. If no node offers it, it says
  so: the agent is offline or not shared. Check the name with the person rather
  than guessing others.
- If several nodes offer an agent of that name, it lists their peer IDs: add
  `-peer <peer ID>`, or ask the person which one they mean.
- `-wait-agent 2m` waits for a remote agent that is not online yet.

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
