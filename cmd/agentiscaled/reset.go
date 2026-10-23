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
	"github.com/libp2p/go-libp2p/core/peer"
)

// runReset is `agentiscaled reset`: it gives the node a new identity in the
// signed-in owner's current network, for a node whose network is gone or no
// longer admits it. Only the node's key and enrollment, in its store, are
// replaced: the settings `up` saved, the agent and its own runtime home, the
// API token and the agent clients' entries, which name the data directory,
// all stay as they are.
func runReset(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node")
	var auth authConfig
	fs.StringVar(&auth.issuer, "auth-issuer", defaultAuth.issuer, "Auth0 tenant that signs you in")
	fs.StringVar(&auth.clientID, "auth-client-id", defaultAuth.clientID, "Auth0 application agentiscaled signs in with")
	fs.StringVar(&auth.audience, "auth-audience", defaultAuth.audience, "Auth0 API the sign-in is for")
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
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no node set up in %s: run 'agentiscaled up'", dir)
	} else if err != nil {
		return err
	}
	c := s.config(dir)
	if c.control == "" {
		c.control = defaultControl
	}

	// Signing in comes first: a sign-in that fails or is abandoned leaves
	// the node as it was.
	token, err := deviceLogin(ctx, auth, out)
	if err != nil {
		return err
	}
	c.signInToken = token

	svc := newService(dir)
	installed := svc != nil && svc.installed()
	if svc != nil {
		if err := svc.stop(); err != nil {
			return err
		}
	}
	aside, err := moveIdentityAside(dir)
	if err != nil {
		return err
	}
	network, err := enrollNew(ctx, c)
	if err != nil {
		return fmt.Errorf("join your network: %w\nThe node's previous identity is in %s; run 'agentiscaled reset' again", err, aside)
	}
	store, err := meshnode.NewStore(dir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	meshURL, _ := store.LoadControlPlaneURL()
	pid, err := peer.IDFromPrivateKey(meshnode.GetOrGenerateKey(store))
	store.Close()
	if err != nil {
		return err
	}
	s.PeerID, s.NetworkID, s.MeshURL = pid.String(), network, meshURL
	if err := saveSettings(dir, s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	if err := os.Remove(filepath.Join(dir, haltFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintf(out, "This node is now %s in network %s; its settings and agent clients are unchanged.\n", s.PeerID, s.NetworkID)
	if aside != "" {
		fmt.Fprintf(out, "Its previous identity is in %s.\n", aside)
	}

	exe, err := stableExecutable()
	if err != nil {
		return err
	}
	if !installed {
		fmt.Fprintln(out, "Run the node with:")
		fmt.Fprintf(out, "  %s run -data-dir %s\n", shellQuote(exe), shellQuote(dir))
		return nil
	}
	if err := svc.start(serviceSpec{exe: exe, dataDir: dir, path: s.Path}); err != nil {
		return fmt.Errorf("start the background service: %w", err)
	}
	fmt.Fprintf(out, "Started the background service %s again.\n", svc.name())
	if err := waitReady(ctx, dir, 30*time.Second); err != nil {
		fmt.Fprintf(out, "The node has not connected to the mesh yet (%v). See 'agentiscaled status' and %s.\n", err, filepath.Join(dir, logFile))
	} else {
		fmt.Fprintln(out, "Connected to the mesh.")
	}
	return nil
}

// enrollNew enrolls the node with its new key; tests stand in for the mesh.
var enrollNew = enrollOnly

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
