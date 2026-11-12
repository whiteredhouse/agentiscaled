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

In a terminal, `up` first asks which agent apps to connect and whether to offer an
agent. Pressing Enter at each takes its default, which is what `up` does without
asking:

```text
Agent clients on this machine can use this node.
  Connect Claude Desktop? [Y/n]:
  Connect Codex CLI? [Y/n]:

Offer an agent from this machine to your network? Found: codex, claude [y/N]:
```

`up -y` asks nothing, and neither does `up` in a script, a service or an SSH session
without a terminal. `-clients` answers the first question. Each question also has a
command of its own for later: `agentiscaled clients` connects or disconnects agent apps
([step 3](#3-your-agents-are-connected)) and `agentiscaled agent add` offers an agent
([Offer an agent](#offer-an-agent-to-your-network)).

If you offer an agent, its setup questions and any required `codex login` finish
before the agentiscale browser sign-in.

`up` then opens your browser to sign in with Google if this node has not joined
yet, and shows a code. Check that the page shows the same code, and approve. On a machine without a browser, such as a server,
open the printed link on your phone or laptop instead.

The first time you sign in, agentiscale creates your private network, which takes
about a minute. `up` waits for it, joins this machine, and starts the node as a
background service of your user account, so it runs again after a restart or when you
log in. Your sign-in is used once and not stored on the machine.

`agentiscaled status` shows the node, its network and whether it is connected.
`agentiscaled down` stops it; `agentiscaled up` starts it again without signing in.

A node keeps trying to reach its network through an outage. If the network is gone
for good, because it was deleted, the node was removed from it, or the network no
longer accepts the node's credential, the node stops trying and says so in one line,
in `agentiscaled status` and in its log. That line names the fix:

```sh
agentiscaled reset
agentiscaled up
```

`reset` stops and removes the background service, moves the node's identity aside
and clears the recorded stop. It does not sign in or join. `up` then joins the node
to your current network as a new node, by signing in as it did the first time, or
with `up -join-code-path <file>` on a machine where nobody can sign in. Everything
else stays as it was: the options `up` saved, including your choice of agent apps and
of `-foreground`, the agent the node offers and that agent's own sign-in, and the
entries in your agent apps, which need no change.

The node tries to refresh an expired credential before treating a mesh refusal as
final. A temporary refresh failure is not recorded as a permanent halt.
For a node started directly with flags, stop that process before running `reset`,
then restart it with its original flags plus `-join-code-path <file>`. A join code is
redeemed at the hosted control service unless you pass `-control <URL>`.

On a Linux server, run the `sudo loginctl enable-linger <you>` command that `up`
prints, so the node keeps running after you log out.

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
entry and changes nothing. An app installed later gets connected by
`agentiscaled clients connect`, or by hand with the lines in
[Connect an agent client](connect-agents.md).

Answering n for an app, or `-clients none`, leaves it as it is; `-clients none` also
prints the lines to add yourself, and `-clients codex` connects only the Codex CLI.
`up` remembers your choice, and `-foreground`, so a later `up` keeps them until you
pass the flag again.

To change which apps use the node later, without running `up`:

```sh
agentiscaled clients list                    # which apps use this node
agentiscaled clients connect codex           # add it to the Codex CLI
agentiscaled clients disconnect claude-desktop
```

`connect` without names connects every installed app, and `disconnect` without names
disconnects every one. `disconnect` removes only this node's server, saving a copy of
the file first. Both remember the choice, so a later `up` keeps it.

## Offer an agent to your network

To let your other agents send tasks to an agent on this machine, run
`agentiscaled agent add`, or answer y to "Offer an agent from this machine to your
network?" in `up`. Both ask the same questions, only when they find an agent CLI they
can host (`codex`, `claude`), and list the ones found; with several, pick one. Then give its name in your network and the
directory it works in. Press Enter at the directory for a workspace of its own, which
agentiscaled creates: `~/Library/Application Support/agentiscaled/workspaces/<name>` on macOS
and `~/.local/share/agentiscaled/workspaces/<name>` (`$XDG_DATA_HOME`) on Linux.

```text
Offer an agent from this machine to your network? Found: codex, claude [y/N]: y
  Which one (codex, claude) [codex]:
  Name of the agent in your network [codex]:
  What it does, as other agents see it (Enter for the runtime's):
  Directory the agent works in [~/Library/Application Support/agentiscaled/workspaces/codex]: ~/work/my-project
  Use your own codex configuration (sign-in, skills, plugins, MCP servers)? [Y/n]:
  Load the directory's instruction files (AGENTS.md, CLAUDE.md)? [Y/n]:
  Load the directory's skills? [Y/n]:
```

By default the agent runs as your own Codex does: with your sign-in, skills, plugins,
settings and MCP servers (your `CODEX_HOME`), and with the directory's own instruction
files (`AGENTS.md`) and skills. Your MCP servers include agentiscale itself if you
connected Codex, so the agent can ask your network's other agents for help. Answer n
to give the agent its own configuration and sign-in in its node's data directory, with
no MCP servers (`-owner-config=false`), or to leave out the directory's
instruction files or skills when it may hold content from someone you don't trust
(`-dir-instructions=false`, `-dir-skills=false`).

The agent runs on the same node your local apps use. There is one mesh identity,
one sign-in to agentiscale, and one background service. If Codex needs a runtime
sign-in, agentiscaled runs `codex login` during agent setup, before joining the node and
(re)starting the service. Local apps can discover and call this agent through the same
node, as well as agents on other machines.

To offer it without questions, name the runtime:

```sh
agentiscaled agent add codex -dir ~/work/my-project
```

Add `-name <name>` for a name other than the default, or `-description "<text>"` for
your own description; `-dir` defaults to the agent's own workspace. `-definition <file>`
gives the agent the role of a project's subagent definition (`.claude/agents/<name>.md`
or `.codex/agents/<name>.toml`); see [Give an agent a subagent's role](connect-agents.md#give-an-agent-a-subagents-role). Running
`agent add` again with the same name changes that agent, and `agent add` with
another name offers one more agent from the same node, next to the first: there
is no second sign-in, node or service. `agent list` shows the agents this machine
offers, and `agent remove <name>` stops offering one, leaving its directory as it
is; each restarts the node's background service. `up` keeps the offered agents as
they are; it takes no agent flags. `status`, `down`, `reset` and `tasks` address
this same node, and `tasks stop` stops the tasks of all its agents. Reset keeps its
agents' configuration and runtime sign-ins.

Agent questions, runtime login and validation finish before `up` or `agent add` stops an existing
service. If a later enrollment, save or startup step fails, agentiscaled attempts to restore
the old settings and restart a previously running service; recovery errors are
reported. A service that was already stopped is not restarted as recovery.
Client setup uses your invoking shell's Codex configuration, even when the hosted
agent uses a different saved `CODEX_HOME`.

The MCP tools preserve Agent Mesh's discovery distinction: `list_local_services`
with type `a2a` lists this node's hosted agents; `discover_remote_services` lists
agents on other nodes. For a local agent, read its card with `get_agent_card`,
then call `send_task`, using its `name` as `service` and omitting `peer_id` for both. For a remote agent, pass its `srv_name` as `service`
and its `peer_id`. Use the same target with `get_task` and `cancel_task`.

Local card reads and task calls use agentiscaled's owner-only Unix socket. They use the same
agent handler and permissions as network calls; Agent Mesh's network discovery
and proxy routes are unchanged.

See [Send a task from Claude Desktop to Codex](connect-agents.md#send-a-task-from-claude-desktop-to-codex)
for an example.

## More than one node on a machine

One node offers all the agents of a machine, so most machines need no other.
A node in a data directory of its own, such as one run apart for testing, takes
`-data-dir` on `up`, `agent`, `clients`, `status`, `down`, `reset`, `tasks` and
`mcp`, and has its own identity, service and recovery target. Its server in your
agent apps, if you connect it with `clients connect -data-dir <dir>`, is named after
the directory, such as `agentiscale-reviewer`.

Nodes set up by older versions for one agent each, in directories such as
`agentiscale-codex-2` or `agentiscale-<name>`, stay independent: this version does not
move their agents or remove their services. To move such an agent onto the default
node, record its workspace, name, description, and choices for `-owner-config`,
`-dir-instructions` and `-dir-skills`, stop the old node with
`down -data-dir <old-directory>`, then offer the agent again with `agent add` and the
same choices. If another node on your network still serves that name, the node's log
says so shortly after it starts. Remove the old node from your network in the console
once the new setup works. An isolated runtime (`-owner-config=false`) needs its own
login in the default node's data directory. Do not copy node keys or enrollment files
between directories.

Confirm that `status` names the intended network and agents, then use
`list_local_services`, `get_agent_card`, and a harmless task to verify the same-node
path. Existing client entries stay unchanged; if a client is configured for the old
agent node, reconnect it deliberately to the selected node.

## Machines that cannot sign in

Create a join code in your network and pass it in a file, to join the first time or
after `reset`:

```sh
agentiscaled up -join-code-path ./join-code
```

## Delete your account

Delete your account and network in the console, under **Account details > Delete
account**. Every node is removed from your network and disconnected at once, and every
join code stops working. Nodes notice they were removed and stop; `agentiscaled down`
stops the one on this machine.

For 30 days you can sign in to the console and choose **Restore account**. Your
network comes back with its name and history, but its machines were removed: run
`agentiscaled reset` and then `agentiscaled up` on each to join it again. Until you
restore it, `agentiscaled up` can't add a machine and says so.

After 30 days, agentiscale deletes your account and your network's data for good, and
signing in again starts a new, empty account. Uninstall agentiscaled on your machines,
and delete the node's data directory, to remove what is stored locally.
