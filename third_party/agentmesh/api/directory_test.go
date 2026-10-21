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

package api

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
)

func TestDirectorySettingsFrom(t *testing.T) {
	def := DefaultDirectorySettings()
	for _, tc := range []struct {
		name string
		in   *DirectoryParameters
		want DirectorySettings
	}{
		{name: "unset", in: nil, want: def},
		{name: "empty", in: &DirectoryParameters{}, want: def},
		{name: "published", in: NewDirectoryParameters(5, 20*time.Minute), want: DirectorySettings{Replication: 5, Reprovide: 20 * time.Minute, RecordTTL: time.Hour}},
		{name: "ttl not derived", in: &DirectoryParameters{Replication: 2, ReprovideInterval: durationpb.New(10 * time.Minute), RecordTtl: durationpb.New(2 * time.Hour)}, want: DirectorySettings{Replication: 2, Reprovide: 10 * time.Minute, RecordTTL: 2 * time.Hour}},
		{name: "ttl not longer than the interval is replaced", in: &DirectoryParameters{ReprovideInterval: durationpb.New(10 * time.Minute), RecordTtl: durationpb.New(10 * time.Minute)}, want: DirectorySettings{Replication: def.Replication, Reprovide: 10 * time.Minute, RecordTTL: 30 * time.Minute}},
		{name: "zero and negative keep the defaults", in: &DirectoryParameters{Replication: -1, ReprovideInterval: durationpb.New(-time.Minute)}, want: def},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DirectorySettingsFrom(tc.in); got != tc.want {
				t.Fatalf("DirectorySettingsFrom(%v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateDirectoryParameters(t *testing.T) {
	for _, tc := range []struct {
		replication int
		reprovide   time.Duration
		wantErr     bool
	}{
		{DefaultDHTReplication, DefaultReprovideInterval, false},
		{1, time.Second, false},
		{0, DefaultReprovideInterval, true},
		{DefaultDHTReplication, 0, true},
		{DefaultDHTReplication, -time.Minute, true},
	} {
		if err := ValidateDirectoryParameters(tc.replication, tc.reprovide); (err != nil) != tc.wantErr {
			t.Errorf("ValidateDirectoryParameters(%d, %s) = %v, want error %v", tc.replication, tc.reprovide, err, tc.wantErr)
		}
	}
}
