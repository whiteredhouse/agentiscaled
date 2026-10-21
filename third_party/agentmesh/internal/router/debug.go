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
	"encoding/json"
	"net/http"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// peerView is what the router knows about one peer: the addresses it would
// hand out for it in a DHT answer, and the standing the peer has here.
// Unversioned diagnostics on the operator listener, parsed by no client.
type peerView struct {
	PeerID        string   `json:"peer_id"`
	Connectedness string   `json:"connectedness"`
	Authenticated bool     `json:"authenticated"`
	PeerRouter    bool     `json:"peer_router"`
	Connections   []string `json:"connections"`
	Addrs         []string `json:"addrs"`
}

// handleDebugPeer serves GET /debug/peer?peer_id=<id>.
func (r *Router) handleDebugPeer(w http.ResponseWriter, req *http.Request) {
	if !r.isReady.Load() {
		http.Error(w, "router is not online yet", http.StatusServiceUnavailable)
		return
	}
	p, err := peer.Decode(req.URL.Query().Get("peer_id"))
	if err != nil {
		http.Error(w, "peer_id: "+err.Error(), http.StatusBadRequest)
		return
	}
	_, isRouter := r.peerRouters.Load(p)
	view := peerView{
		PeerID:        p.String(),
		Connectedness: r.Host.Network().Connectedness(p).String(),
		Authenticated: r.isPeerAuthenticated(p),
		PeerRouter:    isRouter,
		Connections:   []string{},
		Addrs:         []string{},
	}
	for _, c := range r.Host.Network().ConnsToPeer(p) {
		dir := "inbound"
		if c.Stat().Direction == network.DirOutbound {
			dir = "outbound"
		}
		view.Connections = append(view.Connections, dir+" "+c.RemoteMultiaddr().String())
	}
	for _, a := range r.Host.Peerstore().Addrs(p) {
		view.Addrs = append(view.Addrs, a.String())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(view)
}
