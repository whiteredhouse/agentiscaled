// Package nodeclient connects to the node's local Unix APIs.
package nodeclient

import (
	"context"
	"net"
	"net/http"
	"time"
)

// AgentSocketName is the local agent API socket in the node's data directory.
const AgentSocketName = "agent.sock"

// WaitForSocket waits until the node accepts connections on its socket, or
// the wait is over. A socket file left from before a restart does not count.
func WaitForSocket(ctx context.Context, socket string, wait time.Duration) error {
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

// SocketClient is an HTTP client that dials the node's Unix socket whatever
// the URL's host.
func SocketClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}
