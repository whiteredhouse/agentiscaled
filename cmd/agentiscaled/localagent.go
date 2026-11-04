package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
)

const localAgentSocketName = "agent.sock"

type localAgentInfo struct {
	PeerID  string `json:"peer_id"`
	Service string `json:"service"`
}

type agentSocketContextKey struct{}

// serveLocalAgent is agentiscaled's owner API, separate from Agent Mesh's
// network routes. Both paths proxy to the same agent handler and task store.
func serveLocalAgent(ctx context.Context, node *meshnode.AgentMeshNode, cfg *meshnode.NodeConfigComplete, name, target, dataDir string) (*http.Server, error) {
	backend, err := url.Parse(target)
	if err != nil || backend == nil || backend.User == nil {
		return nil, fmt.Errorf("invalid local agent backend")
	}
	token, ok := backend.User.Password()
	if !ok || token == "" {
		return nil, fmt.Errorf("local agent backend has no credential")
	}
	backend.User = nil
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(backend)
		pr.Out.Header.Set("Authorization", "Bearer "+token)
	}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if local, _ := r.Context().Value(agentSocketContextKey{}).(bool); !local {
			http.Error(w, "Unix socket required", http.StatusForbidden)
			return
		}
		info := localAgentInfo{PeerID: node.Host.ID().String(), Service: name}
		if r.Method == http.MethodGet && r.URL.Path == "/info" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(info)
			return
		}
		prefix := "/a2a/" + info.PeerID + "/" + name
		backendPath, allowedMethod := "/", http.MethodPost
		switch r.URL.Path {
		case prefix, prefix + "/":
		case prefix + "/.well-known/agent-card.json":
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
		if err := authorizeLocalAgent(node, cfg, name, r.Method, backendPath); err != nil {
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

// authorizeLocalAgent reuses the embedded node's existing authorization
// methods. The embedding package exports aliases only; no mesh behavior changes.
func authorizeLocalAgent(node *meshnode.AgentMeshNode, cfg *meshnode.NodeConfigComplete, name, method, path string) error {
	token := node.GetIdentity()
	claims, err := node.VerifyLocalBiscuit(token)
	if err != nil {
		return err
	}
	if claims.NodePeerID != node.Host.ID().String() || !slices.Contains(claims.Roles, api.RoleNode) {
		return fmt.Errorf("local credential is not this node's identity")
	}
	node.MeshPolicyMu.RLock()
	rules := node.MeshPolicyRules
	node.MeshPolicyMu.RUnlock()
	if err := meshnode.RequirePublish(claims.Biscuit, claims.VerifyingKey, api.ServiceTypeStringA2A, name, rules, node.BiscuitTimeout); err != nil {
		return err
	}
	if cfg != nil && len(cfg.EgressRequireLabels) > 0 {
		check, err := api.LabelCheck(cfg.EgressRequireLabels)
		if err != nil {
			return err
		}
		auth, err := claims.Biscuit.Authorizer(claims.VerifyingKey, meshnode.AuthorizerOptions(node.BiscuitTimeout)...)
		if err != nil {
			return err
		}
		auth.AddCheck(check)
		auth.AddPolicy(api.AllowIfTruePolicy)
		if err := auth.Authorize(); err != nil {
			return fmt.Errorf("local provider does not meet the configured label floor: %w", err)
		}
	}
	return node.VerifyBiscuitToken(token, meshnode.RequestContext{PeerID: node.Host.ID(), Local: true, Protocol: "agentiscale-local-a2a", Target: "a2a://" + name, HTTP: &meshnode.HTTPRequestFacts{Method: method, Path: path}})
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
			log.Printf("warning: another node on your network also serves a2a://%s (peer %s); stop it with 'agentiscaled down -data-dir <its directory>' or give this agent another -agent-name", name, strings.Join(others, ", "))
			return
		}
	}
}
