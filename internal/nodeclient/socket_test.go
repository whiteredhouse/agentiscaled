package nodeclient

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A socket this process may not connect to, as under a client's sandbox,
// ends the wait at once instead of looking like a node that is not running.
func TestWaitForSocketStopsAtPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root connects to any socket")
	}
	dir, err := os.MkdirTemp("", "ais")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "node.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	if err := os.Chmod(socket, 0); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = WaitForSocket(context.Background(), socket, 5*time.Second)
	if !errors.Is(err, ErrSocketDenied) {
		t.Fatalf("got %v, want ErrSocketDenied", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("waited %v for a socket it may not use", time.Since(start))
	}
}
