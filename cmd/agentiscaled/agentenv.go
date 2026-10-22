package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"
)

// agentEnvFile, in the node's data directory, holds variables for an agent
// that runs with the owner's configuration, one NAME=value per line, for
// what the owner's login shell does not set. Only the owner may read it.
const agentEnvFile = "agent.env"

// loginShellTimeout bounds reading the owner's login shell, whose startup
// files may be slow or wait for input.
var loginShellTimeout = 10 * time.Second

// ownerEnv gives an agent that runs with the owner's configuration the
// environment their terminal has: the background service starts with almost
// none, so the variables of the owner's login shell that this process lacks
// are added, as an editor started from the Dock does, and then agent.env.
// Neither is ever written by agentiscaled.
func ownerEnv(ctx context.Context, dataDir string) error {
	shellEnv, err := loginShellEnv(ctx)
	if err != nil {
		log.Printf("agent environment: your login shell's variables are not used: %v", err)
	}
	added := 0
	for _, kv := range shellEnv {
		name, value, _ := strings.Cut(kv, "=")
		if _, set := os.LookupEnv(name); !set && name != "" {
			if err := os.Setenv(name, value); err != nil {
				return err
			}
			added++
		}
	}
	fileEnv, err := readAgentEnvFile(filepath.Join(dataDir, agentEnvFile))
	if err != nil {
		return err
	}
	for _, kv := range fileEnv {
		name, value, _ := strings.Cut(kv, "=")
		if err := os.Setenv(name, value); err != nil {
			return err
		}
	}
	log.Printf("agent environment: %d variables from your login shell, %d from %s", added, len(fileEnv), agentEnvFile)
	return nil
}

const envMarker = "__AGENTISCALE_ENV__"

// loginShellEnv runs the owner's shell as a login, interactive shell, which
// reads the same startup files as their terminal, and returns its
// environment. Markers set it apart from anything the startup files print.
func loginShellEnv(ctx context.Context) ([]string, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
		if goruntime.GOOS == "darwin" {
			shell = "/bin/zsh"
		}
	}
	ctx, cancel := context.WithTimeout(ctx, loginShellTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-l", "-i", "-c", "printf '"+envMarker+"'; command env -0; printf '"+envMarker+"'")
	cmd.Stdin = nil // no terminal: a startup file that asks for input reads EOF
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s did not finish within %s", shell, loginShellTimeout)
	}
	parts := strings.Split(out.String(), envMarker)
	if len(parts) < 3 {
		if err != nil {
			return nil, fmt.Errorf("%s: %w", shell, err)
		}
		return nil, fmt.Errorf("%s printed no environment", shell)
	}
	var env []string
	for _, kv := range strings.Split(parts[len(parts)-2], "\x00") {
		if strings.Contains(kv, "=") {
			env = append(env, kv)
		}
	}
	return env, nil
}

// readAgentEnvFile reads agent.env, if there is one: NAME=value lines, with
// blank lines and lines starting with # left out. A file others can read is
// refused, since it may hold tokens.
func readAgentEnvFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s can be read by others (mode %v): run chmod 600 %s", path, fi.Mode().Perm(), shellQuote(path))
	}
	var env []string
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, ok := strings.Cut(line, "=")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("%s line %d: want NAME=value", path, n)
		}
		env = append(env, line)
	}
	return env, sc.Err()
}
