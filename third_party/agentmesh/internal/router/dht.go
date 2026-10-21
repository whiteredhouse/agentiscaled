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
	"fmt"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/google/agentmesh/api"
)

// dhtTableBucketSize is kademlia's k on a router: how many routers one
// bucket of its table holds. The routers are a full mesh, so a table that
// holds them all costs nothing, and it is what lets a router name the
// routers closest to a key from its own table (closestRouters). A mesh with
// more routers than this per bucket still works; the closest routers are
// then the closest the table knows.
const dhtTableBucketSize = 64

// dhtOptions configures the router's DHT: a server among the other
// routers, with every router in its table. The table is what the directory
// uses: the routers closest to a key are where a record is placed and
// where a lookup asks (closestRouters). The records themselves are kept
// in recordStore, not in the DHT's provider store.
//
// Its table holds, and its lookups ask, authenticated peer routers only: a
// peer in server mode that never authenticated would otherwise be handed
// queries and records. A router enters the table when it authenticates
// (admitPeerRouter), not when the DHT first hears of it.
func (r *Router) dhtOptions() []dht.Option {
	return []dht.Option{
		dht.Mode(dht.ModeServer),
		dht.ProtocolPrefix(api.DHTProtocolPrefix),
		dht.BucketSize(dhtTableBucketSize),
		dht.RoutingTableFilter(func(_ interface{}, p peer.ID) bool { return r.isAuthenticatedPeerRouter(p) }),
		dht.QueryFilter(func(_ interface{}, ai peer.AddrInfo) bool { return r.isAuthenticatedPeerRouter(ai.ID) }),
	}
}

// isAuthenticatedPeerRouter reports whether p is a peer router that holds a
// current authentication with this router.
func (r *Router) isAuthenticatedPeerRouter(p peer.ID) bool {
	if _, isRouter := r.peerRouters.Load(p); !isRouter {
		return false
	}
	return r.isPeerAuthenticated(p)
}

// admitPeerRouter records p as a peer router that authenticated and puts it
// in the DHT's table: the table's filter kept it out until now, and the
// DHT itself only adds a peer when it first hears of it.
func (r *Router) admitPeerRouter(p peer.ID) {
	if _, known := r.peerRouters.LoadOrStore(p, true); !known {
		logger.Infof("[Federation] Peer router %s authenticated with us", p)
		r.Host.ConnManager().Protect(p, federationTag)
	}
	if r.DHT == nil {
		return
	}
	if added, err := r.DHT.RoutingTable().TryAddPeer(p, true, false); err != nil {
		logger.Warnf("[DHT] Peer router %s not added to the routing table: %v", p, err)
	} else if added {
		logger.Infof("[DHT] Peer router %s added to the routing table (%d routers)", p, r.DHT.RoutingTable().Size())
	}
}

// syncDirectory reads the mesh's directory parameters from /info before the
// DHT is built, since its record TTL is fixed at construction. The control
// plane answered the keys sync a moment ago, so a failure here is one to
// stop on rather than to run a TTL of the router's own.
func (r *Router) syncDirectory() error {
	info, err := r.controlPlane(10 * time.Second).FetchInfo(r.ctx)
	if err != nil {
		return fmt.Errorf("directory parameters: %w", err)
	}
	r.directory = api.DirectorySettingsFrom(info.GetDirectory())
	logger.Infof("[DHT] Directory parameters from the control plane: replication %d, reprovide every %s, records kept %s", r.directory.Replication, r.directory.Reprovide, r.directory.RecordTTL)
	return nil
}
