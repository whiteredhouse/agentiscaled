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

package credprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
)

func TestTARNarrowsNeverSelects_OIDCScopes(t *testing.T) {
	policyScopes := []string{
		"https://www.googleapis.com/auth/bigquery.readonly",
		"https://www.googleapis.com/auth/devstorage.read_only",
	}

	got, err := NarrowOIDCScopes(policyScopes, "bigquery.googleapis.com", nil)
	if err != nil {
		t.Fatalf("NarrowOIDCScopes(nil): %v", err)
	}
	if !slices.Equal(got, policyScopes) {
		t.Fatalf("expected %v, got %v", policyScopes, got)
	}

	tar1 := &api.TaskAuthorizationRule{
		Name: "tasks/bq-only",
		Rules: []*api.TaskRule{{
			AllowedServices: []string{"egress://bigquery.googleapis.com"},
			Operation: &api.TaskOperation{
				AllowedPermissions: []string{"https://www.googleapis.com/auth/bigquery.readonly"},
			},
		}},
	}
	got, err = NarrowOIDCScopes(policyScopes, "bigquery.googleapis.com", []*api.TaskAuthorizationRule{tar1})
	if err != nil {
		t.Fatalf("NarrowOIDCScopes(tar1): %v", err)
	}
	if !slices.Equal(got, []string{"https://www.googleapis.com/auth/bigquery.readonly"}) {
		t.Fatalf("expected narrowed scope, got %v", got)
	}

	tarEscalate := &api.TaskAuthorizationRule{
		Name: "tasks/escalate-cloud-platform",
		Rules: []*api.TaskRule{{
			AllowedServices: []string{"egress://bigquery.googleapis.com"},
			Operation: &api.TaskOperation{
				AllowedPermissions: []string{"https://www.googleapis.com/auth/cloud-platform"},
			},
		}},
	}
	if _, err := NarrowOIDCScopes(policyScopes, "bigquery.googleapis.com", []*api.TaskAuthorizationRule{tarEscalate}); err == nil {
		t.Fatal("expected NarrowOIDCScopes to reject TAR requesting scope outside policyScopes")
	}

	tarMixed := &api.TaskAuthorizationRule{
		Name: "tasks/mixed",
		Rules: []*api.TaskRule{{
			AllowedServices: []string{"egress://bigquery.googleapis.com"},
			Operation: &api.TaskOperation{
				AllowedPermissions: []string{
					"https://www.googleapis.com/auth/bigquery.readonly",
					"https://www.googleapis.com/auth/cloud-platform",
				},
			},
		}},
	}
	got, err = NarrowOIDCScopes(policyScopes, "bigquery.googleapis.com", []*api.TaskAuthorizationRule{tarMixed})
	if err != nil {
		t.Fatalf("NarrowOIDCScopes(tarMixed): %v", err)
	}
	if !slices.Equal(got, []string{"https://www.googleapis.com/auth/bigquery.readonly"}) {
		t.Fatalf("expected only policy-allowed scope, got %v", got)
	}

	got, err = NarrowOIDCScopes(nil, "bigquery.googleapis.com", []*api.TaskAuthorizationRule{tar1})
	if err != nil || len(got) != 0 {
		t.Fatalf("expected empty scopes when policyScopes is empty, got %v, err=%v", got, err)
	}

	hop1 := &api.TaskAuthorizationRule{
		Name: "tasks/session-bq-read-sales",
		Rules: []*api.TaskRule{{
			AllowedServices: []string{"egress://bigquery.googleapis.com"},
			Operation: &api.TaskOperation{
				AllowedMethods: []string{"GET", "POST"},
				AllowedPaths:   []string{"/bigquery/v2/projects/my-proj/datasets/sales_2026/*"},
				AllowedPermissions: []string{
					"bigquery.googleapis.com/datasets.get",
					"bigquery.googleapis.com/tables.get",
					"bigquery.googleapis.com/tables.getData",
					"bigquery.googleapis.com/jobs.create",
				},
			},
			AllowedResources: []string{"//bigquery.googleapis.com/projects/my-proj/datasets/sales_2026/*"},
		}},
	}
	hop2 := &api.TaskAuthorizationRule{
		Name: "tasks/subagent-q1-only",
		Rules: []*api.TaskRule{{
			AllowedServices: []string{"egress://bigquery.googleapis.com"},
			Operation: &api.TaskOperation{
				AllowedMethods:     []string{"GET"},
				AllowedPaths:       []string{"/bigquery/v2/projects/my-proj/datasets/sales_2026/tables/q1/*"},
				AllowedPermissions: []string{"bigquery.googleapis.com/tables.getData"},
			},
			AllowedResources: []string{"//bigquery.googleapis.com/projects/my-proj/datasets/sales_2026/tables/q1"},
		}},
	}
	got, err = NarrowOIDCScopes([]string{"https://www.googleapis.com/auth/bigquery.readonly"}, "bigquery.googleapis.com", []*api.TaskAuthorizationRule{hop1, hop2})
	if err != nil || !slices.Equal(got, []string{"https://www.googleapis.com/auth/bigquery.readonly"}) {
		t.Fatalf("expected bigquery.readonly scope preserved, got %v, err=%v", got, err)
	}
	perms, resources, err := IntersectTaskPermissionsAndResources("bigquery.googleapis.com", []*api.TaskAuthorizationRule{hop1, hop2})
	if err != nil {
		t.Fatalf("IntersectTaskPermissionsAndResources: %v", err)
	}
	if !slices.Equal(perms, []string{"bigquery.googleapis.com/tables.getData"}) {
		t.Fatalf("expected intersected perms [bigquery.googleapis.com/tables.getData], got %v", perms)
	}
	if !slices.Equal(resources, []string{"//bigquery.googleapis.com/projects/my-proj/datasets/sales_2026/tables/q1"}) {
		t.Fatalf("expected intersected resources [../tables/q1], got %v", resources)
	}

	emptyTAR := &api.TaskAuthorizationRule{Name: "tasks/empty-fail-closed"}
	if _, err := NarrowOIDCScopes(policyScopes, "bigquery.googleapis.com", []*api.TaskAuthorizationRule{emptyTAR}); err == nil {
		t.Fatal("expected NarrowOIDCScopes to reject TAR with empty rules list")
	}
	if _, err := NarrowOIDCScopes(nil, "bigquery.googleapis.com", []*api.TaskAuthorizationRule{emptyTAR}); err == nil {
		t.Fatal("expected NarrowOIDCScopes(nil scopes) to reject TAR with empty rules list")
	}
	otherSvcTAR := &api.TaskAuthorizationRule{
		Name: "tasks/storage-only",
		Rules: []*api.TaskRule{{
			AllowedServices: []string{"egress://storage.googleapis.com"},
		}},
	}
	if _, err := NarrowOIDCScopes(policyScopes, "bigquery.googleapis.com", []*api.TaskAuthorizationRule{otherSvcTAR}); err == nil {
		t.Fatal("expected NarrowOIDCScopes to reject TAR targeting a different service")
	}
}

func TestTARNarrowsNeverSelects_AWSSessionPolicy(t *testing.T) {
	template := `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": ["s3:GetObject", "s3:ListBucket"],
			"Resource": ["arn:aws:s3:::acme-analytics/*"]
		}]
	}`

	hop1 := &api.TaskAuthorizationRule{
		Name: "tasks/s3-read",
		Rules: []*api.TaskRule{{
			AllowedServices: []string{"egress://s3.amazonaws.com"},
			Operation: &api.TaskOperation{
				AllowedPermissions: []string{"s3:GetObject", "s3:ListBucket"},
			},
			AllowedResources: []string{"arn:aws:s3:::acme-analytics/2026/*"},
		}},
	}
	hop2 := &api.TaskAuthorizationRule{
		Name: "tasks/s3-q1-only",
		Rules: []*api.TaskRule{{
			AllowedServices: []string{"egress://s3.amazonaws.com"},
			Operation: &api.TaskOperation{
				AllowedPermissions: []string{"s3:GetObject"},
			},
			AllowedResources: []string{"arn:aws:s3:::acme-analytics/2026/q1.parquet"},
		}},
	}
	compiled, err := CompileAWSSessionPolicy(template, "s3.amazonaws.com", []*api.TaskAuthorizationRule{hop1, hop2})
	if err != nil {
		t.Fatalf("CompileAWSSessionPolicy: %v", err)
	}
	if !strings.Contains(compiled, `"s3:GetObject"`) || strings.Contains(compiled, `"s3:ListBucket"`) {
		t.Fatalf("expected only s3:GetObject in compiled policy: %s", compiled)
	}
	if !strings.Contains(compiled, `"arn:aws:s3:::acme-analytics/2026/q1.parquet"`) {
		t.Fatalf("expected narrowed resource in compiled policy: %s", compiled)
	}

	escalateAction := &api.TaskAuthorizationRule{
		Name: "tasks/s3-delete",
		Rules: []*api.TaskRule{{
			AllowedServices: []string{"egress://s3.amazonaws.com"},
			Operation: &api.TaskOperation{
				AllowedPermissions: []string{"s3:DeleteObject"},
			},
		}},
	}
	if _, err := CompileAWSSessionPolicy(template, "s3.amazonaws.com", []*api.TaskAuthorizationRule{escalateAction}); err == nil {
		t.Fatal("expected CompileAWSSessionPolicy to reject Action outside template")
	}

	escalateRes := &api.TaskAuthorizationRule{
		Name: "tasks/s3-other-bucket",
		Rules: []*api.TaskRule{{
			AllowedServices:  []string{"egress://s3.amazonaws.com"},
			AllowedResources: []string{"arn:aws:s3:::payroll-secrets/*"},
		}},
	}
	if _, err := CompileAWSSessionPolicy(template, "s3.amazonaws.com", []*api.TaskAuthorizationRule{escalateRes}); err == nil {
		t.Fatal("expected CompileAWSSessionPolicy to reject Resource outside template")
	}
}

func TestOIDCFederationAndAWSExchangers(t *testing.T) {
	var stsCalls atomic.Int32
	var iamCalls atomic.Int32
	mockSTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ":generateAccessToken") {
			iamCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer federated-sts-token" {
				http.Error(w, "unexpected federated token", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accessToken": "impersonated-sa-token",
				"expireTime":  time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
			})
			return
		}
		stsCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.FormValue("subject_token") != "cp-minted-es256-jwt" {
			http.Error(w, "unexpected subject_token", http.StatusBadRequest)
			return
		}
		if r.FormValue("audience") != "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/agentmesh/providers/agentmesh-cp" {
			http.Error(w, "unexpected audience", http.StatusBadRequest)
			return
		}
		if r.FormValue("scope") != "https://www.googleapis.com/auth/bigquery.readonly" {
			http.Error(w, "unexpected scope: "+r.FormValue("scope"), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "federated-sts-token",
			"expires_in":   300,
		})
	}))
	defer mockSTS.Close()

	var mintCalls atomic.Int32
	mintFn := func(_ context.Context, destination, audience string) (string, time.Time, error) {
		mintCalls.Add(1)
		return "cp-minted-es256-jwt", time.Now().Add(5 * time.Minute), nil
	}

	ex := NewOIDCFederationExchanger("bigquery.googleapis.com", &api.OIDCFederation{
		TokenEndpoint: mockSTS.URL + "/v1/token",
		Audience:      "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/agentmesh/providers/agentmesh-cp",
		Impersonate:   "bq-reader@my-proj.iam.gserviceaccount.com",
		Scopes: []string{
			"https://www.googleapis.com/auth/bigquery.readonly",
			"https://www.googleapis.com/auth/devstorage.read_only",
		},
	}, mintFn, mockSTS.Client())
	ex.iamCredentialsEndpoint = mockSTS.URL

	tar := &api.TaskAuthorizationRule{
		Name: "tasks/bq-only",
		Rules: []*api.TaskRule{{
			AllowedServices: []string{"egress://bigquery.googleapis.com"},
			Operation: &api.TaskOperation{
				AllowedPermissions: []string{"https://www.googleapis.com/auth/bigquery.readonly"},
			},
		}},
	}
	ctx := WithCallerBiscuit(context.Background(), []byte("caller-biscuit-bytes"))
	tok, exp, err := ex.Exchange(ctx, "alice@example.com", []*api.TaskAuthorizationRule{tar})
	if err != nil {
		t.Fatalf("OIDCFederationExchanger.Exchange: %v", err)
	}
	if tok != "impersonated-sa-token" || exp.IsZero() {
		t.Fatalf("unexpected token=%q exp=%v", tok, exp)
	}
	tok2, _, err := ex.Exchange(ctx, "alice@example.com", []*api.TaskAuthorizationRule{tar})
	if err != nil || tok2 != "impersonated-sa-token" {
		t.Fatalf("cached Exchange failed: %v", err)
	}
	if mintCalls.Load() != 1 || stsCalls.Load() != 1 || iamCalls.Load() != 1 {
		t.Fatalf("expected 1 mint/sts/iam call with cache hit, got mint=%d sts=%d iam=%d", mintCalls.Load(), stsCalls.Load(), iamCalls.Load())
	}

	mockAWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("Action") != "AssumeRoleWithWebIdentity" || r.FormValue("RoleArn") != "arn:aws:iam::123456789012:role/agentmesh-reader" {
			http.Error(w, "invalid AWS request", http.StatusBadRequest)
			return
		}
		if !strings.Contains(r.FormValue("Policy"), `"s3:GetObject"`) {
			http.Error(w, "missing compiled session policy", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>ASIA123</AccessKeyId><SecretAccessKey>secret</SecretAccessKey><SessionToken>aws-downscoped-session-token</SessionToken><Expiration>` + time.Now().Add(15*time.Minute).UTC().Format(time.RFC3339) + `</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`))
	}))
	defer mockAWS.Close()

	awsEx := NewAWSAssumeRoleExchanger("s3.amazonaws.com", &api.AWSAssumeRole{
		RoleArn: "arn:aws:iam::123456789012:role/agentmesh-reader",
	}, mintFn, mockAWS.Client())
	awsEx.stsEndpoint = mockAWS.URL

	awsTAR := &api.TaskAuthorizationRule{
		Name: "tasks/s3-get",
		Rules: []*api.TaskRule{{
			AllowedServices:  []string{"egress://s3.amazonaws.com"},
			Operation:        &api.TaskOperation{AllowedPermissions: []string{"s3:GetObject"}},
			AllowedResources: []string{"arn:aws:s3:::my-bucket/data.csv"},
		}},
	}
	awsTok, _, err := awsEx.Exchange(ctx, "alice@example.com", []*api.TaskAuthorizationRule{awsTAR})
	if err != nil || awsTok != "aws-downscoped-session-token" {
		t.Fatalf("AWSAssumeRoleExchanger.Exchange: tok=%q err=%v", awsTok, err)
	}
}

func TestCompileAWSSessionPolicySingleStatementObject(t *testing.T) {
	singleStmtTemplate := `{
		"Version": "2012-10-17",
		"Statement": {
			"Effect": "Allow",
			"Action": ["s3:GetObject", "s3:PutObject"],
			"Resource": "arn:aws:s3:::corp-bucket/*"
		}
	}`
	tar := &api.TaskAuthorizationRule{
		Name: "read-only-s3",
		Rules: []*api.TaskRule{
			{
				AllowedServices:  []string{"egress://s3.amazonaws.com"},
				AllowedResources: []string{"arn:aws:s3:::corp-bucket/reports/*"},
				Operation: &api.TaskOperation{
					AllowedPermissions: []string{"s3:GetObject"},
				},
			},
		},
	}
	compiled, err := CompileAWSSessionPolicy(singleStmtTemplate, "s3.amazonaws.com", []*api.TaskAuthorizationRule{tar})
	if err != nil {
		t.Fatalf("CompileAWSSessionPolicy with single Statement object failed: %v", err)
	}
	if !strings.Contains(compiled, "s3:GetObject") || strings.Contains(compiled, "s3:PutObject") {
		t.Fatalf("unexpected compiled policy actions: %s", compiled)
	}
	if !strings.Contains(compiled, "arn:aws:s3:::corp-bucket/reports/*") {
		t.Fatalf("unexpected compiled policy resources: %s", compiled)
	}
}

func TestCompileAWSSessionPolicyValidationAndBrokerCacheKey(t *testing.T) {
	badActionTAR := &api.TaskAuthorizationRule{
		Name: "tasks/bad-action",
		Rules: []*api.TaskRule{{
			AllowedServices: []string{"egress://s3.amazonaws.com"},
			Operation:       &api.TaskOperation{AllowedPermissions: []string{"s3:GetObject invalid"}},
		}},
	}
	if _, err := CompileAWSSessionPolicy("", "s3.amazonaws.com", []*api.TaskAuthorizationRule{badActionTAR}); err == nil {
		t.Fatal("expected CompileAWSSessionPolicy to reject invalid AWS action")
	}

	badResTAR := &api.TaskAuthorizationRule{
		Name: "tasks/bad-arn",
		Rules: []*api.TaskRule{{
			AllowedServices:  []string{"egress://s3.amazonaws.com"},
			AllowedResources: []string{"not-an-arn"},
		}},
	}
	if _, err := CompileAWSSessionPolicy("", "s3.amazonaws.com", []*api.TaskAuthorizationRule{badResTAR}); err == nil {
		t.Fatal("expected CompileAWSSessionPolicy to reject non-ARN resource")
	}

	staticEx := NewStaticSecretExchanger(t.TempDir(), "../etc/passwd")
	if _, _, err := staticEx.Exchange(context.Background(), "alice", nil); err == nil {
		t.Fatal("expected StaticSecretExchanger to reject path traversal secret name")
	}

	ctx := WithCallerBiscuit(context.Background(), []byte("same-biscuit"))
	tarA := &api.TaskAuthorizationRule{
		Name: "tasks/same-name",
		Rules: []*api.TaskRule{{
			AllowedServices:  []string{"egress://s3.amazonaws.com"},
			AllowedResources: []string{"arn:aws:s3:::bucket-a/*"},
		}},
	}
	tarB := &api.TaskAuthorizationRule{
		Name: "tasks/same-name",
		Rules: []*api.TaskRule{{
			AllowedServices:  []string{"egress://s3.amazonaws.com"},
			AllowedResources: []string{"arn:aws:s3:::bucket-b/*"},
		}},
	}
	keyA := brokerCacheKey(ctx, "aws", "s3.amazonaws.com", "arn:aws:iam::123456789012:role/r", "alice", nil, []string{"tasks/same-name"}, []*api.TaskAuthorizationRule{tarA})
	keyB := brokerCacheKey(ctx, "aws", "s3.amazonaws.com", "arn:aws:iam::123456789012:role/r", "alice", nil, []string{"tasks/same-name"}, []*api.TaskAuthorizationRule{tarB})
	if keyA == keyB {
		t.Fatalf("expected distinct brokerCacheKey for different TAR rules, both got %q", keyA)
	}
}
