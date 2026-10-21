// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/agentmesh/internal/node"
)

const (
	daemonPIDFile      = "agentmesh-node.pid"
	daemonLogFile      = "agentmesh-node.log"
	daemonTokenFile    = "api-token"
	daemonReadyTimeout = 90 * time.Second
	daemonPollInterval = 200 * time.Millisecond
)

// daemonizeRun re-executes this binary as a detached background node and
// returns once its local API answers, so a single non-blocking command is
// enough to bring the mesh up.
func daemonizeRun(socketPath string) error {
	dataDir := resolveDataDir()
	probe := probeTarget{addr: localProbeAddr(bindAddrFlag), socketPath: socketPath}

	if probe.ready(time.Second) {
		fmt.Printf("agentmesh-node is already running and listening on %s\n", probe)
		return nil
	}

	enrolled, controlPlane, err := storedEnrollment(dataDir)
	if errors.Is(err, node.ErrStoreLocked) {
		fmt.Printf("agentmesh-node is already running%s: %s is in use.\n", runningPIDSuffix(dataDir), dataDir)
		return nil
	}
	if err != nil {
		return err
	}
	if !enrolled && bootstrapTokenFlag == "" && bootstrapTokenPathFlag == "" && jwtFlag == "" && jwtPathFlag == "" {
		target := controlPlane
		if target == "" {
			target = "<control-plane-url>"
		}
		return fmt.Errorf("this node is not enrolled yet, and enrolling needs a one-time login you have to approve:\n"+
			"  agentmesh-node join --headless %s\n"+
			"then re-run 'agentmesh-node run --daemonize'", target)
	}

	tokenArgs, tokenPath, err := ensureDaemonToken(dataDir)
	if err != nil {
		return err
	}
	secretArgs, err := migrateSecretFlagsToFiles(dataDir)
	if err != nil {
		return err
	}
	tokenArgs = append(tokenArgs, secretArgs...)

	logPath := filepath.Join(dataDir, daemonLogFile)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", logPath, err)
	}
	defer func() {
		_ = logFile.Close()
	}()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the agentmesh-node binary: %w", err)
	}

	child := exec.Command(exe, append(withoutDaemonizeFlag(os.Args[1:]), tokenArgs...)...) // #nosec G204 -- re-executes this same binary with this process's own arguments
	child.Stdout = logFile
	child.Stderr = logFile
	child.SysProcAttr = detachedProcAttr()
	if err := child.Start(); err != nil {
		return fmt.Errorf("starting the background node: %w", err)
	}

	pidPath := filepath.Join(dataDir, daemonPIDFile)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(child.Process.Pid)), 0644); err != nil {
		logger.Warnf("Failed to write %s: %v", pidPath, err)
	}

	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	if err := waitForDaemon(probe, exited); err != nil {
		return fmt.Errorf("%w\nLast lines of %s:\n%s", err, logPath, tailFile(logPath, 8192, 10))
	}

	printDaemonSummary(child.Process.Pid, probe, tokenPath, logPath)
	return nil
}

// storedEnrollment reports whether this data directory already holds a mesh
// identity, along with the control plane to enroll with if it does not.
func storedEnrollment(dataDir string) (bool, string, error) {
	store, err := node.NewStore(dataDir)
	if err != nil {
		return false, "", fmt.Errorf("opening the node store: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Errorf("closing store: %v", err)
		}
	}()
	identity, _ := store.LoadIdentity()
	return len(identity) > 0, defaultControlPlane(store, controlPlaneAddr), nil
}

// ensureDaemonToken keeps the background node authenticated without the user
// inventing a secret: it reuses an explicitly configured token, or generates
// one under the data directory and passes it to the child by path.
func ensureDaemonToken(dataDir string) ([]string, string, error) {
	if apiTokenPathFlag != "" {
		return nil, apiTokenPathFlag, nil
	}
	if os.Getenv("AGENTMESH_API_TOKEN") != "" {
		return nil, "", nil
	}
	tokenPath := filepath.Join(dataDir, daemonTokenFile)
	if _, err := os.Stat(tokenPath); err == nil {
		return []string{"--api-token-path", tokenPath}, tokenPath, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, "", fmt.Errorf("generating an API token: %w", err)
	}
	f, err := os.OpenFile(tokenPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, "", fmt.Errorf("creating %s: %w", tokenPath, err)
	}
	defer func() {
		_ = f.Close()
	}()
	if _, err := io.WriteString(f, hex.EncodeToString(buf)); err != nil {
		return nil, "", fmt.Errorf("writing %s: %w", tokenPath, err)
	}
	return []string{"--api-token-path", tokenPath}, tokenPath, nil
}

// waitForDaemon blocks until the background node answers, it exits, or the
// startup budget runs out.
func waitForDaemon(probe probeTarget, exited <-chan error) error {
	deadline := time.After(daemonReadyTimeout)
	for {
		if probe.ready(daemonPollInterval) {
			return nil
		}
		select {
		case err := <-exited:
			return fmt.Errorf("the background node exited during startup (%v)", err)
		case <-deadline:
			return fmt.Errorf("the background node did not answer on %s within %s", probe, daemonReadyTimeout)
		case <-time.After(daemonPollInterval):
		}
	}
}

// probeTarget is where a freshly started node is expected to answer. A node
// can serve on a TCP address, on a Unix socket, or on both, so either endpoint
// answering means it is up.
type probeTarget struct {
	addr       string
	socketPath string
}

func (p probeTarget) String() string {
	if p.addr == "" {
		return p.socketPath
	}
	return p.addr
}

// ready reports whether the node's local API is answering.
func (p probeTarget) ready(timeout time.Duration) bool {
	if p.addr != "" && probeReady(p.addr, timeout) {
		return true
	}
	return p.socketPath != "" && socketProbeReady(p.socketPath, timeout)
}

// probeReady reports whether the node's local API is answering on addr.
func probeReady(addr string, timeout time.Duration) bool {
	if tlsCertFlag != "" {
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// socketProbeReady reports whether the node's local API is answering on its
// Unix socket. The host in the URL is ignored once the dialer targets a socket.
func socketProbeReady(path string, timeout time.Duration) bool {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", path)
			},
		},
	}
	resp, err := client.Get("http://localhost/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// localProbeAddr turns a listen address into one that can be dialed from this
// machine, since a wildcard bind is not itself a destination.
func localProbeAddr(bindAddr string) string {
	host, port, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return bindAddr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// runningPIDSuffix renders the pid of a previously daemonized node, when the
// data directory still records one, for use inside a status message.
func runningPIDSuffix(dataDir string) string {
	data, err := os.ReadFile(filepath.Join(dataDir, daemonPIDFile))
	if err != nil {
		return ""
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return ""
	}
	return fmt.Sprintf(" (pid %d)", pid)
}

// confirmPurge gates the destructive reset: the identity it deletes can only
// be recreated by enrolling again, so it is never implicit.
func confirmPurge(dataDir string) error {
	if assumeYesFlag {
		return nil
	}
	if !isInteractiveTerminal() {
		return fmt.Errorf("refusing to delete the node state in %s without confirmation: re-run with --yes", dataDir)
	}
	fmt.Printf("Delete all node state in %s, including the key behind this node's PeerID? [y/N]: ", dataDir)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading the confirmation: %w", err)
	}
	if !isYesResponse(answer) {
		return fmt.Errorf("aborted: nothing was deleted")
	}
	return nil
}

// nodeStateFiles are the files a node creates in its data directory. Reset
// removes exactly these and never the directory itself, since --data-dir can
// point at a directory that holds unrelated files.
func nodeStateFiles() []string {
	return []string{node.StoreFile, daemonTokenFile, daemonPIDFile, daemonLogFile}
}

// purgeDataDir deletes all local node state, so the next start behaves like a
// first start instead of inheriting a half-configured one.
func purgeDataDir(dataDir string) ([]string, error) {
	if err := ensureNodeStopped(dataDir); err != nil {
		return nil, err
	}
	var removed []string
	for _, name := range nodeStateFiles() {
		path := filepath.Join(dataDir, name)
		switch err := os.Remove(path); {
		case err == nil:
			removed = append(removed, path)
		case os.IsNotExist(err):
		default:
			return removed, fmt.Errorf("removing %s: %w", path, err)
		}
	}
	return removed, nil
}

// ensureNodeStopped refuses to touch state that a running node still owns.
func ensureNodeStopped(dataDir string) error {
	// No database, no lock to take, and nothing to recreate by looking.
	if _, err := os.Stat(filepath.Join(dataDir, node.StoreFile)); os.IsNotExist(err) {
		return nil
	}
	store, err := node.NewStore(dataDir)
	if errors.Is(err, node.ErrStoreLocked) {
		return fmt.Errorf("a agentmesh-node is still running%s: stop it before resetting %s", runningPIDSuffix(dataDir), dataDir)
	}
	if err != nil {
		// Any other failure, such as a corrupt database, is itself a reason to reset.
		return nil
	}
	return store.Close()
}

// migrateSecretFlagsToFiles persists any deprecated inline --bootstrap-token
// or --jwt flag value to a 0600 file inside dataDir and returns the
// corresponding --*-path flags so secrets never sit in the daemon's cmdline.
func migrateSecretFlagsToFiles(dataDir string) ([]string, error) {
	var extra []string
	if bootstrapTokenFlag != "" && bootstrapTokenPathFlag == "" {
		p := filepath.Join(dataDir, "bootstrap-token")
		if err := os.WriteFile(p, []byte(bootstrapTokenFlag), 0600); err != nil {
			return nil, fmt.Errorf("writing %s: %w", p, err)
		}
		extra = append(extra, "--bootstrap-token-path", p)
	}
	if jwtFlag != "" && jwtPathFlag == "" {
		p := filepath.Join(dataDir, "enrollment-jwt")
		if err := os.WriteFile(p, []byte(jwtFlag), 0600); err != nil {
			return nil, fmt.Errorf("writing %s: %w", p, err)
		}
		extra = append(extra, "--jwt-path", p)
	}
	return extra, nil
}

// withoutDaemonizeFlag drops --daemonize and any inline secret flags
// (--bootstrap-token, --jwt) so the child runs in the foreground without
// exposing credentials in ps output.
func withoutDaemonizeFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--daemonize" || strings.HasPrefix(arg, "--daemonize=") {
			continue
		}
		if arg == "--bootstrap-token" || arg == "--jwt" {
			i++ // skip value
			continue
		}
		if strings.HasPrefix(arg, "--bootstrap-token=") || strings.HasPrefix(arg, "--jwt=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// tailFile returns at most the last maxLines lines of path, read from its last
// maxBytes, for reporting a failed startup.
func tailFile(path string, maxBytes int64, maxLines int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() {
		_ = f.Close()
	}()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	if info.Size() > maxBytes {
		if _, err := f.Seek(-maxBytes, io.SeekEnd); err != nil {
			return ""
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func printDaemonSummary(pid int, probe probeTarget, tokenPath, logPath string) {
	fmt.Printf("agentmesh-node is running in the background.\n")
	fmt.Printf("  PID       %d\n", pid)
	if probe.addr != "" {
		fmt.Printf("  Endpoint  http://%s/mcp\n", probe.addr)
	}
	if probe.socketPath != "" {
		fmt.Printf("  Socket    %s\n", probe.socketPath)
	}
	if tokenPath != "" {
		fmt.Printf("  Token     %s\n", tokenPath)
	}
	fmt.Printf("  Logs      %s\n", logPath)
	fmt.Printf("  Stop      kill %d\n", pid)
	if probe.socketPath != "" {
		fmt.Printf("\nCall it over the socket without a token, e.g.\n  curl --unix-socket %s http://localhost/healthz\n", probe.socketPath)
	}
	if probe.addr != "" {
		fmt.Printf("\nOver TCP, authenticate with the header \"X-Mesh-Authentication: Bearer <token>\".\n")
	}
}
