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
	"math/rand/v2"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/google/agentmesh/api"
)

const (
	// goAwayTimeout bounds one go-away stream: a member that does not
	// answer is closed like one that does not speak the protocol.
	goAwayTimeout = 5 * time.Second
	// goAwayFanOut is how many go-away streams are open at once: a draining
	// router with thousands of members finishes within the shutdown grace.
	goAwayFanOut = 64
	// shedInterval is how often the router compares its inbound connections
	// with the high watermark.
	shedInterval = 5 * time.Second
	// DefaultOverloadRetryAfter is how long a member shed for load stays off
	// this router: long enough for the others to absorb it, short enough
	// that a router that emptied out is used again.
	DefaultOverloadRetryAfter = 5 * time.Minute
)

// goAwayGrace is how long a member told to go away keeps its connection,
// enough to attach elsewhere before the circuits through this router are
// cut. A variable so tests do not wait for it.
var goAwayGrace = 5 * time.Second

// sendGoAway tells one peer to leave and returns once the peer has closed
// the stream or the timeout passed. A peer that does not speak the protocol
// is only closed, by the caller, which is what a trim did.
func (r *Router) sendGoAway(ctx context.Context, p peer.ID, msg *api.RouterGoAway) {
	ctx, cancel := context.WithTimeout(ctx, goAwayTimeout)
	defer cancel()
	reason := msg.GetReason().String()
	s, err := r.Host.NewStream(ctx, p, api.GoAwayProtocolID)
	if err != nil {
		logger.Debugf("[GoAway] %s does not take a go-away, closing instead: %v", p, err)
		goAwaySentTotal.WithLabelValues(reason, goAwayUnsupported).Inc()
		return
	}
	_ = s.SetDeadline(time.Now().Add(goAwayTimeout))
	data, _ := proto.Marshal(msg)
	if err := msgio.NewVarintWriter(s).WriteMsg(data); err != nil {
		logger.Debugf("[GoAway] Writing to %s: %v", p, err)
		_ = s.Reset()
		goAwaySentTotal.WithLabelValues(reason, goAwayFailed).Inc()
		return
	}
	// The member closes its end once it has acted on the message.
	_ = s.CloseWrite()
	_, _ = s.Read(make([]byte, 1))
	_ = s.Close()
	goAwaySentTotal.WithLabelValues(reason, goAwayOK).Inc()
}

// goAwayAll sends msg to every peer in peers, goAwayFanOut streams at a
// time, forgets them as authenticated, and after goAwayGrace closes their
// connections. It returns once that is done or ctx ends.
func (r *Router) goAwayAll(ctx context.Context, peers []peer.ID, msg *api.RouterGoAway) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, goAwayFanOut)
send:
	for _, p := range peers {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break send
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.sendGoAway(ctx, p, msg)
		}()
	}
	wg.Wait()
	for _, p := range peers {
		r.forgetPeer(p)
	}
	select {
	case <-time.After(goAwayGrace):
	case <-ctx.Done():
	}
	for _, p := range peers {
		_ = r.Host.Network().ClosePeer(p)
	}
}

// members lists the authenticated peers that are not routers: the ones a
// go-away is for. Federation connections stay whatever the load.
func (r *Router) members() []peer.ID {
	var out []peer.ID
	r.authenticatedPeers.Range(func(k, _ any) bool {
		p := k.(peer.ID)
		if _, isRouter := r.peerRouters.Load(p); !isRouter && r.isPeerAuthenticated(p) {
			out = append(out, p)
		}
		return true
	})
	return out
}

// drainMembers tells every member the router is stopping. Run from Close,
// before the host goes: the members attach elsewhere now instead of
// finding out when the connection dies and redialling a router that is
// gone. They stay away for the lease the router withdrew with, the time
// the control plane keeps it listed.
func (r *Router) drainMembers() {
	if r.Host == nil {
		return
	}
	members := r.members()
	if len(members) == 0 {
		logger.Infow("[GoAway] Draining: no members attached, nothing to send away", "router_id", r.Host.ID().String())
		return
	}
	retryAfter := r.config.ShutdownLeaseTTL
	if retryAfter <= 0 {
		retryAfter = DefaultShutdownLeaseTTL
	}
	logger.Infof("[GoAway] Draining: sending %d members away for %s", len(members), retryAfter)
	ctx, cancel := context.WithTimeout(context.Background(), retryAfter)
	defer cancel()
	r.goAwayAll(ctx, members, &api.RouterGoAway{
		Reason:     api.RouterGoAway_DRAINING,
		RetryAfter: durationpb.New(retryAfter),
	})
}

// inboundConnections counts the connections peers opened to this router.
func (r *Router) inboundConnections() int {
	n := 0
	for _, c := range r.Host.Network().Conns() {
		if c.Stat().Direction == network.DirInbound {
			n++
		}
	}
	return n
}

// enforceAuthDeadline closes an inbound connection whose peer has not
// passed the handshake AuthDeadline after it opened. A peer router is left
// alone whatever its state, as is a peer that authenticated on another
// connection.
func (r *Router) enforceAuthDeadline(c network.Conn) {
	if r.config.AuthDeadline <= 0 || c.Stat().Direction != network.DirInbound {
		return
	}
	time.AfterFunc(r.config.AuthDeadline, func() {
		if c.IsClosed() || (r.ctx != nil && r.ctx.Err() != nil) {
			return
		}
		p := c.RemotePeer()
		if r.isPeerAuthenticated(p) {
			return
		}
		if _, isRouter := r.peerRouters.Load(p); isRouter {
			return
		}
		logger.Debugf("[AuthN] Closing connection from %s: no handshake within %s", p, r.config.AuthDeadline)
		unauthenticatedClosedTotal.Inc()
		_ = c.Close()
	})
}

// shedIfOverloaded sends members away when the router holds at least its
// high watermark of them: as many as take it back to the low watermark,
// chosen at random so no member is always the one. Members are counted,
// not connections: a member attached elsewhere holds a connection here for
// its DHT queries without being one, and those connections are what the
// connection manager closes first past the high watermark (see memberTag).
// Returns how many were sent away.
func (r *Router) shedIfOverloaded(ctx context.Context) int {
	members := r.members()
	if len(members) < r.config.HighWaterMark {
		return 0
	}
	excess := len(members) - r.config.LowWaterMark
	if excess <= 0 {
		return 0
	}
	rand.Shuffle(len(members), func(i, j int) { members[i], members[j] = members[j], members[i] })
	shed := members[:excess]
	logger.Warnf("[GoAway] %d members, high watermark %d: sending %d away for %s", len(members), r.config.HighWaterMark, len(shed), DefaultOverloadRetryAfter)
	r.goAwayAll(ctx, shed, &api.RouterGoAway{
		Reason:     api.RouterGoAway_OVERLOADED,
		RetryAfter: durationpb.New(DefaultOverloadRetryAfter),
	})
	return len(shed)
}

func (r *Router) runShedLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(shedInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !r.shutdown.Load() {
				r.shedIfOverloaded(r.ctx)
			}
		case <-r.ctx.Done():
			return
		}
	}
}
