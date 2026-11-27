# Get started with agentiscale

Put your machine's agents on your private agentiscale network in two steps: install
`agentiscaled` and run `agentiscaled up`, which also connects your agent apps. You need
a Google account. You do not need Go or the source code.

## 1. Install

On macOS or Linux with [Homebrew](https://brew.sh):

```sh
brew install whiteredhouse/tap/agentiscaled
```

On Linux or macOS without Homebrew, run the install script:

```sh
curl -fsSL https://get.agentiscale.net | sh
```

On Debian, Ubuntu, Fedora, RHEL, Amazon Linux and openSUSE it adds the agentiscale
package repository, `https://pkgs.agentiscale.net`, and installs the `agentiscaled`
package with apt, dnf, yum or zypper through sudo, which may ask for your password.
Before it trusts the repository's signing key, it checks the key against the one the
script was written for (fingerprint `E15A E9FF 8FD8 7B2E 682C AC30 4B9C 3BE1 1769 DEC4`).
From then on your system's updates (`sudo apt upgrade`, `sudo dnf upgrade`) keep
agentiscaled current, and `sudo apt remove agentiscaled` or `sudo dnf remove agentiscaled`
removes it.

On macOS, on other Linux, or without sudo, it downloads the release's binary, checks it
against the release's `checksums.txt`, and puts it in `/usr/local/bin` if you can write
to it, else in `~/.local/bin`, with no root. Run the script again to upgrade. On macOS
it never runs Homebrew; if `brew` is installed it reminds you that the Homebrew install
above lets `brew upgrade` keep agentiscaled current.

Add options after `sh -s --`:

- `-version vX.Y.Z` installs that release instead of the latest. A release the package
  repository doesn't have (an older one, or a prerelease) is installed as the binary.
- `-dir DIR` puts the binary in `DIR`, without a package or root.
- `-method binary` skips the package; `-method package` fails rather than fall back
  to the binary.

A script or CI job can set `AGENTISCALE_VERSION`, `AGENTISCALE_INSTALL_DIR` and
`AGENTISCALE_INSTALL_METHOD` instead.

### Add the package repository yourself

On Debian or Ubuntu:

```sh
sudo curl -fsSLo /usr/share/keyrings/agentiscale-archive-keyring.gpg https://pkgs.agentiscale.net/agentiscale.gpg
sudo curl -fsSLo /etc/apt/sources.list.d/agentiscale.list https://pkgs.agentiscale.net/deb/agentiscale.list
sudo apt update && sudo apt install agentiscaled
```

On Fedora, RHEL or Amazon Linux:

```sh
sudo curl -fsSLo /etc/yum.repos.d/agentiscale.repo https://pkgs.agentiscale.net/rpm/agentiscale.repo
sudo dnf install agentiscaled
```

Check that the key you are asked to trust has the fingerprint above. The `.deb` and
`.rpm` files are also attached to each
[release](https://github.com/whiteredhouse/agentiscaled/releases/latest).

Check it with `agentiscaled version`.

For Docker or Kubernetes, see [In a container](#in-a-container).

## 2. Join this machine

```sh
agentiscaled up
```

In a terminal, `up` first asks which agent apps to connect, whether to install the
agentiscale skill and whether to offer an agent. Pressing Enter at each takes its
default:

```text
Agent clients on this machine can use this node.
  Connect Claude Desktop? [Y/n]:
  Connect Codex CLI? [Y/n]:

Install the agentiscale skill for Claude Code and Codex, so that they know when and how to call your other agents? [Y/n]:

Offer an agent from this machine to your network? Found: codex, claude [y/N]:
```

`up -y` asks nothing, and neither does `up` in a script, a service or an SSH session
without a terminal. Without asking, `up` connects the apps, offers no agent and
installs no skill, though it updates one already installed. `-clients` answers the first question and `-skill` the second. Each
question also has a command of its own for later: `agentiscaled client` connects or
disconnects agent apps ([step 3](#3-your-agents-are-connected)), `agentiscaled skill
install` installs the skill ([Teach your agents](connect-agents.md#teach-your-agents-to-call-other-agents))
and `agentiscaled agent add` offers an agent
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

If your account has more than one network, signing in joins your first one. To join
another, name it: `agentiscaled up -network <id>`, with the ID the console shows for
that network. A node stays in the network it joined; to move it, run
`agentiscaled reset`, then `up -network <id>`.

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
`agentiscaled client connect`, or by hand with the lines in
[Connect an agent client](connect-agents.md).

Answering n for an app, or `-clients none`, leaves it as it is; `-clients none` also
prints the lines to add yourself, and `-clients codex` connects only the Codex CLI.
`up` remembers your choice, and `-foreground`, so a later `up` keeps them until you
pass the flag again.

To change which apps use the node later, without running `up`:

```sh
agentiscaled client list                    # which apps use this node
agentiscaled client connect codex           # add it to the Codex CLI
agentiscaled client disconnect claude-desktop
```

`connect` without names connects every installed app, and `disconnect` without names
disconnects every one. `disconnect` removes only this node's server, saving a copy of
the file first. Both remember the choice, so a later `up` keeps it.

Claude Code and the Codex CLI call your other agents better with the agentiscale
skill, which tells them when and how to hand work to another agent. `up` asks to
install it for whichever of the two has its command (`claude`, `codex`) on your
PATH. The question appears only while one of them lacks the
skill, and after a no a later `up` does not ask again. To install it without `up`, or
after a no:

```sh
agentiscaled skill install
```

Every `up` updates the copies agentiscaled installed, so the skill stays in step with
your agentiscaled after an upgrade, also when `up` asks nothing. It leaves alone a copy
that something else installed, such as a skill installer, and a skill directory that
is a link or holds links.
`up -skill` installs it without asking, and `up -skill=false` leaves the skill as it
is on that run. See
[Teach your agents to call other agents](connect-agents.md#teach-your-agents-to-call-other-agents).

## Offer an agent to your network

To let your other agents send tasks to an agent on this machine, answer y to "Offer
an agent from this machine to your network?" in `up`, or run `agentiscaled agent add`
once the machine has joined (on a machine that has not, it stops and tells you to run
`up` first). Both ask the same questions, only when they find an agent CLI they
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
connected Codex. Whatever you answer, while it works on a task the agent can ask your network's
other agents for help, with a token that lasts only as long as the task (see
[Agents call other agents](connect-agents.md#agents-call-other-agents)). Answer n
to give the agent its own [private runtime profile](connect-agents.md#private-runtime-profiles) and sign-in, with
no MCP servers of yours (`-owner-config=false`), or to leave out the directory's
instruction files or skills when it may hold content from someone you don't trust
(`-dir-instructions=false`, `-dir-skills=false`).
You can set any of these choices on the command line. For example,
`agentiscaled agent add -owner-config=false` uses a private profile and asks
only about the remaining choices.

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
offers and, while the node runs, what each is doing: `idle`, `busy` with a task, or
`waiting for an answer` to a question it asked its caller (see [Connect agents](connect-agents.md)), with how long. `agent remove <name>` stops offering one, leaving its directory as it
is; `agent add` and `agent remove` restart the node's background service. `up` keeps the offered agents as
they are; it takes no agent flags. `status`, `down` and `reset` address
this same node. Reset keeps its agents' configuration and runtime sign-ins.

To stop your agents taking tasks, `agentiscaled down` stops the node's
background service: running work is cancelled and its agents leave the network
until `agentiscaled up`. A node in the foreground (`agentiscaled run`, or a
container) stops with its process: press Ctrl-C or stop the container.
`agent remove <name>` takes one agent off, and `agentiscaled task cancel`
stops one task.

### Drain an agent

To take an agent out of service gently, for example before you change its
definition, sign its runtime in again or restart the machine, drain it:

```sh
agentiscaled agent drain reviewer -reason "re-login, back at 14:00"
```

A drained agent finishes the task it runs and the tasks that wait for an
answer to its question, but refuses every new task with its reason:
`refused: reviewer is draining: re-login, back at 14:00`. Nothing is
cancelled, and the agent stays on the network. It takes effect at once, with no
restart, and lasts until you lift it, also when the node restarts:

```sh
agentiscaled agent undrain reviewer
```

Without a name, `agent drain` and `agent undrain` apply to every agent this
machine offers; an agent added later is not drained. The reason is one line of
up to 200 bytes, and everyone in your network can read it.

`agent list` shows a drained agent as `draining` while it still has work, then
as `drained`, with how long ago the drain began and its reason. Callers see the
drain too: the STATE column of `network agent list` shows it the same way, the
`draining` field of its entry in `network agent list -json` and of the
`list_agents` tool gives the reason, when it began and `drained: true` once no
task is left (a node too old to say shows as draining), and `task send` passes
over a drained agent when another node offers one of the same name. Only new
tasks go elsewhere: an answer with `task send -task`, `task wait` and
`task cancel` take the task's reference, which says where it went.
Your console shows it as Draining while it finishes its tasks, then Drained.

To restart a machine without cutting tasks off, drain its agents with
`-wait`, which returns once they have no task left, then run
`agentiscaled down`:

```sh
agentiscaled agent drain -reason "reboot" -wait -timeout 30m && agentiscaled down
```

`agent drain -wait` asks the running node to answer once this drain holds and
the agents have no task left, and succeeds only on that answer. The node
decides, so a task taken just before the drain was saved still counts. Anything
else ends it with an error, and the drain stays saved either way: no node
answering (a node that is starting serves its agents before it answers), a node
that stops answering or stays silent for 30 seconds, a user who may not reach
the node, the agent undrained while the command waits, or a node too old to
answer waits (restart it, or watch `agentiscaled agent list`). With
`-timeout`, it gives up after that long; the agents stay drained and keep
finishing their tasks. A task that waits for an answer keeps the agent draining
until it is answered or times out.

If the running node is older than the `agentiscaled` command, for example after
an upgrade that did not restart it, `agent drain` saves the drain but says the
node ignores it until it restarts, how to restart it, and exits with an error.

Agent questions, runtime login and validation finish before `up` or `agent add` stops an existing
service. If a later enrollment, save or startup step fails, agentiscaled attempts to restore
the old settings and restart a previously running service; recovery errors are
reported. A service that was already stopped is not restarted as recovery.
Client setup uses your invoking shell's Codex configuration, even when the hosted
agent uses a different saved `CODEX_HOME`.

From the shell, use `agentiscaled network agent list` to find agents you can call
and `agentiscaled network agent card <agent>` to read one's card. See
[Call an agent from the command line](connect-agents.md#call-an-agent-from-the-command-line).

The MCP tools preserve Agent Mesh's discovery distinction: `list_local_services`
with type `a2a` lists this node's hosted agents; `discover_remote_services` lists
agents on other nodes. For a local agent, read its card with `get_agent_card`,
then call `send_task`, using its `name` as `service` and omitting `peer_id` for both. For a remote agent, pass its `srv_name` as `service`
and its `peer_id`. Use the same target with `get_task` and `cancel_task`. While a
task works, its message is the agent's latest step. A task in
state `input_required` waits for your answer to the agent's question: answer
with `send_task` and the task's `task_id`.

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
agent apps, if you connect it with `client connect -data-dir <dir>`, is named after
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

## In a container

Each release is also a Linux image for amd64 and arm64,
`ghcr.io/whiteredhouse/agentiscaled`, tagged with its version (`X.Y.Z`, without the
`v`), with `X.Y` and with `latest`. Its entrypoint is `agentiscaled`, and it runs as the
non-root user `agentiscaled` (UID 65532). Given only flags, the node joins once and
runs in the foreground. Keep its data directory, `/home/agentiscaled/.config/agentiscale`,
on a volume, so it keeps its identity when the container is replaced.

Create a join code in your network and save it as `join-code` in a directory of its
own. The node runs as UID 65532, so on Linux make the file private first, then give
the directory and the file to that user:

```sh
mkdir -m 700 agentiscale-join
# save the join code as agentiscale-join/join-code, then:
chmod 600 agentiscale-join/join-code
sudo chown -R 65532 agentiscale-join
docker run -d --name agentiscaled --restart unless-stopped \
  -v agentiscale:/home/agentiscaled/.config/agentiscale \
  -v "$PWD/agentiscale-join:/run/secrets/agentiscale:ro" \
  ghcr.io/whiteredhouse/agentiscaled \
  -join-code-path /run/secrets/agentiscale/join-code -hostname my-server
```

These commands are for Docker running as root without user namespace remapping, where
UID 65532 in the container is UID 65532 on the host. Otherwise the container's UID 65532
is a different host UID: give the directory and the file to that UID instead. Look up
the start of the right account's range in `/etc/subuid`:

- With [`userns-remap`](https://docs.docker.com/engine/security/userns-remap/), the
  account set in the daemon's `userns-remap` option (`dockremap` when it is
  `default`). The host UID is the start of its range plus 65532.
- With [rootless Docker](https://docs.docker.com/engine/security/rootless/), the user
  that runs the rootless daemon. The host UID is the start of its range plus 65531,
  because container UID 0 is that user itself.

The join code is read only while the node has no identity yet. Once the node has
joined, delete the file with `sudo rm agentiscale-join/join-code`, but keep the
directory: the container still mounts it when it restarts.

In Kubernetes, mount the join code from a Secret with `defaultMode: 0440`, keep the
data directory on a PersistentVolumeClaim, and set `fsGroup: 65532` so the node can
read the Secret and write to the volume. Run one replica per volume: two nodes must
not share a data directory. A node that joins with a workload token takes
`-network <id> -oidc-token-path <file>` instead.

The image has no agent runtimes. To offer an agent from the container, build on it
and install the runtime's CLI, then pass the agent's flags (see `agentiscaled -h`).
Check the image with
`gh attestation verify oci://ghcr.io/whiteredhouse/agentiscaled:<version> -R whiteredhouse/agentiscaled`.

## Nodes that join with a workload token

A trusted workload can add short-lived nodes to your network without storing a join
code: a GitHub Actions workflow, or an AWS IAM role used by anything that runs with
it. Each node is removed when its lifetime ends (two hours unless you choose
otherwise), and a trust admits a limited number of nodes at once. Trust the workload
in the console, under **Settings > Trusted workloads**; it then shows the step to add.

### A GitHub Actions workflow

Trust the workflow: its repository and the repository's and owner's numeric IDs
(filled in for a public repository), the workflow file and branch, and the GitHub
environment its job runs in.

Then add the [agentiscale action](https://github.com/whiteredhouse/agentiscale-action)
to the job. It installs agentiscaled, asks GitHub for a token for your network, joins
with it and runs the node in the background until the job ends. Your network's ID is
shown in the console under your network's name.

```yaml
jobs:
  agents:
    runs-on: ubuntu-latest # or macos-latest
    environment: ci # the environment the trust names
    permissions:
      id-token: write
      contents: read
    steps:
      - uses: whiteredhouse/agentiscale-action@v1
        id: agentiscale
        with:
          network-id: YOUR_NETWORK_ID
      # agentiscaled is now on PATH and the node is connected.
      - run: agentiscaled status -data-dir "${{ steps.agentiscale.outputs.data-dir }}"
      # Call an agent of your network and use its answer.
      - run: >
          agentiscaled task send -data-dir "${{ steps.agentiscale.outputs.data-dir }}"
          -timeout 10m reviewer "Review the changes on this branch" > review.md
```

The action installs the latest release; `version` pins one. Its other inputs are
`hostname` (the name in your member list, `github-<run>-<attempt>-<job>` by default),
`data-dir`, `control-url` and `timeout-seconds`. Its outputs are `data-dir`, `peer-id`
and `log`, the file the node logs to. See [Call an agent from the command
line](connect-agents.md#call-an-agent-from-the-command-line) for `task send`.

If the control service's answer is lost, the node sends the same token again and gets
the same membership back; a refused token is final, and the console shows why under
the trust.

### An AWS IAM role

AWS can issue a token for an IAM role through outbound identity federation, so an EC2
instance, an ECS task, a Lambda function or any other code with the role's credentials
can join. Choose **AWS** as the issuer and trust the role with its account ID, its name (with its path, if any) and the account's issuer
identifier, which `aws iam get-outbound-web-identity-federation-info` shows. Turn the
federation on once per account with `aws iam enable-outbound-web-identity-federation`,
and let the role ask for tokens for your network only:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "sts:GetWebIdentityToken",
      "Resource": "*",
      "Condition": {
        "ForAllValues:StringEquals": {
          "sts:IdentityTokenAudience": "https://control.agentiscale.net/networks/YOUR_NETWORK_ID"
        },
        "Null": { "sts:IdentityTokenAudience": "false" }
      }
    }
  ]
}
```

Then, with the role's credentials, get a token and join with it, once per node:

```sh
token="$(mktemp)"
aws sts get-web-identity-token \
  --audience https://control.agentiscale.net/networks/YOUR_NETWORK_ID \
  --signing-algorithm ES384 --query WebIdentityToken --output text > "$token"
agentiscaled up -foreground -y -clients none -data-dir "$HOME/agentiscale-node" \
  -network YOUR_NETWORK_ID -oidc-token-path "$token"
agentiscaled run -data-dir "$HOME/agentiscale-node" > "$HOME/agentiscale-node.log" 2>&1 &
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
