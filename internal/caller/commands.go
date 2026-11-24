package caller

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	meshnode "github.com/google/agentmesh/export/node"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/whiteredhouse/agentiscaled/internal/nodeclient"
	"github.com/whiteredhouse/agentiscaled/internal/nodestate"
)

const DefaultTaskDeadline = 15 * time.Minute

const MaxProgressLine = 200

// ProgressLine normalizes one progress message for display or transmission.
func ProgressLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= MaxProgressLine {
		return s
	}
	r := []rune(s)
	return string(r[:MaxProgressLine-1]) + "…"
}

// The client and node service can start together at login.
const nodeStartWait = 45 * time.Second

func readySocket(ctx context.Context, dataDir string) (string, error) {
	socket := filepath.Join(dataDir, meshnode.DefaultSocketName)
	if err := nodeclient.WaitForSocket(ctx, socket, nodeStartWait); err != nil {
		return "", fmt.Errorf("no running agentiscaled at %s: start it first (%w)", socket, err)
	}
	return socket, nil
}

func openClient(ctx context.Context, dataDir string) (*Client, error) {
	socket, err := readySocket(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	return &Client{Mesh: nodeclient.SocketClient(socket), BaseURL: "http://agentiscaled", Local: nodeclient.SocketClient(filepath.Join(dataDir, nodeclient.AgentSocketName))}, nil
}

// RunMCP serves the owner's MCP bridge on standard input and output.
func RunMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	dir := fs.String("data-dir", nodestate.DefaultDataDir(), "Data directory of the running agentiscaled")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := nodestate.CheckHalt(*dir); err != nil {
		return err
	}
	socket, err := readySocket(ctx, *dir)
	if err != nil {
		return err
	}
	return BridgeMCP(ctx, &mcp.StdioTransport{}, socket)
}
