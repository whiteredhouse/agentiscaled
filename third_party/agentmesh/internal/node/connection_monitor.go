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

import "context"

// routerAttacher is the node as the connection monitor sees it: how many
// routers it keeps, how many it wants, and the two ways to get more.
type routerAttacher interface {
	// AttachedRouters is how many routers the node holds a live session with
	// among those it keeps.
	AttachedRouters() int
	// WantRouters is how many it keeps: Options.Routers, or every router it
	// knows of when there are fewer.
	WantRouters() int
	// TopUp attaches to more routers from what the node knows and returns how
	// many it then keeps.
	TopUp(ctx context.Context) int
	// RefreshRouters re-reads the routers from the control plane.
	RefreshRouters(ctx context.Context) error
}

// checkRouterConnection keeps the node attached to the routers it wants.
// It returns two booleans:
//   - stable: the node holds at least one router and did not have to
//     recover one on this check. Short of its count but on the mesh, it
//     tops up and still reports stable: the mesh is there, the margin is
//     being restored.
//   - reconnected: the node had no router and found one, either among the
//     routers it already knew of or after re-reading them from the control
//     plane. Neither is a failure of the mesh.
//
// Only a node left with no router at all, after both, reports a failure;
// the caller counts those and exits on too many in a row.
func checkRouterConnection(ctx context.Context, mgr routerAttacher) (stable bool, reconnected bool) {
	have, want := mgr.AttachedRouters(), mgr.WantRouters()
	if have > 0 {
		if have < want {
			logger.Infof("[Monitor] Attached to %d of %d routers; attaching to more", have, want)
			mgr.TopUp(ctx)
		}
		return true, false
	}

	logger.Warn("[Monitor] Disconnected from every router. Attempting to reconnect...")
	if mgr.TopUp(ctx) > 0 {
		return false, true
	}
	logger.Infof("[Monitor] No known router admitted this node. Re-reading the routers from the control plane...")
	if err := mgr.RefreshRouters(ctx); err != nil {
		logger.Warnf("[Monitor] Could not re-read the routers: %v", err)
		return false, false
	}
	if mgr.TopUp(ctx) > 0 {
		return false, true
	}
	return false, false
}
