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
	"strings"

	"github.com/spf13/pflag"
)

// IssuerFlags are the OIDC issuer settings a control plane binary takes on
// its command line. Both flags accumulate: given twice, or once with a
// comma-separated value, every issuer counts. A repeated flag that replaced
// the previous value dropped an issuer silently, and every member of the
// platform behind the dropped one was refused with "unknown issuer".
type IssuerFlags struct {
	Login    []string
	Workload []string
}

// Register adds --issuer and --workload-issuer to fs. optional names what
// an empty value means in the help text ("Optional " for agentmesh-one,
// "" for the control plane, where one of the two is required).
func (f *IssuerFlags) Register(fs *pflag.FlagSet, optional string) {
	fs.StringSliceVar(&f.Login, "issuer", nil, optional+"OIDC issuer URL; repeatable or comma-separated")
	fs.StringSliceVar(&f.Workload, "workload-issuer", nil, optional+"workload OIDC issuer URL or '<issuer>=<email-suffix>', repeatable or comma-separated; accepted at /register, /refresh and /token/exchange, refused at /user/* and /oauth/authorize")
}

// OIDCIssuer is the value for Options.OIDCIssuer.
func (f *IssuerFlags) OIDCIssuer() string { return strings.Join(f.Login, ",") }

// WorkloadIssuer is the value for Options.WorkloadIssuer.
func (f *IssuerFlags) WorkloadIssuer() string { return strings.Join(f.Workload, ",") }

// Empty reports whether no issuer of either kind was given.
func (f *IssuerFlags) Empty() bool { return len(f.Login) == 0 && len(f.Workload) == 0 }
