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
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/standalone"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
)

// adminClient talks to a running agentmesh-one's admin API; subcommands never open
// the database from a second process.
type adminClient struct {
	client *http.Client
	server string
	token  string
}

// resolveAdminToken picks the admin credential: a file named by the flag,
// then the AGENTMESH_ADMIN_TOKEN env, then the token persisted in data-dir by a
// previous run. The value itself is never a flag argument.
func resolveAdminToken(tokenPath, dataDir string) (string, error) {
	if tok, err := secretFromPathOrEnv(tokenPath, "AGENTMESH_ADMIN_TOKEN"); err != nil {
		return "", fmt.Errorf("invalid --admin-token-path: %w", err)
	} else if tok != "" {
		return tok, nil
	}
	tok, err := standalone.AdminTokenFromDataDir(dataDir)
	if err != nil {
		return "", fmt.Errorf("no admin token: pass --admin-token-path, set AGENTMESH_ADMIN_TOKEN, or point --data-dir at a agentmesh-one data directory (%v)", err)
	}
	return tok, nil
}

func (c *adminClient) do(method, path, contentType string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(method, c.server+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(respBody))
	}
	return respBody, nil
}

// createToken mints a bootstrap token. autonomousRecovery is copied onto
// every node the token enrolls: such a node may still /refresh after the
// control plane's signing key rotated past its grace period, which is what
// a device that spends days offline needs and what a stolen device should
// not get.
func (c *adminClient) createToken(role string, ttlHours, maxUsages int, description string, autonomousRecovery bool) (*api.BootstrapTokenCreateResponse, error) {
	payload, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&api.BootstrapTokenCreateRequest{
		Role:               role,
		TtlHours:           int32(ttlHours),
		MaxUsages:          int32(maxUsages),
		Description:        description,
		AutonomousRecovery: autonomousRecovery,
	})
	if err != nil {
		return nil, err
	}
	body, err := c.do(http.MethodPost, "/admin/bootstrap-tokens", "application/json", payload)
	if err != nil {
		return nil, err
	}
	created := &api.BootstrapTokenCreateResponse{}
	if err := protojson.Unmarshal(body, created); err != nil {
		return nil, fmt.Errorf("failed to decode response %q: %w", body, err)
	}
	return created, nil
}

func (c *adminClient) listTokens() ([]*api.BootstrapToken, error) {
	body, err := c.do(http.MethodGet, "/admin/bootstrap-tokens", "", nil)
	if err != nil {
		return nil, err
	}
	list := &api.BootstrapTokenListResponse{}
	if err := protojson.Unmarshal(body, list); err != nil {
		return nil, fmt.Errorf("failed to decode response %q: %w", body, err)
	}
	return list.GetTokens(), nil
}

func (c *adminClient) banPeer(peerID string) error {
	payload, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&api.TokenRevokeRequest{PeerId: peerID})
	if err != nil {
		return err
	}
	_, err = c.do(http.MethodPost, "/admin/revoke", "application/json", payload)
	return err
}

// revokeToken soft-revokes a bootstrap token. idOrPrefix may be the full
// SHA-256 id or the unique prefix `token list` and `token qr` print.
func (c *adminClient) revokeToken(idOrPrefix string) (string, error) {
	id, err := c.resolveTokenID(idOrPrefix)
	if err != nil {
		return "", err
	}
	_, err = c.do(http.MethodDelete, "/admin/bootstrap-tokens/"+id, "", nil)
	return id, err
}

func (c *adminClient) resolveTokenID(idOrPrefix string) (string, error) {
	if idOrPrefix == "" {
		return "", fmt.Errorf("token id is required")
	}
	list, err := c.listTokens()
	if err != nil {
		return "", err
	}
	var matches []string
	for _, tok := range list {
		if strings.HasPrefix(tok.GetId(), idOrPrefix) {
			matches = append(matches, tok.GetId())
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no bootstrap token with id %q", idOrPrefix)
	default:
		return "", fmt.Errorf("token id %q is ambiguous (%d matches); pass more characters", idOrPrefix, len(matches))
	}
}

// newAdminSubcommands wires the token and admin command trees onto root.
func newAdminSubcommands() []*cobra.Command {
	var (
		server         string
		adminTokenPath string
		dataDir        string
		clientFactory  = func() (*adminClient, error) {
			tok, err := resolveAdminToken(adminTokenPath, dataDir)
			if err != nil {
				return nil, err
			}
			return &adminClient{
				client: &http.Client{Timeout: 10 * time.Second},
				server: server,
				token:  tok,
			}, nil
		}
	)

	addSharedFlags := func(cmd *cobra.Command) {
		// cobra's cmd.Print* goes to stderr unless told otherwise; results
		// belong on stdout so `token create | awk` and $(...) capture them.
		cmd.SetOut(os.Stdout)
		cmd.SetErr(os.Stderr)
		cmd.PersistentFlags().StringVar(&server, "server", "http://127.0.0.1:8080", "Base URL of the running agentmesh-one server")
		cmd.PersistentFlags().StringVar(&adminTokenPath, "admin-token-path", "", "File containing the admin API bearer token (or env AGENTMESH_ADMIN_TOKEN, or read from --data-dir)")
		cmd.PersistentFlags().StringVar(&dataDir, "data-dir", ".", "agentmesh-one data directory holding the persisted admin token")
	}

	var (
		role               string
		ttlHours           int
		maxUsages          int
		description        string
		autonomousRecovery bool
	)
	tokenCreate := &cobra.Command{
		Use:   "create",
		Short: "Generate a new scoped bootstrap token",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFactory()
			if err != nil {
				return err
			}
			created, err := c.createToken(role, ttlHours, maxUsages, description, autonomousRecovery)
			if err != nil {
				return err
			}
			cmd.Printf("Token:    %s\n", created.GetToken())
			cmd.Printf("Role:     %s\n", created.GetRole())
			cmd.Printf("Expires:  %s\n", created.GetExpireTime().AsTime().Format(time.RFC3339))
			cmd.PrintErrln("The plain token is shown only once; store it now.")
			return nil
		},
	}
	tokenCreate.Flags().StringVar(&role, "role", api.RoleNode, "Role bound to the token")
	tokenCreate.Flags().IntVar(&ttlHours, "ttl-hours", 24, "Token validity in hours")
	tokenCreate.Flags().IntVar(&maxUsages, "max-usages", 1, "How many enrollments the token allows")
	tokenCreate.Flags().StringVar(&description, "description", "", "Free-form note stored with the token")
	tokenCreate.Flags().BoolVar(&autonomousRecovery, "autonomous-recovery", false, autonomousRecoveryHelp)

	tokenList := &cobra.Command{
		Use:   "list",
		Short: "List bootstrap tokens and their status",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFactory()
			if err != nil {
				return err
			}
			list, err := c.listTokens()
			if err != nil {
				return err
			}
			now := time.Now()
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tROLE\tUSAGES\tSTATUS\tEXPIRES\tDESCRIPTION")
			for _, tok := range list {
				id := tok.GetId()
				if len(id) > 12 {
					id = id[:12]
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\t%s\t%s\n",
					id, tok.GetRole(), tok.GetUsagesCount(), tok.GetMaxUsages(), tokenStatus(tok, now),
					tok.GetExpireTime().AsTime().Format(time.RFC3339), tok.GetDescription())
			}
			return tw.Flush()
		},
	}

	tokenRevoke := &cobra.Command{
		Use:   "revoke <token-id>",
		Short: "Revoke a bootstrap token so no further device can enroll with it",
		Long: "Soft-revokes a bootstrap token by its id or the id prefix shown by " +
			"`token list` and `token qr`. Devices already enrolled keep their identity; " +
			"use `admin ban` for those.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFactory()
			if err != nil {
				return err
			}
			id, err := c.revokeToken(args[0])
			if err != nil {
				return err
			}
			cmd.Printf("Token %s revoked\n", id[:12])
			return nil
		},
	}

	tokenCmd := &cobra.Command{Use: "token", Short: "Manage bootstrap tokens on a running agentmesh-one"}
	addSharedFlags(tokenCmd)
	tokenCmd.AddCommand(tokenCreate, tokenList, tokenRevoke, newTokenQRCommand(clientFactory, &server))

	adminBan := &cobra.Command{
		Use:   "ban <peer-id>",
		Short: "Ban a peer ID from the mesh",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFactory()
			if err != nil {
				return err
			}
			if err := c.banPeer(args[0]); err != nil {
				return err
			}
			cmd.Printf("Peer %s banned\n", args[0])
			return nil
		},
	}

	adminCmd := &cobra.Command{Use: "admin", Short: "Administrative actions on a running agentmesh-one"}
	addSharedFlags(adminCmd)
	adminCmd.AddCommand(adminBan)

	return []*cobra.Command{tokenCmd, adminCmd}
}

// tokenStatus mirrors the control plane's usability check (/enroll and
// ConsumeBootstrapTokenUsage) for display.
func tokenStatus(tok *api.BootstrapToken, now time.Time) string {
	switch {
	case tok.GetRevokeTime() != nil:
		return "revoked"
	case tok.GetExpireTime() != nil && now.After(tok.GetExpireTime().AsTime()):
		return "expired"
	case tok.GetUsagesCount() >= tok.GetMaxUsages():
		return "exhausted"
	default:
		return "active"
	}
}

// autonomousRecoveryHelp is shared by every command that mints tokens.
const autonomousRecoveryHelp = "Let enrolled devices renew even after the signing key rotated past its grace period (devices that stay offline for days); a lost device then keeps renewing until banned"

// newTokenQRCommand mints a node token and renders it, with the URL devices
// must enroll against, as a terminal QR code. server points at the shared
// --server flag so the QR defaults to the URL the admin used.
func newTokenQRCommand(clientFactory func() (*adminClient, error), server *string) *cobra.Command {
	var (
		enrollURL          string
		ttlHours           int
		maxUsages          int
		description        string
		autonomousRecovery bool
	)
	cmd := &cobra.Command{
		Use:   "qr",
		Short: "Mint a device enrollment token and print it as a QR code",
		Long: "Mints a bootstrap token for the node role and renders " +
			"mesh://enroll?server=<url>&token=<token> as a QR code for the Agent Mesh mobile app. " +
			"Single use by default; --max-usages lets one code, projected in a room, enroll " +
			"many devices until it is exhausted, expires or is revoked. " +
			"The embedded URL defaults to --server; pass --enroll-url when devices reach " +
			"the mesh on a different address (a tunnel hostname, a reverse proxy). Devices " +
			"only trust https control planes (plaintext http is accepted for loopback only).",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := enrollURL
			if target == "" {
				target = *server
			}
			target = strings.TrimRight(target, "/")
			if _, _, err := api.ParseEnrollURI(api.EnrollURI(target, "probe")); err != nil {
				return fmt.Errorf("enrollment URL %q: %w", target, err)
			}
			if maxUsages < 1 {
				return fmt.Errorf("--max-usages must be at least 1")
			}
			c, err := clientFactory()
			if err != nil {
				return err
			}
			if description == "" {
				description = "device enrollment via " + target
			}
			created, err := c.createToken(api.RoleNode, ttlHours, maxUsages, description, autonomousRecovery)
			if err != nil {
				return err
			}
			return printEnrollQR(cmd.OutOrStdout(), target, created.Token, time.Duration(ttlHours)*time.Hour, maxUsages)
		},
	}
	cmd.Flags().StringVar(&enrollURL, "enroll-url", "", "https URL embedded in the QR code when it differs from --server")
	cmd.Flags().IntVar(&ttlHours, "ttl-hours", 1, "Token validity in hours")
	cmd.Flags().IntVar(&maxUsages, "max-usages", 1, "How many devices may enroll with this code")
	cmd.Flags().StringVar(&description, "description", "", "Free-form note stored with the token")
	cmd.Flags().BoolVar(&autonomousRecovery, "autonomous-recovery", false, autonomousRecoveryHelp)
	return cmd
}
