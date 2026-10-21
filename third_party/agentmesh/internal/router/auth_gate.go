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

package router

import (
	"context"
	"errors"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// streamScopeUnauthenticated labels a stream the gate refused: its peer's
// connection ended before the peer authenticated.
const streamScopeUnauthenticated = "unauthenticated"

// authPollInterval is how often a stream waiting on its peer's handshake
// checks for it.
const authPollInterval = 100 * time.Millisecond

// defaultAuthWait bounds the wait for a peer's handshake when the router
// runs without an auth deadline, which would otherwise close the connection.
const defaultAuthWait = time.Minute

var errNotAuthenticated = errors.New("peer did not authenticate")

// authGate is the host the DHT and gossipsub are built on. A library
// registers its stream handlers through it, and a handler so registered
// serves a peer only once that peer completed the mesh handshake on this
// router; a stream the library opens waits for the same. The handshake
// itself, identify and the relay keep the plain host: the first two are
// what the handshake needs, the relay checks admission in its own ACL.
//
// A stream that arrives before its peer's handshake is held, not reset: a
// member's DHT and gossip streams open on connect, a moment before its
// handshake completes, and a reset would make each library drop the peer
// and retry on its own schedule. An unauthenticated peer's stream is held
// unserved until its connection closes at the auth deadline, and counted.
type authGate struct {
	host.Host
	r *Router
}

func (g *authGate) SetStreamHandler(pid protocol.ID, h network.StreamHandler) {
	g.Host.SetStreamHandler(pid, g.r.requireAuthenticated(h))
}

func (g *authGate) SetStreamHandlerMatch(pid protocol.ID, match func(protocol.ID) bool, h network.StreamHandler) {
	g.Host.SetStreamHandlerMatch(pid, match, g.r.requireAuthenticated(h))
}

func (g *authGate) NewStream(ctx context.Context, p peer.ID, pids ...protocol.ID) (network.Stream, error) {
	if err := g.r.awaitAuthenticated(ctx, p); err != nil {
		return nil, err
	}
	return g.Host.NewStream(ctx, p, pids...)
}

// requireAuthenticated wraps a stream handler so it runs only once the
// stream's peer has authenticated.
func (r *Router) requireAuthenticated(next network.StreamHandler) network.StreamHandler {
	return func(s network.Stream) {
		p := s.Conn().RemotePeer()
		if err := r.awaitAuthenticated(r.lifetime(), p); err != nil {
			streamsRefusedTotal.WithLabelValues(streamScopeUnauthenticated, string(s.Protocol())).Inc()
			logger.Debugf("[AuthZ] Refusing %s stream from %s: %v", s.Protocol(), p, err)
			_ = s.Reset()
			return
		}
		next(s)
	}
}

// awaitAuthenticated returns once p has authenticated, or with an error
// when ctx ends, p's connections are gone, or the auth deadline passes.
func (r *Router) awaitAuthenticated(ctx context.Context, p peer.ID) error {
	if r.isPeerAuthenticated(p) {
		return nil
	}
	wait := r.config.AuthDeadline
	if wait <= 0 {
		wait = defaultAuthWait
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, wait)
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
		if r.isPeerAuthenticated(p) {
			return nil
		}
		if r.Host.Network().Connectedness(p) != network.Connected {
			return errNotAuthenticated
		}
	}
}

// lifetime is the router's own context, for work not tied to a request.
func (r *Router) lifetime() context.Context {
	if r.ctx != nil {
		return r.ctx
	}
	return context.Background()
}
