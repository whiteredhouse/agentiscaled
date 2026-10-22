# Get started with agentiscale

Put your machine's agents on your private agentiscale network in two steps: install
`agentiscaled` and run `agentiscaled up`, which also connects your agent apps. You need
a Google account. You do not need Go or the source code.

## 1. Install

On macOS or Linux with [Homebrew](https://brew.sh):

```sh
brew install whiteredhouse/tap/agentiscaled
```

On Debian, Ubuntu, Fedora or RHEL without Homebrew, download the `.deb` or `.rpm` for
your machine from the [latest release](https://github.com/whiteredhouse/agentiscaled/releases/latest)
and install it with `sudo apt install ./agentiscaled_*.deb` or
`sudo dnf install ./agentiscaled_*.rpm`.

Check it with `agentiscaled version`.

## 2. Join this machine

```sh
agentiscaled up
```

`up` opens your browser to sign in with Google and shows a code. Check that the page
shows the same code, and approve. On a machine without a browser, such as a server,
open the printed link on your phone or laptop instead.

The first time you sign in, agentiscale creates your private network, which takes
about a minute. `up` waits for it, joins this machine, and starts the node as a
background service of your user account, so it runs again after a restart or when you
log in. Your sign-in is used once and not stored on the machine.

`agentiscaled status` shows the node, its network and whether it is connected.
`agentiscaled down` stops it; `agentiscaled up` starts it again without signing in.

On a Linux server, run the `sudo loginctl enable-linger <you>` command that `up`
prints, so the node keeps running after you log out.

In a terminal, `up` then asks two questions. Pressing Enter at each takes its default,
which is what `up` does without asking:

```text
Agent clients on this machine can use this node.
  Connect Claude Desktop? [Y/n]:
  Connect Codex CLI? [Y/n]:

Offer an agent from this machine to your network? [y/N]:
```

`up -y` asks nothing, and neither does `up` in a script, a service or an SSH session
without a terminal. Flags answer the questions too: `-clients` the first,
`-agent` and `-agent-dir` the second.

## 3. Your agents are connected

`up` ends by adding this node to the agent apps it finds on the machine: Claude Desktop
(macOS) and the Codex CLI. It prints what it did:

```text
Agent clients:
  Claude Desktop: added "agentiscale" to /Users/you/Library/Application Support/Claude/claude_desktop_config.json (the file before: …). Quit Claude Desktop (Cmd+Q) and open it again to load it.
  Codex CLI: added "agentiscale" with codex mcp add; new Codex sessions have it.
```

Quit and reopen Claude Desktop, then ask it to "show my agentiscale mesh info" to check
the connection.

`up` only adds an `agentiscale` server. It keeps everything else in the apps'
configuration, saves a copy of a file before changing it, and never changes a server
that is already there: if an app already has a server named `agentiscale` for something
else, `up` leaves it and prints the lines to add yourself. Running `up` again finds its
entry and changes nothing. An app installed later gets connected by the next `up`, or
by hand with the lines in [Connect an agent client](connect-agents.md).

Answering n for an app, or `-clients none`, leaves it as it is; `-clients none` also
prints the lines to add yourself, and `-clients codex` connects only the Codex CLI.

## Offer an agent to your network

To let your other agents send tasks to an agent on this machine, answer y to "Offer an
agent from this machine to your network?" and give its name in your network and the
directory it works in. Press Enter at the directory for a workspace of its own, which
`up` creates: `~/Library/Application Support/agentiscaled/workspaces/<name>` on macOS
and `~/.local/share/agentiscaled/workspaces/<name>` (`$XDG_DATA_HOME`) on Linux.

```text
Offer an agent from this machine to your network? [y/N]: y
  Runtime: codex (the only one today)
  Name of the agent in your network [codex]:
  What it does, as other agents see it (Enter for the runtime's):
  Directory the agent works in [~/Library/Application Support/agentiscaled/workspaces/codex]: ~/work/my-project
  Use your own codex configuration (sign-in, skills, plugins, MCP servers)? [Y/n]:
  Load the directory's instruction files (AGENTS.md)? [Y/n]:
  Load the directory's skills? [Y/n]:
```

By default the agent runs as your own Codex does: with your sign-in, skills, plugins,
settings and MCP servers (your `CODEX_HOME`), and with the directory's own instruction
files (`AGENTS.md`) and skills. Your MCP servers include agentiscale itself if you
connected Codex, so the agent can ask your network's other agents for help. Answer n
to give the agent its own configuration and sign-in in its node's data directory, with
no MCP servers (`-agent-owner-config=false`), or to leave out the directory's
instruction files or skills when it may hold content from someone you don't trust
(`-agent-dir-instructions=false`, `-agent-dir-skills=false`).

The agent gets a node of its own, so your agent apps stay on the first one, which only
sends. If the agent's Codex is not signed in, `up` runs `codex login` first (with its
own configuration, for the agent's `CODEX_HOME`), then sets up that node as

```sh
agentiscaled up -y -data-dir <config dir>/agentiscale-codex-my-project -agent codex -agent-dir ~/work/my-project
```

with `-agent-name <name>` added for a name other than the default, and
`-agent-description "<text>"` for a description of your own. That signs in to
agentiscale once more for the new node. You can run that line
yourself instead. What the `codex` runtime does today, with an example from Claude
Desktop, is in
[Send a task from Claude Desktop to Codex](connect-agents.md#send-a-task-from-claude-desktop-to-codex).

## More than one node on a machine

Each node has its own data directory. Give a second node another one, and pass the
same `-data-dir` to `status`, `down` and `mcp`. Its server in your agent apps is named
after the directory, such as `agentiscale-agent`. A node that offers an agent, like
the one below, is not added to your apps unless you ask with
`-clients claude-desktop,codex`:

```sh
agentiscaled up -data-dir ~/agentiscale-agent -agent codex -agent-dir ~/work/my-project
```

## Machines that cannot sign in

Create a join code in your network and pass it in a file:

```sh
agentiscaled up -join-code-path ./join-code
```
