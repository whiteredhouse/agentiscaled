package node

import (
	"context"
	"encoding/base64"
	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/identity"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLocalA2ADiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, cleanup := startBareNode(t, ctx)
	defer cleanup()
	svc := newFakeSvc("helper", api.ServiceType_SERVICE_TYPE_A2A)
	svc.info.Description = "Local helper"
	n.services.insertService(svc)
	for _, name := range []string{"", "helper"} {
		rows, err := n.DiscoverRemoteServices(ctx, api.ServiceType_SERVICE_TYPE_A2A, name)
		if err != nil || len(rows) != 1 {
			t.Fatalf("discovery %q: %v, %v", name, rows, err)
		}
		if rows[0].PeerId != n.Host.ID().String() || rows[0].SrvName != "helper" || rows[0].SrvDescription != "Local helper" {
			t.Fatalf("provider = %+v", rows[0])
		}
		stream, err := n.DiscoverRemoteServicesStream(ctx, api.ServiceType_SERVICE_TYPE_A2A, name)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for row := range stream {
			if row.PeerId == n.Host.ID().String() {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("stream found local agent %d times", count)
		}
	}
}

func TestLocalA2AInvocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, cleanup := startBareNode(t, ctx)
	defer cleanup()
	enrollUnderRoot(t, n)
	calls := 0
	svc := newFakeSvc("helper", api.ServiceType_SERVICE_TYPE_A2A)
	svc.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get(api.HeaderPeerID) != n.Host.ID().String() {
			t.Errorf("unverified caller %q", r.Header.Get(api.HeaderPeerID))
		}
		if r.Header.Get(api.HeaderMeshAuthentication) != "" || r.Header.Get(api.HeaderMeshBiscuit) != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials reached backend")
		}
		if r.URL.Path != "/tasks" || r.URL.RawQuery != "x=1" {
			t.Errorf("upstream URL %s", r.URL)
		}
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write(b)
	})
	n.services.insertService(svc)
	proxy := createEgressProxy(n)
	target := "/mesh/" + n.Host.ID().String() + "/a2a/helper/tasks?x=1"
	request := func(labels string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", target, strings.NewReader("task"))
		req.Header.Set(api.HeaderPeerID, "forged")
		req.Header.Set(api.HeaderMeshAuthentication, "secret")
		req.Header.Set("Cookie", "secret")
		if labels != "" {
			req.Header.Set(api.HeaderMeshRequiredLabels, labels)
		}
		out := httptest.NewRecorder()
		proxy.ServeHTTP(out, req.WithContext(ctx))
		return out
	}
	out := request("")
	if out.Code != 200 || out.Body.String() != "task" || calls != 1 {
		t.Fatalf("local invocation: %d %s, calls %d", out.Code, out.Body, calls)
	}
	out = request("region=unattested")
	if out.Code != 403 || calls != 1 {
		t.Fatalf("label denial: %d, calls %d", out.Code, calls)
	}
	n.revokedPeers.Add(n.Host.ID().String(), time.Now().Unix())
	out = request("")
	if out.Code != 403 || calls != 1 {
		t.Fatalf("revocation: %d %s, calls %d", out.Code, out.Body, calls)
	}
}

func TestLocalProviderUsesStandingIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, cleanup := startBareNode(t, ctx)
	defer cleanup()
	enrollUnderRoot(t, n)
	// The caller's credential is checked at ingress; it must not replace the
	// provider's credential when checking whether this node may host a service.
	if err := n.VerifyProvider(WithCallerBiscuit(ctx, []byte("invalid caller")), n.Host.ID(), nil, "a2a://helper"); err != nil {
		t.Fatalf("provider checked caller credential: %v", err)
	}
}

func TestLocalA2AStreamingCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, cleanup := startBareNode(t, ctx)
	defer cleanup()
	enrollUnderRoot(t, n)
	stopped := make(chan struct{})
	svc := newFakeSvc("helper", api.ServiceType_SERVICE_TYPE_A2A)
	svc.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(stopped)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	n.services.insertService(svc)
	server := httptest.NewServer(createEgressProxy(n))
	defer server.Close()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/mesh/"+n.Host.ID().String()+"/a2a/helper/stream", nil)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(response.Body, b); err != nil {
		t.Fatal(err)
	}
	if string(b) != "data: first\n\n" {
		t.Fatalf("stream = %q", b)
	}
	response.Body.Close()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("closing response did not cancel local handler")
	}
}

func TestLocalA2AAuthorizationRefusals(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		publish, invoke, expired, badCaller, floor bool
	}{
		{name: "provider lacks publish", invoke: true},
		{name: "caller lacks invocation", publish: true},
		{name: "expired identity", publish: true, invoke: true, expired: true},
		{name: "invalid caller identity", publish: true, invoke: true, badCaller: true},
		{name: "operator label floor", publish: true, invoke: true, floor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			n, cleanup := startBareNode(t, ctx)
			defer cleanup()
			_, key := enrollUnderRoot(t, n)
			role := &api.PolicyRole{Name: api.RoleNode, AllowedTargets: []string{"*"}}
			if tc.publish {
				role.AllowedPublish = []string{"a2a://helper"}
			}
			if tc.invoke {
				role.AllowedServices = []string{"a2a://helper"}
			}
			expiry := time.Now().Add(time.Hour)
			if tc.expired {
				expiry = time.Now().Add(-time.Hour)
			}
			token, err := identity.MintBootstrapBiscuitToken(key, n.Host.ID(), api.RoleNode, expiry, []*api.PolicyRole{role}, nil)
			if err != nil {
				t.Fatal(err)
			}
			n.SetIdentityCache(token)
			if tc.floor {
				n.nodeConfig.EgressRequireLabels = map[string]string{"region": "eu"}
			}
			calls := 0
			svc := newFakeSvc("helper", api.ServiceType_SERVICE_TYPE_A2A)
			svc.handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ })
			n.services.insertService(svc)
			if tc.badCaller {
				ctx = WithCallerBiscuit(ctx, []byte("invalid caller"))
			}
			req := httptest.NewRequest("POST", "/mesh/"+n.Host.ID().String()+"/a2a/helper/", strings.NewReader("task"))
			out := httptest.NewRecorder()
			createEgressProxy(n).ServeHTTP(out, req.WithContext(ctx))
			if out.Code != 403 || calls != 0 {
				t.Fatalf("refusal = %d %s; backend calls %d", out.Code, out.Body, calls)
			}
		})
	}
}

func TestLocalProviderRejectsRevokedStandingToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, cleanup := startBareNode(t, ctx)
	defer cleanup()
	enrollUnderRoot(t, n)
	parsed, _, err := identity.UnmarshalInbound(n.GetIdentity())
	if err != nil {
		t.Fatal(err)
	}
	n.revokedTokens.Add(base64.RawURLEncoding.EncodeToString(parsed.RevocationIds()[0]), time.Now().Add(time.Hour))
	if err := n.VerifyProvider(ctx, n.Host.ID(), nil, "a2a://helper"); err == nil {
		t.Fatal("revoked provider token accepted")
	}
}

func TestLocalA2AEmptyRequestBody(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, cleanup := startBareNode(t, ctx)
	defer cleanup()
	enrollUnderRoot(t, n)
	svc := newFakeSvc("helper", api.ServiceType_SERVICE_TYPE_A2A)
	svc.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Error(err)
		}
		if err := r.Body.Close(); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	n.services.insertService(svc)
	req := httptest.NewRequest("DELETE", "/mesh/"+n.Host.ID().String()+"/a2a/helper/task", nil)
	req.Body = nil // http.Transport represents an empty outgoing request this way.
	out := httptest.NewRecorder()
	createEgressProxy(n).ServeHTTP(out, req.WithContext(ctx))
	if out.Code != http.StatusNoContent {
		t.Fatalf("empty request: %d %s", out.Code, out.Body)
	}
}
