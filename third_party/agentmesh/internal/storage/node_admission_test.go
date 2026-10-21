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

package storage

import (
	"errors"
	"testing"
	"time"
)

func TestEnrolledNodeCheckAdmission(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		node EnrolledNode
		want error
	}{
		{
			name: "session still open",
			node: EnrolledNode{ExpiresAt: now.Add(time.Hour)},
		},
		{
			// Bootstrap enrollments carry no session, so a zero time must not
			// read as "expired at the epoch".
			name: "no session expiry recorded",
			node: EnrolledNode{},
		},
		{
			name: "banned",
			node: EnrolledNode{Banned: true, ExpiresAt: now.Add(time.Hour)},
			want: ErrNodeBanned,
		},
		{
			name: "session lapsed",
			node: EnrolledNode{ExpiresAt: now.Add(-time.Second)},
			want: ErrNodeSessionExpired,
		},
		{
			// A ban is the more actionable of the two, so it wins.
			name: "banned and lapsed",
			node: EnrolledNode{Banned: true, ExpiresAt: now.Add(-time.Hour)},
			want: ErrNodeBanned,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.node.CheckAdmission(now)
			if !errors.Is(err, tt.want) {
				t.Errorf("CheckAdmission() = %v, want %v", err, tt.want)
			}
		})
	}
}
