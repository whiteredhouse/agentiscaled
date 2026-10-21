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

package version

import (
	"runtime/debug"
	"testing"
)

func TestFromBuildInfo(t *testing.T) {
	for _, test := range []struct {
		name string
		info debug.BuildInfo
		want string
	}{
		{name: "unknown", want: "devel"},
		{name: "module", info: debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, want: "v0.1.0"},
		{name: "development", info: debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, want: "devel"},
		{name: "revision", info: debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef"}}}, want: "devel-0123456789ab"},
		{name: "dirty", info: debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef"}, {Key: "vcs.modified", Value: "true"}}}, want: "devel-0123456789ab-dirty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := fromBuildInfo(&test.info); got != test.want {
				t.Fatalf("version = %q, want %q", got, test.want)
			}
		})
	}
}

func TestString(t *testing.T) {
	previous := Version
	t.Cleanup(func() { Version = previous })
	Version = "v0.2.0-test"
	if got := String(); got != Version {
		t.Fatalf("version = %q, want linker override %q", got, Version)
	}
	Version = "devel"
	if String() == "" {
		t.Fatal("development version must not be empty")
	}
}
