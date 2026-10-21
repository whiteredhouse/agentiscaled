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
	"errors"
	"fmt"

	"github.com/google/agentmesh/internal/node"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/spf13/cobra"
)

// newStateCmd moves a member between this node's database and a state
// directory, the layout the native SDKs use, so an identity enrolled by one
// runs under the other. Both directions need the node stopped: the database
// lock refuses a running node's data directory.
func newStateCmd() *cobra.Command {
	stateCmd := &cobra.Command{
		Use:   "state",
		Short: "Export or import this node's identity and credential as an SDK state directory",
		Long: "Export or import this node's identity and credential as a state directory.\n\n" +
			"A state directory is how the native SDKs keep a member: identity.key (the\n" +
			"libp2p private key) and credential.json (the MemberCredential message of\n" +
			"api/agentmesh.proto as JSON). Exporting hands this node's identity to a program\n" +
			"written with an SDK; importing runs an SDK-enrolled identity as a agentmesh-node.\n" +
			"Stop the node first; a running node holds its data directory.",
	}

	exportCmd := &cobra.Command{
		Use:   "export <dir>",
		Short: "Write this node's identity and credential to a state directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStoreForState()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			privKey, err := store.LoadKey()
			if err != nil {
				return err
			}
			if len(privKey) == 0 {
				return errors.New("this node has no identity yet; run it once or enroll first")
			}
			credential, err := store.Credential()
			if err != nil {
				return err
			}
			if len(credential.Biscuit) == 0 {
				return errors.New("this node has not enrolled; nothing to export beyond the key")
			}
			if err := node.WriteStateDir(args[0], privKey, credential); err != nil {
				return err
			}
			id, err := peerIDOf(privKey)
			if err != nil {
				return err
			}
			fmt.Printf("Exported %s to %s\nThe files hold the node's private key and credential; whoever reads them is this member.\n", id, args[0])
			return nil
		},
	}

	importCmd := &cobra.Command{
		Use:   "import <dir>",
		Short: "Replace this node's identity and credential with a state directory's",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			privKey, credential, err := node.ReadStateDir(args[0])
			if err != nil {
				return err
			}
			id, err := peerIDOf(privKey)
			if err != nil {
				return err
			}
			store, err := openStoreForState()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			if existing, _ := store.LoadKey(); len(existing) > 0 && !assumeYesFlag {
				existingID, err := peerIDOf(existing)
				if err != nil {
					existingID = "an identity"
				}
				return fmt.Errorf("%s already holds %s; pass --yes to replace it with %s", resolveDataDir(), existingID, id)
			}
			if err := store.SaveKey(privKey); err != nil {
				return err
			}
			if err := store.SetCredential(credential); err != nil {
				return err
			}
			fmt.Printf("Imported %s from %s into %s\nStart the node with 'agentmesh-node run'; it resumes as that member.\n", id, args[0], resolveDataDir())
			return nil
		},
	}
	importCmd.Flags().BoolVar(&assumeYesFlag, "yes", false, "Replace an identity already in the data directory")

	stateCmd.AddCommand(exportCmd, importCmd)
	return stateCmd
}

func openStoreForState() (*node.Store, error) {
	store, err := node.NewStore(resolveDataDir())
	if errors.Is(err, node.ErrStoreLocked) {
		return nil, fmt.Errorf("%w; stop it first", err)
	}
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", resolveDataDir(), err)
	}
	return store, nil
}

func peerIDOf(privKey []byte) (string, error) {
	priv, err := crypto.UnmarshalPrivateKey(privKey)
	if err != nil {
		return "", err
	}
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}
