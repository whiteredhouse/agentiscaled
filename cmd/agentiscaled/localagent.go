package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/eclipse-biscuit/biscuit-go/v2"
	"github.com/eclipse-biscuit/biscuit-go/v2/datalog"
	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
)

const localAgentSocketName = "agent.sock"

// localAgentInfo is what the local agent API says about the node: its peer
// ID and the names of the agents it serves.
type localAgentInfo struct {
	PeerID   string   `json:"peer_id"`
	Services []string `json:"services"`
}

type agentSocketContextKey struct{}

// serveLocalAgent is agentiscaled's owner API, separate from Agent Mesh's
// network routes. Both paths proxy to the same agent handler and task store.
// targets maps each agent's name to where serveAgent serves it.
func serveLocalAgent(ctx context.Context, node *meshnode.AgentMeshNode, cfg *meshnode.NodeConfigComplete, targets map[string]string, dataDir string) (*http.Server, error) {
	proxies := map[string]*httputil.ReverseProxy{}
	for name, target := range targets {
		backend, err := url.Parse(target)
		if err != nil || backend == nil || backend.User == nil {
			return nil, fmt.Errorf("invalid local agent backend for %s", name)
		}
		token, ok := backend.User.Password()
		if !ok || token == "" {
			return nil, fmt.Errorf("local agent backend for %s has no credential", name)
		}
		backend.User = nil
		proxies[name] = &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(backend)
			pr.Out.Header.Set("Authorization", "Bearer "+token)
		}}
	}
	names := slices.Sorted(maps.Keys(proxies))
	authorization := socketClient(filepath.Join(dataDir, meshnode.DefaultSocketName))
	authorization.Timeout = 5 * time.Second
	authorization.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	go func() { <-ctx.Done(); authorization.CloseIdleConnections() }()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if local, _ := r.Context().Value(agentSocketContextKey{}).(bool); !local {
			http.Error(w, "Unix socket required", http.StatusForbidden)
			return
		}
		info := localAgentInfo{PeerID: node.Host.ID().String(), Services: names}
		if r.Method == http.MethodGet && r.URL.Path == "/info" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(info)
			return
		}
		// /a2a/<peer>/<name>[/.well-known/agent-card.json]
		rest, ok := strings.CutPrefix(r.URL.Path, "/a2a/"+info.PeerID+"/")
		name, sub, _ := strings.Cut(rest, "/")
		proxy := proxies[name]
		backendPath, allowedMethod := "/", http.MethodPost
		switch {
		case !ok || proxy == nil:
			http.NotFound(w, r)
			return
		case sub == "":
		case sub == ".well-known/agent-card.json":
			backendPath, allowedMethod = "/.well-known/agent-card.json", http.MethodGet
		default:
			http.NotFound(w, r)
			return
		}
		if r.Method != allowedMethod {
			w.Header().Set("Allow", allowedMethod)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// A hosted agent's call carries its task's token (taskNetwork),
		// which the node checks in place of its own credential.
		var callerToken string
		if v := r.Header.Get(api.HeaderMeshAuthentication); v != "" {
			bearer, ok := strings.CutPrefix(v, "Bearer ")
			if !ok || strings.TrimSpace(bearer) == "" {
				http.Error(w, "local agent authorization failed", http.StatusForbidden)
				return
			}
			callerToken = strings.TrimSpace(bearer)
		}
		if err := authorizeLocalAgent(r.Context(), node, cfg, name, r.Method, backendPath, authorization, callerToken); err != nil {
			log.Printf("local agent %s refused: %v", name, err)
			http.Error(w, "local agent authorization failed", http.StatusForbidden)
			return
		}
		// Socket permissions establish the owner; caller-controlled peer/biscuit
		// headers are never an identity source for this API.
		req := r.Clone(r.Context())
		for key := range req.Header {
			lower := strings.ToLower(key)
			if lower == "authorization" || lower == "cookie" || lower == strings.ToLower(api.HeaderPeerID) || strings.HasPrefix(lower, "x-mesh-") {
				req.Header.Del(key)
			}
		}
		req.Header.Set(api.HeaderPeerID, info.PeerID)
		req.URL.Path = backendPath
		req.URL.RawPath = ""
		proxy.ServeHTTP(w, req)
	})
	listener, err := listenAgentSocket(filepath.Join(dataDir, localAgentSocketName))
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, ConnContext: func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, agentSocketContextKey{}, c.LocalAddr().Network() == "unix")
	}}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("local agent API: %v", err)
		}
	}()
	go func() { <-ctx.Done(); _ = server.Close() }()
	return server, nil
}

// authorizeLocalAgent checks the provider with public policy primitives, then
// asks the existing Mesh sidecar to evaluate invocation policy. No self-dial or
// internal Mesh types are needed. The request is built from trusted inputs only,
// and the caller's credential is the node's own unless callerToken, a task's
// token, narrows it: the sidecar then refuses a token that is revoked, expired
// or does not allow the call.
func authorizeLocalAgent(ctx context.Context, node *meshnode.AgentMeshNode, cfg *meshnode.NodeConfigComplete, name, method, path string, authorization *http.Client, callerToken string) error {
	token := node.GetIdentity()
	claims, err := node.VerifyLocalBiscuit(token)
	if err != nil {
		return err
	}
	if claims.NodePeerID != node.Host.ID().String() || !slices.Contains(claims.Roles, api.RoleNode) {
		return fmt.Errorf("local credential is not this node's identity")
	}
	// Bound this adapter's evaluation as well as the sidecar request. Mesh's
	// verifier above validates the credential and its attenuation blocks first.
	timeout := node.BiscuitTimeout
	if timeout <= 0 {
		timeout = time.Second
	}
	newAuthorizer := func() (biscuit.Authorizer, error) {
		return claims.Biscuit.Authorizer(claims.VerifyingKey, biscuit.WithWorldOptions(
			datalog.WithMaxDuration(timeout), datalog.WithMaxFacts(1000), datalog.WithMaxIterations(100),
		))
	}
	auth, err := newAuthorizer()
	if err != nil {
		return err
	}
	auth.AddFact(api.PublishFact(api.ServiceTypeStringA2A, name))
	node.MeshPolicyMu.RLock()
	for _, rule := range node.MeshPolicyRules {
		auth.AddRule(rule)
	}
	node.MeshPolicyMu.RUnlock()
	for _, policy := range api.PublishPolicies {
		auth.AddPolicy(policy)
	}
	if err := auth.Authorize(); err != nil {
		return fmt.Errorf("local provider lacks publish permission: %w", err)
	}
	if cfg != nil && len(cfg.EgressRequireLabels) > 0 {
		check, err := api.LabelCheck(cfg.EgressRequireLabels)
		if err != nil {
			return err
		}
		// The floor checks credential-attested labels, separately from rules
		// that the current mesh policy may derive for publish permission.
		labels, err := newAuthorizer()
		if err != nil {
			return err
		}
		labels.AddCheck(check)
		labels.AddPolicy(api.AllowIfTruePolicy)
		if err := labels.Authorize(); err != nil {
			return fmt.Errorf("local provider does not meet the configured label floor: %w", err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://agentmesh/ext_authz"+path, nil)
	if err != nil {
		return err
	}
	credential := base64.StdEncoding.EncodeToString(token)
	if callerToken != "" {
		credential = callerToken
	}
	req.Header.Set(api.HeaderMeshBiscuit, credential)
	req.Header.Set(api.HeaderMeshTargetService, "a2a://"+name)
	response, err := authorization.Do(req)
	if err != nil {
		return fmt.Errorf("local invocation authorization unavailable: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("local invocation authorization refused: HTTP %d", response.StatusCode)
	}
	return nil
}

func listenAgentSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			conn.Close()
			return nil, fmt.Errorf("another agent API is listening on %s", path)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("check existing agent socket: %w", err)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

// duplicateAgentChecks are the delays before each look for another node
// serving the same agent name; the first look may run before the mesh has
// heard from other nodes.
var duplicateAgentChecks = []time.Duration{15 * time.Second, 2 * time.Minute}

// warnDuplicateAgent logs once when another node on the network also serves
// an agent named name, such as a separate agent node left from an earlier
// setup. Callers then reach whichever node discovery returns first.
func warnDuplicateAgent(ctx context.Context, self, name string, discover func(context.Context) ([]*api.DiscoveredProvider, error)) {
	for _, delay := range duplicateAgentChecks {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		lookCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		providers, err := discover(lookCtx)
		cancel()
		if err != nil {
			continue
		}
		var others []string
		for _, p := range providers {
			if p.GetSrvName() == name && p.GetPeerId() != self && !slices.Contains(others, p.GetPeerId()) {
				others = append(others, p.GetPeerId())
			}
		}
		if len(others) > 0 {
			log.Printf("warning: another node on your network also serves a2a://%s (peer %s); stop it with 'agentiscaled down -data-dir <its directory>' or offer this agent under another name with 'agentiscaled agent add -name'", name, strings.Join(others, ", "))
			return
		}
	}
}
