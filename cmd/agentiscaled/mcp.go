package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	meshnode "github.com/google/agentmesh/export/node"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runMCP is `agentiscaled mcp`: it serves the running node's MCP tools on
// stdin and stdout, for agent clients that start local MCP servers as a
// command, such as Claude Desktop and the Codex CLI, and adds the
// tools that send A2A tasks to other nodes' agents. It reaches the
// node over its Unix socket, whose file permissions are the credential, so no
// API token goes into a client's configuration.
func runMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the running agentiscaled")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if h, err := loadHalt(*dataDir); err != nil {
		return err
	} else if h != nil {
		return errors.New(h.Message)
	}
	socket := filepath.Join(*dataDir, meshnode.DefaultSocketName)
	if err := waitForSocket(ctx, socket, nodeStartWait); err != nil {
		return fmt.Errorf("no running agentiscaled at %s: start it first (%w)", socket, err)
	}
	return bridgeMCP(ctx, &mcp.StdioTransport{}, socket)
}

// nodeStartWait is how long `agentiscaled mcp` waits for the node. At login
// the client and the node's background service start together, and the node
// opens its socket only once it has reached its network.
var nodeStartWait = 45 * time.Second

// waitForSocket waits until the node accepts connections on its socket, or
// the wait is over. A socket file left from before a restart does not count.
func waitForSocket(ctx context.Context, socket string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", socket)
		if err == nil {
			return conn.Close()
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// nodeA2AGuide starts the paragraph of the node's instructions that sends
// clients to the a2a CLI for A2A agents; the bridge's send_task replaces it.
const nodeA2AGuide = "A2A agents ('a2a://...')"

// withoutA2AGuide drops that paragraph from the node's instructions, so that a
// client is not told both to install the a2a CLI and not to use it.
func withoutA2AGuide(instructions string) string {
	paras := strings.Split(instructions, "\n\n")
	kept := paras[:0]
	for _, p := range paras {
		if !strings.HasPrefix(p, nodeA2AGuide) {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

// socketClient is an HTTP client that dials the node's Unix socket whatever
// the URL's host.
func socketClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

// bridgeMCP serves an MCP server on the client transport that offers every
// tool of the node's MCP server, reached over Streamable HTTP on its Unix
// socket, plus the A2A task tools, until the client closes.
func bridgeMCP(ctx context.Context, client mcp.Transport, socket string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpClient := socketClient(socket)
	// The host is never dialled; the socket is.
	node, err := mcp.NewClient(&mcp.Implementation{Name: "agentiscaled-mcp", Version: "0.1.0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://agentiscaled/mcp", HTTPClient: httpClient}, nil)
	if err != nil {
		return fmt.Errorf("connect to the node: %w", err)
	}
	defer node.Close()

	instructions := a2aInstructions
	if init := node.InitializeResult(); init != nil {
		instructions = withoutA2AGuide(init.Instructions) + "\n\n" + instructions
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "agentiscale", Version: "0.1.0"},
		&mcp.ServerOptions{Instructions: instructions})
	var cursor string
	for {
		page, err := node.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return fmt.Errorf("list the node's tools: %w", err)
		}
		for _, tool := range page.Tools {
			name := tool.Name
			server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return node.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: req.Params.Arguments})
			})
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	addA2ATools(server, &a2aTasks{http: httpClient, base: "http://agentiscaled"})

	// The client closing its end is the normal way to stop.
	if err := server.Run(ctx, client); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, mcp.ErrConnectionClosed) && ctx.Err() == nil {
		return err
	}
	return nil
}
