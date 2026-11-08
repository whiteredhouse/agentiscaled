package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	meshnode "github.com/google/agentmesh/export/node"
)

// runReset is `agentiscaled reset`: it drops the node's identity, for a node
// whose network is gone or no longer admits it, so that `up` joins it again
// as a new node, by sign-in or with a join code. It stops and removes the
// background service, moves the node's store (its key and enrollment) aside
// and clears the recorded stop. It does not sign in or join. The settings
// `up` saved, the agent and its own runtime home, the API token and the agent
// clients' entries, which name the data directory, all stay as they are.
func runReset(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	dir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	s, err := loadSettings(dir)
	savedSettings := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !savedSettings {
		// A node run directly with flags has a store but no saved settings.
		if _, err := os.Stat(filepath.Join(dir, meshnode.StoreFile)); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no node set up in %s: run 'agentiscaled up'", dir)
		} else if err != nil {
			return err
		}
	}

	svc := newService(dir)
	if svc != nil && svc.installed() {
		// Removed, not only stopped: a service left installed would start
		// a node with no identity at the next login.
		if err := svc.remove(); err != nil {
			return err
		}
		fmt.Fprintf(out, "Stopped and removed %s.\n", svc.name())
	}
	aside, err := moveIdentityAside(dir)
	if err != nil {
		return err
	}
	if savedSettings {
		s.PeerID, s.NetworkID, s.MeshURL = "", "", ""
		if err := saveSettings(dir, s); err != nil {
			return fmt.Errorf("save settings: %w", err)
		}
	}
	if err := os.Remove(filepath.Join(dir, haltFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if aside != "" {
		fmt.Fprintf(out, "The node's identity is reset; its previous identity is in %s.\n", aside)
	} else {
		fmt.Fprintln(out, "The node had no identity; nothing to move aside.")
	}
	fmt.Fprint(out, rejoinSteps(dir, savedSettings))
	return nil
}

// rejoinSteps tells the owner how to join a reset node again. `up` keeps
// the node's saved settings, its choice of agent clients and of running in
// the foreground included. A node run directly with flags has none, so it
// is restarted with them.
func rejoinSteps(dir string, savedSettings bool) string {
	up := upCommand(dir)
	if !savedSettings {
		return fmt.Sprintf("To join your network as a new node, restart the foreground node with its original flags and -data-dir %s, adding -join-code-path <file>, or run '%s -foreground -clients none' to join by signing in.\n", shellQuote(dir), up)
	}
	return fmt.Sprintf("Its settings and agent clients are unchanged. To join your network as a new node, run:\n  %[1]s\nor, on a machine where nobody can sign in:\n  %[1]s -join-code-path <file>\n", up)
}

// moveIdentityAside renames the node's store, which holds only its key and
// its enrollment (credential, trusted keys, routers, mesh URL), so that the
// node enrolls anew, and returns where it went. Nothing else in the data
// directory moves. A store held by a running node is refused.
func moveIdentityAside(dataDir string) (string, error) {
	path := filepath.Join(dataDir, meshnode.StoreFile)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	store, err := meshnode.NewStore(dataDir)
	if err != nil && strings.Contains(err.Error(), "file lock") {
		return "", fmt.Errorf("another agentiscaled is using %s; stop it first", dataDir)
	} else if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	store.Close()
	aside := path + ".replaced-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(path, aside); err != nil {
		return "", err
	}
	return aside, nil
}
