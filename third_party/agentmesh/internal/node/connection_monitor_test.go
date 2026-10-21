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
	"testing"
)

// fakeAttacher scripts a node for the monitor: how many routers it holds,
// how many it wants, how many each top-up yields, and whether the control
// plane answers.
type fakeAttacher struct {
	attached   int
	want       int
	topUps     []int // what AttachedRouters reads after each TopUp, in order
	refreshErr error
	refreshed  bool
	topUpCalls int
}

func (f *fakeAttacher) AttachedRouters() int { return f.attached }
func (f *fakeAttacher) WantRouters() int     { return f.want }
func (f *fakeAttacher) TopUp(context.Context) int {
	if f.topUpCalls < len(f.topUps) {
		f.attached = f.topUps[f.topUpCalls]
	}
	f.topUpCalls++
	return f.attached
}
func (f *fakeAttacher) RefreshRouters(context.Context) error {
	f.refreshed = true
	return f.refreshErr
}

func TestCheckRouterConnection(t *testing.T) {
	cases := []struct {
		name          string
		mgr           *fakeAttacher
		wantStable    bool
		wantReconn    bool
		wantTopUps    int
		wantRefreshed bool
	}{
		{
			name:       "holds every router it wants",
			mgr:        &fakeAttacher{attached: 2, want: 2},
			wantStable: true,
		},
		{
			// On the mesh with one router short: the margin is restored,
			// and this is not a failure of the mesh.
			name:       "short of its count tops up and stays stable",
			mgr:        &fakeAttacher{attached: 1, want: 2, topUps: []int{2}},
			wantStable: true,
			wantTopUps: 1,
		},
		{
			name:       "no router, reattaches from what it knows",
			mgr:        &fakeAttacher{attached: 0, want: 2, topUps: []int{1}},
			wantReconn: true,
			wantTopUps: 1,
		},
		{
			name:          "no router, reattaches after re-reading the control plane",
			mgr:           &fakeAttacher{attached: 0, want: 2, topUps: []int{0, 2}},
			wantReconn:    true,
			wantTopUps:    2,
			wantRefreshed: true,
		},
		{
			name:          "no router, control plane unreachable",
			mgr:           &fakeAttacher{attached: 0, want: 2, topUps: []int{0}, refreshErr: errors.New("dial tcp: connection refused")},
			wantTopUps:    1,
			wantRefreshed: true,
		},
		{
			name:          "no router admits it even after re-reading",
			mgr:           &fakeAttacher{attached: 0, want: 2, topUps: []int{0, 0}},
			wantTopUps:    2,
			wantRefreshed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stable, reconnected := checkRouterConnection(context.Background(), tc.mgr)
			if stable != tc.wantStable {
				t.Errorf("stable = %v, want %v", stable, tc.wantStable)
			}
			if reconnected != tc.wantReconn {
				t.Errorf("reconnected = %v, want %v", reconnected, tc.wantReconn)
			}
			if tc.mgr.topUpCalls != tc.wantTopUps {
				t.Errorf("top-ups = %d, want %d", tc.mgr.topUpCalls, tc.wantTopUps)
			}
			if tc.mgr.refreshed != tc.wantRefreshed {
				t.Errorf("refreshed = %v, want %v", tc.mgr.refreshed, tc.wantRefreshed)
			}
		})
	}
}
