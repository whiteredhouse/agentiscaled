// Package nodeclient connects to the node's local Unix APIs.
package nodeclient

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"time"
)

// AgentSocketName is the local agent API socket in the node's data directory.
const AgentSocketName = "agent.sock"

// ErrSocketDenied reports that this process may not connect to the node's
// socket, typically because a client's sandbox blocks Unix sockets. Waiting
// does not change that, so WaitForSocket returns it at once.
var ErrSocketDenied = errors.New("permission denied")

// WaitForSocket waits until the node accepts connections on its socket, or
// the wait is over. A socket file left from before a restart does not count.
// A connection refused for lack of permission ends the wait at once with an
// error that wraps ErrSocketDenied.
func WaitForSocket(ctx context.Context, socket string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", socket)
		if err == nil {
			return conn.Close()
		}
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("%w: %w", ErrSocketDenied, err)
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
