package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
)

const clientsUsage = `usage: agentiscaled clients list|connect|disconnect [-data-dir DIR] [client ...]

  list        show which agent clients use this node
  connect     add this node to the clients' MCP configuration (default: every installed one)
  disconnect  remove this node from the clients' MCP configuration (default: every one)

Clients: ` + "claude-desktop, codex"

// runClients is `agentiscaled clients`: it connects agent clients to a node
// that `up` set up, or disconnects them, apart from bringing the node up.
// It records the choice, so that a later `up` keeps it.
func runClients(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "help" {
		return errors.New(clientsUsage)
	}
	cmd := args[0]
	if cmd != "list" && cmd != "connect" && cmd != "disconnect" {
		return fmt.Errorf("unknown clients command %q\n%s", cmd, clientsUsage)
	}
	fs := flag.NewFlagSet("clients "+cmd, flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node")
	names, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return err
	}
	for _, n := range names {
		if !slices.Contains(clientNames, n) {
			return fmt.Errorf("unknown client %q (have %s)", n, strings.Join(clientNames, ", "))
		}
	}
	if cmd == "list" && len(names) > 0 {
		return errors.New("clients list takes no clients")
	}
	dir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	s, err := loadSettings(dir)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no node set up in %s; run 'agentiscaled up' first", dir)
	} else if err != nil {
		return err
	}
	switch cmd {
	case "list":
		listClients(ctx, out, dir)
		return nil
	case "connect":
		exe, err := stableExecutable()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			if names = installedClients(); len(names) == 0 {
				fmt.Fprintf(out, "No agent client (%s) is installed.", strings.Join(clientNames, ", "))
				printNextSteps(out, exe, dir)
				return nil
			}
		}
		connectClients(ctx, out, selectClients(strings.Join(names, ",")), exe, dir)
	case "disconnect":
		if len(names) == 0 {
			names = systemClients()
		}
		disconnectClients(ctx, out, selectClients(strings.Join(names, ",")), dir)
	}
	s.Clients = withClients(s.Clients, names, cmd == "connect")
	return saveSettings(dir, s)
}

// parseInterleaved parses flags given before, between or after the
// arguments, and answers the arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return rest, nil
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// systemClients are the clients this system can have, by their -clients
// names, as auto selects them.
func systemClients() []string {
	var names []string
	for _, n := range clientNames {
		if n != "claude-desktop" || goruntime.GOOS == "darwin" {
			names = append(names, n)
		}
	}
	return names
}

// withClients is the saved -clients value after connecting (add) or
// disconnecting the named clients. auto stays auto when clients are added,
// since it already takes every installed one.
func withClients(saved string, names []string, add bool) string {
	var base []string
	switch saved {
	case "", clientsAuto:
		if add {
			return clientsAuto
		}
		base = systemClients()
	case "none":
	default:
		for _, n := range strings.Split(saved, ",") {
			base = append(base, strings.TrimSpace(n))
		}
	}
	var list []string
	for _, n := range clientNames {
		inBase, named := slices.Contains(base, n), slices.Contains(names, n)
		if add && (inBase || named) || !add && inBase && !named {
			list = append(list, n)
		}
	}
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ",")
}

// listClients says, for each client this system can have, whether it
// uses the node.
func listClients(ctx context.Context, out io.Writer, dataDir string) {
	for _, n := range systemClients() {
		c := selectClients(n)[0]
		fmt.Fprintf(out, "%-15s (%s): %s\n", c.name(), n, clientState(ctx, c, dataDir))
	}
}

func clientState(ctx context.Context, c agentClient, dataDir string) string {
	if !c.installed() {
		return "not installed"
	}
	have, err := c.entries(ctx)
	if err != nil {
		return fmt.Sprintf("could not read its configuration (%v)", err)
	}
	switch found := entryForNode(have, dataDir); {
	case found == "":
		return "not connected"
	case have[found].Disabled:
		return fmt.Sprintf("connected as %q, but turned off", found)
	default:
		return fmt.Sprintf("connected as %q", found)
	}
}

// disconnectClients removes every entry that serves the node from each
// client's MCP configuration, and leaves every other entry as it is.
func disconnectClients(ctx context.Context, out io.Writer, clients []agentClient, dataDir string) {
	fmt.Fprintln(out, "Agent clients:")
	for _, c := range clients {
		if !c.installed() {
			fmt.Fprintf(out, "  %s: not installed.\n", c.name())
			continue
		}
		have, err := c.entries(ctx)
		if err != nil {
			fmt.Fprintf(out, "  %s: could not read its configuration (%v); left it as it is.\n", c.name(), err)
			continue
		}
		var found []string
		for n, e := range have {
			if servesNode(e, dataDir) {
				found = append(found, n)
			}
		}
		if len(found) == 0 {
			fmt.Fprintf(out, "  %s: not connected to this node.\n", c.name())
			continue
		}
		slices.Sort(found)
		for _, n := range found {
			did, err := c.remove(ctx, n)
			if err != nil {
				fmt.Fprintf(out, "  %s: %v. Remove %q from its configuration yourself.\n", c.name(), err, n)
				continue
			}
			fmt.Fprintf(out, "  %s: %s\n", c.name(), did)
		}
	}
}
