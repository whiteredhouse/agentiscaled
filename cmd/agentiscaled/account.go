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
	"time"
)

// accountConfirmation is the word the owner types to delete their account.
const accountConfirmation = "delete"

// runAccount is `agentiscaled account`.
func runAccount(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "delete":
			return runAccountDelete(ctx, args[1:], in, out)
		case "restore":
			return runAccountRestore(ctx, args[1:], out)
		}
	}
	fmt.Fprint(out, "Usage: agentiscaled account delete [-yes] [-data-dir dir]\n"+
		"       agentiscaled account restore [-data-dir dir]\n\n"+
		"  delete   delete your agentiscale account and network (you can restore them for 30 days)\n"+
		"  restore  undo a deletion within those 30 days\n")
	if len(args) == 0 {
		return errors.New("missing account command")
	}
	return fmt.Errorf("unknown account command %q", args[0])
}

// accountFlags are the flags of the account commands.
type accountFlags struct {
	dataDir, control string
	auth             authConfig
}

func (f *accountFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.dataDir, "data-dir", defaultDataDir(), "Data directory of this machine's node")
	fs.StringVar(&f.control, "control", "", "Control service URL (default: the one this node uses, or the hosted control service)")
	fs.StringVar(&f.auth.issuer, "auth-issuer", defaultAuth.issuer, "OIDC issuer that signs you in")
	fs.StringVar(&f.auth.clientID, "auth-client-id", defaultAuth.clientID, "Public client ID agentiscaled signs in with")
	fs.StringVar(&f.auth.audience, "auth-audience", defaultAuth.audience, "API audience the sign-in is for")
}

// resolve returns the node's data directory and the control service URL:
// the flag's, else the one the node saved, else the hosted one.
func (f *accountFlags) resolve(fs *flag.FlagSet) (dir, control string, err error) {
	if fs.NArg() > 0 {
		return "", "", fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if dir, err = filepath.Abs(f.dataDir); err != nil {
		return "", "", err
	}
	control = f.control
	if control == "" {
		if s, err := loadSettings(dir); err == nil && s.Control != "" {
			control = s.Control
		}
	}
	if control == "" {
		control = defaultControl
	}
	return dir, control, nil
}

// runAccountDelete is `agentiscaled account delete`: after the owner
// confirms and signs in, it asks the control service to delete their account
// and network, then stops this machine's background node. The control
// service removes every node and join code at once and purges the account
// after a grace period, during which the owner can restore it.
func runAccountDelete(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("account delete", flag.ContinueOnError)
	var f accountFlags
	f.register(fs)
	yes := fs.Bool("yes", false, "Don't ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, url, err := f.resolve(fs)
	if err != nil {
		return err
	}

	fmt.Fprint(out, `This deletes your agentiscale account and your network. Every node is
removed from the network and disconnected now, every join code stops
working, and your agents can no longer reach each other through
agentiscale. You can restore the account for 30 days; after that it is
deleted for good, and signing in again starts a new, empty account.
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
	token, err := deviceLogin(ctx, f.auth, out)
	if err != nil {
		return err
	}
	deletion, err := deleteAccount(ctx, url, token)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Your account and network will be deleted for good on %s.\n", deletion.PurgeAfter.Local().Format("2 January 2006"))
	fmt.Fprintln(out, "Until then, undo it with 'agentiscaled account restore' or in the console.")

	if svc := newService(dir); svc != nil && svc.installed() {
		if err := svc.stop(); err != nil {
			fmt.Fprintf(out, "Could not stop the background service %s: %v. Stop it with 'agentiscaled down'.\n", svc.name(), err)
		} else {
			fmt.Fprintf(out, "Stopped the background service %s.\n", svc.name())
		}
	}
	fmt.Fprintln(out, "Nodes on your other machines stop once they notice they were removed.")
	if _, err := os.Stat(dir); err == nil {
		fmt.Fprintf(out, "Once the account is gone for good, uninstall agentiscaled and delete %s to remove this machine's node data.\n", dir)
	}
	return nil
}

// runAccountRestore is `agentiscaled account restore`: it undoes a
// scheduled deletion of the signed-in account. The network's nodes stay
// removed, so each machine joins again with `agentiscaled reset`.
func runAccountRestore(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("account restore", flag.ContinueOnError)
	var f accountFlags
	f.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, url, err := f.resolve(fs)
	if err != nil {
		return err
	}
	token, err := deviceLogin(ctx, f.auth, out)
	if err != nil {
		return err
	}
	name, err := restoreAccount(ctx, url, token)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Restored your account and %s.\n", name)
	fmt.Fprintln(out, "Its machines were removed when you deleted it: run 'agentiscaled reset' on each to join it again.")
	return nil
}

// scheduledDeletion is the control service's answer to DELETE /v1/me.
type scheduledDeletion struct {
	Status     string    `json:"status"`
	NetworkIDs []string  `json:"network_ids"`
	PurgeAfter time.Time `json:"purge_after"`
}

// deleteAccount asks the control service to delete the signed-in account
// and returns when it will be purged.
func deleteAccount(ctx context.Context, control, token string) (scheduledDeletion, error) {
	var answer struct {
		scheduledDeletion
		Error string `json:"error"`
	}
	status, err := accountRequest(ctx, http.MethodDelete, strings.TrimSuffix(control, "/")+"/v1/me", token, &answer)
	if err != nil {
		return scheduledDeletion{}, fmt.Errorf("delete account: %w", err)
	}
	switch {
	case status == http.StatusAccepted && answer.Status == "scheduled" && !answer.PurgeAfter.IsZero():
		return answer.scheduledDeletion, nil
	case status == http.StatusNotFound:
		return scheduledDeletion{}, errors.New("delete account: you have no account to delete")
	}
	return scheduledDeletion{}, fmt.Errorf("delete account: %s (%d %s)", answer.Error, status, http.StatusText(status))
}

// restoreAccount asks the control service to undo the signed-in account's
// deletion and returns the restored network's name.
func restoreAccount(ctx context.Context, control, token string) (string, error) {
	var answer struct {
		Network struct {
			Name string `json:"name"`
		} `json:"network"`
		Error string `json:"error"`
	}
	status, err := accountRequest(ctx, http.MethodPost, strings.TrimSuffix(control, "/")+"/v1/me/restore", token, &answer)
	if err != nil {
		return "", fmt.Errorf("restore account: %w", err)
	}
	switch status {
	case http.StatusOK:
		if answer.Network.Name == "" {
			return "your network", nil
		}
		return answer.Network.Name, nil
	case http.StatusConflict:
		return "", errors.New("restore account: your account is not scheduled for deletion")
	case http.StatusNotFound:
		return "", errors.New("restore account: there is no account to restore; a deletion can be undone only for 30 days")
	}
	return "", fmt.Errorf("restore account: %s (%d %s)", answer.Error, status, http.StatusText(status))
}

// accountRequest sends an account request with the sign-in's token and
// decodes the JSON answer, if any, into answer.
func accountRequest(ctx context.Context, method, url, token string, answer any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(answer)
	return resp.StatusCode, nil
}
