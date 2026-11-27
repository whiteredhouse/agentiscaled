package caller

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/whiteredhouse/agentiscaled/internal/cli"
	"github.com/whiteredhouse/agentiscaled/internal/nodestate"
)

const networkUsage = "usage: agentiscaled network agent list [flags]\n       agentiscaled network agent card [flags] AGENT\n" + toolUsageLines

// RunNetwork lists local and remote agents and reads their current A2A
// cards, and finds, describes and calls remote MCP tools.
func RunNetwork(ctx context.Context, args []string, stdin io.Reader, out, errOut io.Writer) error {
	if len(args) > 0 && args[0] == "tool" {
		return RunTool(ctx, args[1:], stdin, out, errOut)
	}
	if len(args) < 2 || args[0] != "agent" || (args[1] != "list" && args[1] != "card") {
		if len(args) > 0 && (args[len(args)-1] == "-h" || args[len(args)-1] == "--help") {
			fmt.Fprintln(errOut, networkUsage)
			return nil
		}
		return errors.New(networkUsage)
	}
	card := args[1] == "card"
	fs := flag.NewFlagSet("network agent "+args[1], flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		if card {
			fmt.Fprintln(errOut, "usage: agentiscaled network agent card [flags] AGENT\nRead the current JSON card of a local or remote agent.")
		} else {
			fmt.Fprintln(errOut, "usage: agentiscaled network agent list [flags]\nList the agents hosted here and those discovered on other nodes.")
		}
		fs.PrintDefaults()
	}
	dir := fs.String("data-dir", nodestate.DefaultDataDir(), "Data directory of the running node")
	var peer string
	var asJSON bool
	if card {
		fs.StringVar(&peer, "peer", "", "Peer ID of the node that offers AGENT")
		fs.BoolVar(&asJSON, "json", false, "Print compact JSON (the default is indented JSON)")
	} else {
		fs.BoolVar(&asJSON, "json", false, "Print agents as JSON")
	}
	pos, err := cli.ParseInterleaved(fs, args[2:])
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if card && len(pos) != 1 || !card && len(pos) != 0 {
		return errors.New(networkUsage)
	}
	var name AgentName
	if card {
		if name, err = ParseAgentName(pos[0]); err != nil {
			return err
		}
	}
	if err := nodestate.CheckHalt(*dir); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := openClient(ctx, *dir)
	if err != nil {
		return err
	}
	if card {
		target, _, err := findAgent(ctx, client, name, peer, 0, false)
		if err != nil {
			return safeError(err)
		}
		data, err := client.Card(ctx, target)
		if err != nil {
			return safeError(err)
		}
		enc := json.NewEncoder(out)
		if !asJSON {
			enc.SetIndent("", "  ")
		}
		return enc.Encode(json.RawMessage(data))
	}
	list, err := listNetworkAgents(ctx, client, *dir)
	if err != nil {
		return safeError(err)
	}
	if asJSON {
		return json.NewEncoder(out).Encode(list)
	}
	if len(list.Agents) == 0 {
		_, err := fmt.Fprintln(out, "No agents found.")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "AGENT\tPEER\tRUNTIME\tDESCRIPTION"); err != nil {
		return err
	}
	for _, agent := range list.Agents {
		where := agent.PeerID
		if where == "" {
			where = "local"
		}
		description := strings.Join(strings.Fields(terminalSafe(agent.Description)), " ")
		if agent.Draining != nil {
			// The reason is a single line of printable text (Drain).
			description = strings.TrimSpace("(" + strings.TrimPrefix(terminalSafe(agent.Draining.Text()), "is ") + ") " + description)
		}
		// Runtime is a plain word (ValidRuntime); a node too old to say
		// shows "-".
		runtime := agent.Runtime
		if runtime == "" {
			runtime = "-"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", agent.Service, where, runtime, description); err != nil {
			return err
		}
	}
	return w.Flush()
}

func listNetworkAgents(ctx context.Context, client *Client, dir string) (AgentList, error) {
	info, err := client.LocalInfo(ctx)
	// A node hosting no agents has a mesh socket but no local agent socket.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return AgentList{}, fmt.Errorf("list this node's agents: %w", err)
	}
	providers, err := readDiscovery(ctx, client.Mesh, client.BaseURL, "")
	if err != nil {
		return AgentList{}, fmt.Errorf("discover the network's agents: %w", err)
	}
	return ListedAgents(info, "", providers), nil
}
