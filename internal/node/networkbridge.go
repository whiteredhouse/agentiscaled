package node

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// networkBridgeCommand is the hidden agentiscaled command that serves a run
// the network's agents on stdio (RunNetworkBridge).
const networkBridgeCommand = "network-bridge"

// RunNetworkBridge is `agentiscaled network-bridge -url URL -ticket-file
// FILE`: it serves one hosted run the network's other agents as an MCP
// server on stdio, for a runtime that starts MCP servers from a
// configuration file the agent itself can read (agy). The bridge reads a
// one-time ticket from FILE, which is not in any process list, removes the
// file and exchanges the ticket at the node for the run's bearer, which
// stays in the bridge's memory. Whoever redeems a ticket first spends it,
// and the node revokes one still unspent when the turn ends.
func RunNetworkBridge(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet(networkBridgeCommand, flag.ContinueOnError)
	endpoint := fs.String("url", "", "the run's network MCP endpoint on this node")
	ticketFile := fs.String("ticket-file", "", "the file that holds the run's one-time ticket, removed once read")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if err := loopbackEndpoint(*endpoint); err != nil {
		return err
	}
	if *ticketFile == "" {
		return errors.New("-ticket-file is required")
	}
	b, err := os.ReadFile(*ticketFile)
	if err != nil {
		return fmt.Errorf("read the ticket: %w", err)
	}
	os.Remove(*ticketFile)
	ticket := strings.TrimSpace(string(b))
	if ticket == "" {
		return errors.New("the ticket file is empty")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	client := &http.Client{Timeout: 10 * time.Second, Transport: loopbackTransport(), CheckRedirect: noRedirect}
	bearer, err := redeemTicket(ctx, client, ticketURL(*endpoint), ticket)
	if err != nil {
		return err
	}
	return bridgeNetwork(ctx, &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}}, *endpoint, bearer)
}

// loopbackEndpoint checks that endpoint is a node's network endpoint, on a
// loopback address, so that a ticket never leaves the machine.
func loopbackEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Path != "/mcp" {
		return fmt.Errorf("-url %q is not a node's network endpoint", endpoint)
	}
	host, _, err := net.SplitHostPort(u.Host)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("-url %q is not on a loopback address", endpoint)
	}
	return nil
}

// redeemTicket exchanges a ticket for its run's bearer.
func redeemTicket(ctx context.Context, client *http.Client, endpoint, ticket string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(url.Values{"ticket": {ticket}}.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach the node: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the node refused the ticket (%s): it is spent, expired, or its task has ended", resp.Status)
	}
	bearer := strings.TrimSpace(string(b))
	if bearer == "" {
		return "", errors.New("the node sent no credential for the ticket")
	}
	return bearer, nil
}

// bridgeNetwork serves on the client transport every tool of the run's
// network server at endpoint, reached with bearer, under the same name and
// instructions, until the client closes.
func bridgeNetwork(ctx context.Context, client mcp.Transport, endpoint, bearer string) error {
	httpClient := &http.Client{Transport: bearerTransport{bearer: bearer, next: loopbackTransport()}, CheckRedirect: noRedirect}
	node, err := mcp.NewClient(&mcp.Implementation{Name: "agentiscaled-network-bridge", Version: "0.1.0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient}, nil)
	if err != nil {
		return fmt.Errorf("connect to the node: %w", err)
	}
	defer node.Close()
	var instructions string
	if init := node.InitializeResult(); init != nil {
		instructions = init.Instructions
	}
	server := mcp.NewServer(&mcp.Implementation{Name: networkServerName, Version: "0.1.0"},
		&mcp.ServerOptions{Instructions: instructions})
	var cursor string
	for {
		page, err := node.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return fmt.Errorf("list the network's tools: %w", err)
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
	// The runtime closing its end is the normal way to stop.
	if err := server.Run(ctx, client); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, mcp.ErrConnectionClosed) && ctx.Err() == nil {
		return err
	}
	return nil
}

// bearerTransport adds the run's bearer to each request to the node.
type bearerTransport struct {
	bearer string
	next   http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.bearer)
	return t.next.RoundTrip(r)
}

// noRedirect keeps the ticket and the bearer to the endpoint the bridge
// was given: it follows no redirect.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// loopbackTransport reaches the node directly, never through a proxy.
func loopbackTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return t
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// bridgeExecutable is the agentiscaled executable that a runtime starts as
// the network bridge.
var bridgeExecutable = os.Executable
