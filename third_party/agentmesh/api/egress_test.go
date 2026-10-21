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
	"slices"
	"strings"
	"testing"

	"github.com/eclipse-biscuit/biscuit-go/v2"
)

func TestEgressServiceType(t *testing.T) {
	got, err := ParseServiceType("egress")
	if err != nil || got != ServiceType_SERVICE_TYPE_EGRESS {
		t.Fatalf("ParseServiceType(egress) = %v, %v", got, err)
	}
	s, err := ServiceTypeToString(ServiceType_SERVICE_TYPE_EGRESS)
	if err != nil || s != ServiceTypeStringEgress {
		t.Fatalf("ServiceTypeToString = %q, %v", s, err)
	}
	for _, svc := range []string{"egress://api.github.com", "egress://*.internal.example.com", "egress://*"} {
		if err := ValidateServiceFormat(svc); err != nil {
			t.Errorf("ValidateServiceFormat(%q): %v", svc, err)
		}
	}
	// The grant compiler needs no egress-specific case.
	if f := BuildServiceDatalogFact("egress://*.internal.example.com"); f.Name != FactGrantedServiceSuffix || f.IDs[1] != biscuit.String(".internal.example.com") {
		t.Errorf("suffix grant = %v", f)
	}
}

// TestValidateEgressServicePattern: a grant is a hostname pattern. The forms
// the generic validator lets through, a path or uppercase, would compile to a
// grant that matches no destination, so they are refused here.
func TestValidateEgressServicePattern(t *testing.T) {
	for _, ok := range []string{
		"egress://api.github.com",
		"egress://*.internal.example.com",
		"egress://api.*",
		"egress://*",
		"*",
		// Other types keep their own rules.
		"mcp://Calc/add",
	} {
		if err := ValidateEgressServicePattern(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"egress://api.github.com/v3",
		"egress://api.github.com:443",
		"egress://https://api.github.com",
		"egress://API.github.com",
		"egress://api.github.com.",
		"egress://a*.example.com",
		"egress://",
	} {
		if err := ValidateEgressServicePattern(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidateEgressDestination(t *testing.T) {
	roles := map[string]bool{"pep": true}
	tests := []struct {
		name    string
		d       *EgressDestination
		wantErr string
	}{
		{"valid by role", &EgressDestination{Name: "api.github.com", Credential: "github-eu", ServedBy: []string{"pep"}}, ""},
		{"valid by label with target_url", &EgressDestination{Name: "mam.internal.example.com", TargetUrl: "http://mam.internal.example.com:8080", ServedBy: []string{"site=dc1"}}, ""},
		{"no name", &EgressDestination{ServedBy: []string{"pep"}}, "no name"},
		{"uppercase", &EgressDestination{Name: "API.github.com", ServedBy: []string{"pep"}}, "lowercase"},
		{"wildcard", &EgressDestination{Name: "*.github.com", ServedBy: []string{"pep"}}, "one hostname"},
		{"port", &EgressDestination{Name: "api.github.com:443", ServedBy: []string{"pep"}}, "one hostname"},
		{"path", &EgressDestination{Name: "api.github.com/v3", ServedBy: []string{"pep"}}, "one hostname"},
		{"target_url with credential", &EgressDestination{Name: "api.github.com", TargetUrl: "https://:tok@api.github.com", ServedBy: []string{"pep"}}, "must not carry a credential"},
		{"target_url scheme", &EgressDestination{Name: "api.github.com", TargetUrl: "ftp://api.github.com", ServedBy: []string{"pep"}}, "http or https"},
		{"target_url empty hostname", &EgressDestination{Name: "api.github.com", TargetUrl: "https://:443", ServedBy: []string{"pep"}}, "no host"},
		{"credential is a path", &EgressDestination{Name: "api.github.com", Credential: "/etc/passwd", ServedBy: []string{"pep"}}, "not a path"},
		{"no served_by", &EgressDestination{Name: "api.github.com"}, "served_by"},
		{"unknown role", &EgressDestination{Name: "api.github.com", ServedBy: []string{"nobody"}}, "neither a role"},
		{"bad label value", &EgressDestination{Name: "api.github.com", ServedBy: []string{"site="}}, "label value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEgressDestination(tt.d, roles)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestEgressServedBy(t *testing.T) {
	d := &EgressDestination{Name: "api.github.com", ServedBy: []string{"pep", "site=eu"}}
	if !EgressServedBy(d, []string{"mesh:role:node", "pep"}, nil) {
		t.Error("role match missed")
	}
	if !EgressServedBy(d, []string{"mesh:role:node"}, map[string]string{"site": "eu"}) {
		t.Error("label match missed")
	}
	if EgressServedBy(d, []string{"mesh:role:node"}, map[string]string{"site": "us"}) {
		t.Error("matched a node it does not select")
	}
	if EgressTargetURL(d) != "https://api.github.com" {
		t.Errorf("default target = %q", EgressTargetURL(d))
	}
	d.TargetUrl = "http://localhost:9"
	if EgressTargetURL(d) != "http://localhost:9" {
		t.Errorf("explicit target = %q", EgressTargetURL(d))
	}
}

func TestBuildEgressServingRules(t *testing.T) {
	rules := BuildEgressServingRules([]*EgressDestination{
		{Name: "api.github.com", ServedBy: []string{"pep", "site=eu", `bad"role`, `site=bad"val`}},
		nil,
		{Name: "", ServedBy: []string{"pep"}},
		{Name: `bad"name.example.com`, ServedBy: []string{"pep"}},
	})
	texts := PolicyRuleTexts(rules)
	want := []string{
		`granted_service_exact("egress", "api.github.com") <- role("pep")`,
		`granted_publish_exact("egress", "api.github.com") <- role("pep")`,
		`granted_service_exact("egress", "api.github.com") <- label("site", "eu")`,
		`granted_publish_exact("egress", "api.github.com") <- label("site", "eu")`,
	}
	if !slices.Equal(texts, want) {
		t.Errorf("rules = %v, want %v", texts, want)
	}
	if _, err := ParseDatalogRules(texts); err != nil {
		t.Fatalf("rendered rules do not parse back: %v", err)
	}
}

// served_by is who serves a destination, so the nodes it selects get the
// publish grant for egress://<name> with their roles, and nobody else.
func TestWithEgressPublish(t *testing.T) {
	policyRoles := []*PolicyRole{{Name: "node", AllowedPublish: []string{"mcp://calc"}}, {Name: "other"}}
	egress := []*EgressDestination{
		{Name: "api.github.com", ServedBy: []string{"node"}},
		{Name: "mam.internal.example.com", ServedBy: []string{"site=eu"}},
		{Name: "nobody.example", ServedBy: []string{"pep"}},
	}
	got := WithEgressPublish(policyRoles, egress, []string{"node"}, map[string]string{"site": "eu"})
	var node *PolicyRole
	for _, r := range got {
		if r.GetName() == "node" {
			node = r
		}
	}
	want := []string{"mcp://calc", "egress://api.github.com", "egress://mam.internal.example.com"}
	if node == nil || !slices.Equal(node.AllowedPublish, want) {
		t.Fatalf("node role publishes %v, want %v", node.GetAllowedPublish(), want)
	}
	if slices.Equal(policyRoles[0].AllowedPublish, want) {
		t.Fatal("the policy's own role entry was changed in place")
	}
	if len(WithEgressPublish(policyRoles, egress, []string{"other"}, nil)) != len(policyRoles) || len(WithEgressPublish(policyRoles, egress, []string{"other"}, nil)[1].AllowedPublish) != 0 {
		t.Fatal("a role no destination selects got a grant")
	}
	// A role the policy does not define gets an entry with the grant alone.
	got = WithEgressPublish(nil, egress, []string{"mesh:role:node"}, map[string]string{"site": "eu"})
	if len(got) != 1 || got[0].GetName() != "mesh:role:node" || !slices.Equal(got[0].AllowedPublish, []string{"egress://mam.internal.example.com"}) {
		t.Fatalf("undefined role: got %v", got)
	}
}
