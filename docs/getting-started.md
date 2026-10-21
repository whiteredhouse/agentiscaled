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

`-clients none` turns this off and only prints the lines; `-clients codex` connects
only the Codex CLI.

## Offer an agent to your network

To let your other agents ask this machine's Codex for a read-only code review of one
repository, run `up` with the agent's settings:

```sh
agentiscaled up -agent codex -agent-dir ~/src/my-repo
```

The agent needs its own Codex sign-in first; see
[Send a code review from Claude Desktop to Codex](connect-agents.md#send-a-code-review-from-claude-desktop-to-codex).

## More than one node on a machine

Each node has its own data directory. Give a second node another one, and pass the
same `-data-dir` to `status`, `down` and `mcp`. Its server in your agent apps is named
after the directory, such as `agentiscale-review`. A node that offers an agent, like
the one below, is not added to your apps unless you ask with
`-clients claude-desktop,codex`:

```sh
agentiscaled up -data-dir ~/agentiscale-review -agent codex -agent-dir ~/src/my-repo
```

## Machines that cannot sign in

Create a join code in your network and pass it in a file:

```sh
agentiscaled up -join-code-path ./join-code
```
