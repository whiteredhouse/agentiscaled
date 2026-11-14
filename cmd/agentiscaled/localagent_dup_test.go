package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
)

func TestWarnDuplicateAgent(t *testing.T) {
	old := duplicateAgentChecks
	duplicateAgentChecks = []time.Duration{0, 0}
	t.Cleanup(func() { duplicateAgentChecks = old })
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	calls := 0
	warnDuplicateAgent(context.Background(), "self", "codex", func(context.Context) ([]*api.DiscoveredProvider, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("mesh not ready")
		}
		return []*api.DiscoveredProvider{
			{PeerId: "self", SrvName: "codex"},
			{PeerId: "other", SrvName: "codex"},
			{PeerId: "third", SrvName: "helper"},
		}, nil
	})
	if calls != 2 {
		t.Fatalf("discover called %d times, want 2", calls)
	}
	out := buf.String()
	if !strings.Contains(out, "also serves a2a://codex (peer other)") || strings.Contains(out, "third") {
		t.Fatalf("log = %q", out)
	}

	buf.Reset()
	warnDuplicateAgent(context.Background(), "self", "codex", func(context.Context) ([]*api.DiscoveredProvider, error) {
		return []*api.DiscoveredProvider{{PeerId: "self", SrvName: "codex"}}, nil
	})
	if buf.Len() != 0 {
		t.Fatalf("warned without a duplicate: %q", buf.String())
	}
}

// A local socket that accepts but never answers must not hold up a call to
// another node.
func TestRemoteCallSkipsStuckLocalSocket(t *testing.T) {
	old := localInfoTimeout
	localInfoTimeout = 100 * time.Millisecond
	t.Cleanup(func() { localInfoTimeout = old })
	dir := shortTempDir(t)
	socket := filepath.Join(dir, localAgentSocketName)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold the connection open without answering
		}
	}()
	mesh := &http.Client{Transport: &http.Transport{}}
	tasks := &a2aTasks{http: mesh, base: "http://agentiscaled", local: socketClient(socket)}
	remote := "12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN"
	start := time.Now()
	endpoint, client, err := tasks.route(context.Background(), taskTarget{PeerID: remote, Service: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("route took %s", time.Since(start))
	}
	if client.Transport != mesh.Transport || !strings.Contains(endpoint, "/mesh/"+remote+"/a2a/codex") {
		t.Fatalf("route = %s, want the mesh route", endpoint)
	}
}
