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
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// streamScopeUnauthenticated labels a stream the gate refused: its peer
// never authenticated while the stream waited.
const streamScopeUnauthenticated = "unauthenticated"

// authWait bounds how long a stream waits for its peer's handshake: a
// router's auth deadline for the connection, so a peer that never
// authenticates is cut off with it.
const authWait = time.Minute

const authPollInterval = 100 * time.Millisecond

var errNotAuthenticated = errors.New("peer did not authenticate")

// meshHost is the host the node's DHT and gossipsub are built on. A stream
// a library opens to a router runs the handshake with that router first,
// so the router serves it at once instead of holding it; a stream to another
// member waits for that member to authenticate, as it does when it calls.
// A stream a library serves is served only to a router this node
// authenticated with or a peer that authenticated with this node; any other
// waits for that, bounded, and is then refused and counted.
type meshHost struct {
	host.Host
	n *AgentMeshNode
}

func (h *meshHost) SetStreamHandler(pid protocol.ID, handler network.StreamHandler) {
	h.Host.SetStreamHandler(pid, h.n.requireAuthenticated(handler))
}

func (h *meshHost) SetStreamHandlerMatch(pid protocol.ID, match func(protocol.ID) bool, handler network.StreamHandler) {
	h.Host.SetStreamHandlerMatch(pid, match, h.n.requireAuthenticated(handler))
}

func (h *meshHost) NewStream(ctx context.Context, p peer.ID, pids ...protocol.ID) (network.Stream, error) {
	if err := h.n.ensurePeerAuthenticated(ctx, p); err != nil {
		return nil, err
	}
	return h.Host.NewStream(ctx, p, pids...)
}

// isPeerAuthenticated reports whether p is a router this node authenticated
// with on an open session, or a peer whose handshake with this node holds.
func (n *AgentMeshNode) isPeerAuthenticated(p peer.ID) bool {
	return n.isAuthenticatedAndConnected(p) || n.isAdmitted(p)
}

// ensurePeerAuthenticated returns once p is authenticated: for a router the
// control plane lists, by running the handshake (one at a time per router,
// shared with the one the connection notifee started). Any other peer
// authenticates with this node when it calls it; until it has, nothing is
// sent to it, and a lookup is not held waiting for a peer it cannot vouch
// for.
func (n *AgentMeshNode) ensurePeerAuthenticated(ctx context.Context, p peer.ID) error {
	if n.isPeerAuthenticated(p) {
		return nil
	}
	if !n.isCatalogRouter(p) {
		return fmt.Errorf("%s: %w", p, errNotAuthenticated)
	}
	biscuitBytes, err := n.loadIdentityForAuth()
	if err != nil {
		return err
	}
	return n.authRouter(ctx, peer.AddrInfo{ID: p}, biscuitBytes)
}

// requireAuthenticated wraps a stream handler so it runs only once the
// stream's peer is authenticated.
func (n *AgentMeshNode) requireAuthenticated(next network.StreamHandler) network.StreamHandler {
	return func(s network.Stream) {
		p := s.Conn().RemotePeer()
		if err := n.awaitPeerAuthenticated(context.Background(), p); err != nil {
			streamsRefusedTotal.WithLabelValues(streamScopeUnauthenticated, string(s.Protocol())).Inc()
			logger.Debugf("[AuthZ] Refusing %s stream from %s: %v", s.Protocol(), p, err)
			_ = s.Reset()
			return
		}
		next(s)
	}
}

// isCatalogRouter reports whether p is a router the control plane lists.
func (n *AgentMeshNode) isCatalogRouter(p peer.ID) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range n.routerCatalog {
		if c.ID == p {
			return true
		}
	}
	return false
}

// awaitPeerAuthenticated returns once p is authenticated, or with an error
// when ctx ends, p's connections are gone, or authWait passes.
func (n *AgentMeshNode) awaitPeerAuthenticated(ctx context.Context, p peer.ID) error {
	if n.isPeerAuthenticated(p) {
		return nil
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, authWait)
	defer cancel()
	tick := time.NewTicker(authPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return parent.Err()
			}
			return errNotAuthenticated
		case <-tick.C:
		}
		if n.isPeerAuthenticated(p) {
			return nil
		}
		if n.Host.Network().Connectedness(p) != network.Connected {
			return errNotAuthenticated
		}
	}
}
