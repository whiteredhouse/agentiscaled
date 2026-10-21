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

package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p/core/crypto"
)

func TestOptionsValidateControlPlaneKeySize(t *testing.T) {
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	base := Options{PrivKey: priv, Store: store, RequiredRole: api.RoleNode}

	if err := base.Validate(); err != nil {
		t.Fatalf("baseline options should validate: %v", err)
	}

	good := base
	good.ControlPlanePubKey = make([]byte, ed25519.PublicKeySize)
	if err := good.Validate(); err != nil {
		t.Errorf("32-byte key rejected: %v", err)
	}

	// A truncated hex flag value must fail here, not panic inside ed25519 on
	// the first router handshake.
	bad := base
	bad.ControlPlanePubKey = make([]byte, 31)
	err = bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Errorf("31-byte key: err = %v, want size error", err)
	}
}

func TestOptionsReachability(t *testing.T) {
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	base := Options{PrivKey: priv, Store: store, RequiredRole: api.RoleNode}

	for _, ok := range []Reachability{"", ReachabilityPrivate, ReachabilityAuto} {
		o := base
		o.Reachability = ok
		if err := o.Validate(); err != nil {
			t.Errorf("reachability %q rejected: %v", ok, err)
		}
	}
	o := base
	o.Reachability = "public"
	if err := o.Validate(); err == nil || !strings.Contains(err.Error(), "reachability") {
		t.Errorf("reachability %q: err = %v, want rejection", o.Reachability, err)
	}

	// Unset means private: the node is reached through the routers unless
	// the operator says otherwise.
	o = base
	o.Default()
	if o.Reachability != ReachabilityPrivate {
		t.Errorf("default reachability = %q, want %q", o.Reachability, ReachabilityPrivate)
	}
}
