package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// accountConfirmation is the word the owner types to delete their account.
const accountConfirmation = "delete"

// runAccount is `agentiscaled account`.
func runAccount(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 || args[0] != "delete" {
		fmt.Fprint(out, "Usage: agentiscaled account delete [-yes] [-data-dir dir]\n\n"+
			"  delete   delete your agentiscale account and network, for good\n")
		if len(args) == 0 {
			return errors.New("missing account command")
		}
		return fmt.Errorf("unknown account command %q", args[0])
	}
	return runAccountDelete(ctx, args[1:], in, out)
}

// runAccountDelete is `agentiscaled account delete`: after the owner
// confirms and signs in, it asks the control service to delete their account
// and network, then stops this machine's background node. Other machines'
// nodes stop on their own once their network is gone.
func runAccountDelete(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("account delete", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of this machine's node")
	control := fs.String("control", "", "Control service URL (default: the one this node uses, or the hosted control service)")
	yes := fs.Bool("yes", false, "Don't ask for confirmation")
	var auth authConfig
	fs.StringVar(&auth.issuer, "auth-issuer", defaultAuth.issuer, "OIDC issuer that signs you in")
	fs.StringVar(&auth.clientID, "auth-client-id", defaultAuth.clientID, "Public client ID agentiscaled signs in with")
	fs.StringVar(&auth.audience, "auth-audience", defaultAuth.audience, "API audience the sign-in is for")
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
	url := *control
	if url == "" {
		if s, err := loadSettings(dir); err == nil && s.Control != "" {
			url = s.Control
		}
	}
	if url == "" {
		url = defaultControl
	}

	fmt.Fprint(out, `This deletes your agentiscale account and your network, for good:
every node is removed from the network and disconnected, every join code
stops working, and your agents can no longer reach each other through
agentiscale. Signing in again later starts a new, empty account.
`)
	if !*yes {
		fmt.Fprintf(out, "Type %q to delete your account: ", accountConfirmation)
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if strings.ToLower(strings.TrimSpace(line)) != accountConfirmation {
			return errors.New("not confirmed; nothing was deleted")
		}
	}
	token, err := deviceLogin(ctx, auth, out)
	if err != nil {
		return err
	}
	networks, err := deleteAccount(ctx, url, token)
	if err != nil {
		return err
	}
	for _, n := range networks {
		fmt.Fprintf(out, "Deleted network %s.\n", n)
	}
	fmt.Fprintln(out, "Your account is deleted. Its network's mesh is removed within a few minutes.")

	if svc := newService(dir); svc != nil && svc.installed() {
		if err := svc.remove(); err != nil {
			fmt.Fprintf(out, "Could not stop the background service %s: %v. Stop it with 'agentiscaled down'.\n", svc.name(), err)
		} else {
			fmt.Fprintf(out, "Stopped and removed the background service %s.\n", svc.name())
		}
	}
	if _, err := os.Stat(dir); err == nil {
		fmt.Fprintf(out, "This machine's node data is still in %s; delete that directory to remove it.\n", dir)
	}
	fmt.Fprintln(out, "Nodes on your other machines stop once they notice the network is gone; uninstall agentiscaled there to remove their data.")
	return nil
}

// deleteAccount asks the control service to delete the signed-in account
// and returns the IDs of the networks it deleted.
func deleteAccount(ctx context.Context, control, token string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimSuffix(control, "/")+"/v1/me", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("delete account: %w", err)
	}
	defer resp.Body.Close()
	var answer struct {
		Status     string   `json:"status"`
		NetworkIDs []string `json:"network_ids"`
		Error      string   `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&answer)
	switch {
	case resp.StatusCode == http.StatusAccepted && answer.Status == "deleting":
		return answer.NetworkIDs, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, errors.New("delete account: you have no account to delete")
	}
	return nil, fmt.Errorf("delete account: %s (%s)", answer.Error, resp.Status)
}
