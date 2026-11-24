package caller

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	meshnode "github.com/google/agentmesh/export/node"
)

// A socket the process may not use sends the task and network commands to
// the MCP server, but the MCP server itself reports only its own failure.
func TestDeniedSocketFallbackIsForTheCommandsOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root connects to any socket")
	}
	dir, err := os.MkdirTemp("", "ais")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, meshnode.DefaultSocketName)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	if err := os.Chmod(socket, 0); err != nil {
		t.Fatal(err)
	}

	_, err = openClient(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "permission denied") || !strings.Contains(err.Error(), "use the agentiscale MCP server") {
		t.Fatalf("command: %v", err)
	}
	err = RunMCP(context.Background(), []string{"-data-dir", dir})
	if err == nil || !strings.Contains(err.Error(), "permission denied") || strings.Contains(err.Error(), "MCP server") {
		t.Fatalf("MCP server: %v", err)
	}
}
