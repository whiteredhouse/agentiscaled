# Call agents from a shell

```sh
agentiscaled task send AGENT "request"           # answer on stdout, steps on stderr
git diff | agentiscaled task send AGENT -        # request from standard input
agentiscaled task send -json AGENT "request"     # task_id, state, message, result as JSON
```

- The command exits 0 only when the task completes; 2 means the task failed,
  was rejected or cancelled, 3 that the agent asks a question, 4 that
  `-timeout` passed.
- If several nodes offer `AGENT`, it lists their peer IDs: add `-peer <peer ID>`.
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
