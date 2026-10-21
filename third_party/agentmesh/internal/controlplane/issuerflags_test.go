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

package controlplane

import (
	"testing"

	"github.com/spf13/pflag"
)

// TestIssuerFlagsAccumulate is the Cloud Run case: --workload-issuer given
// once per platform must keep every platform, and the comma form must keep
// working. Before this helper a repeated flag replaced the previous value.
func TestIssuerFlagsAccumulate(t *testing.T) {
	cases := []struct {
		name         string
		args         []string
		wantLogin    string
		wantWorkload string
		wantIssuers  int
	}{
		{
			name:         "repeated",
			args:         []string{"--workload-issuer=http://spire.example", "--workload-issuer=https://bucket.example/substrate"},
			wantWorkload: "http://spire.example,https://bucket.example/substrate",
			wantIssuers:  2,
		},
		{
			name:         "comma-separated",
			args:         []string{"--workload-issuer=http://spire.example,https://bucket.example/substrate"},
			wantWorkload: "http://spire.example,https://bucket.example/substrate",
			wantIssuers:  2,
		},
		{
			name:         "both forms and a suffix entry",
			args:         []string{"--issuer=https://login.example", "--workload-issuer=http://spire.example", "--workload-issuer=https://accounts.google.com=.gserviceaccount.com,https://bucket.example/substrate"},
			wantLogin:    "https://login.example",
			wantWorkload: "http://spire.example,https://accounts.google.com=.gserviceaccount.com,https://bucket.example/substrate",
			wantIssuers:  2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f IssuerFlags
			fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
			f.Register(fs, "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("Parse(%v): %v", tc.args, err)
			}
			if got := f.OIDCIssuer(); got != tc.wantLogin {
				t.Fatalf("OIDCIssuer() = %q, want %q", got, tc.wantLogin)
			}
			if got := f.WorkloadIssuer(); got != tc.wantWorkload {
				t.Fatalf("WorkloadIssuer() = %q, want %q", got, tc.wantWorkload)
			}
			// The value feeds the same parser the server uses.
			issuers, suffixes := parseWorkloadIssuers(f.WorkloadIssuer())
			if len(issuers) != tc.wantIssuers {
				t.Fatalf("parseWorkloadIssuers kept %d plain issuers, want %d", len(issuers), tc.wantIssuers)
			}
			if tc.name == "both forms and a suffix entry" && len(suffixes["https://accounts.google.com"]) != 1 {
				t.Fatalf("suffix entry lost: %v", suffixes)
			}
		})
	}

	t.Run("empty", func(t *testing.T) {
		var f IssuerFlags
		fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
		f.Register(fs, "")
		if err := fs.Parse(nil); err != nil {
			t.Fatal(err)
		}
		if !f.Empty() || f.OIDCIssuer() != "" || f.WorkloadIssuer() != "" {
			t.Fatalf("no flags: Empty()=%v OIDCIssuer()=%q WorkloadIssuer()=%q, want empty", f.Empty(), f.OIDCIssuer(), f.WorkloadIssuer())
		}
	})
}
