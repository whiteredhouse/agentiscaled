package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

const hostedPath = "/hosted-agents"
const hostedOwnerClaim = "agentiscale-hosted-owner"
const hostedIDPattern = "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"

var hostedTaskRoute = regexp.MustCompile("^/hosted-agents/" + hostedIDPattern + "/tasks(/" + hostedIDPattern + "(/cancel)?)?$")

type hostedRelay struct {
	base      string
	node      *meshnode.AgentMeshNode
	peer      peer.ID
	key       crypto.PrivKey
	localNode *http.Client
	control   *http.Client
}

// The owner socket signs only the known hosted routes. A caller cannot select
// the control origin, network, signed headers, or a less restricted credential.
func newHostedRelay(d diagnosis, node *meshnode.AgentMeshNode, cfg *meshnode.NodeConfigComplete) http.Handler {
	network, err := uuid.Parse(d.network)
	if err != nil || network == uuid.Nil || d.priv == nil {
		return nil
	}
	origin, err := url.Parse(d.control)
	if err != nil || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || strings.Trim(origin.Path, "/") != "" {
		return nil
	}
	ip := net.ParseIP(origin.Hostname())
	if origin.Scheme != "https" && !(origin.Scheme == "http" && (ip.IsLoopback() || origin.Hostname() == "localhost")) {
		return nil
	}
	pid, err := peer.IDFromPrivateKey(d.priv)
	if err != nil {
		return nil
	}
	if cfg != nil && len(cfg.EgressRequireLabels) != 0 {
		// A logical hosted service does not present a mesh provider identity
		// matching an operator's required egress labels.
		return nil
	}
	base := origin.Scheme + "://" + origin.Host + "/v1/networks/" + network.String()
	localNode := socketClient(filepath.Join(d.dataDir, meshnode.DefaultSocketName))
	localNode.Timeout = 5 * time.Second
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &hostedRelay{base: base, node: node, peer: pid, key: d.priv, localNode: localNode, control: client}
}

func (h *hostedRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	match := hostedTaskRoute.FindStringSubmatch(path)
	method := http.MethodGet
	if match != nil && (match[1] == "" || match[2] != "") {
		method = http.MethodPost
	}
	if r.Method != method || (path != hostedPath && match == nil) || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.EscapedPath() != path {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMessageBytes+4096))
	if err != nil || ((method == http.MethodGet || strings.HasSuffix(path, "/cancel")) && len(body) != 0) {
		http.Error(w, "invalid hosted request", http.StatusBadRequest)
		return
	}
	creds := r.Header.Values(api.HeaderMeshAuthentication)
	raw, parent, err := h.credential(r.Context(), creds)
	if err != nil {
		if len(creds) == 0 {
			http.Error(w, "hosted credential unavailable", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "invalid delegated credential", http.StatusForbidden)
		}
		return
	}
	up, err := http.NewRequestWithContext(r.Context(), method, h.base+path, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "invalid control endpoint", http.StatusInternalServerError)
		return
	}
	if err := h.sign(up, parent, raw, body); err != nil {
		http.Error(w, "cannot sign hosted request", http.StatusInternalServerError)
		return
	}
	if len(body) > 0 {
		up.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.control.Do(up)
	if err != nil {
		http.Error(w, "hosted control unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Cache-Control", "no-store")
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		http.Error(w, http.StatusText(resp.StatusCode), resp.StatusCode)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHostedResponse+1))
	if err != nil || len(data) > maxHostedResponse {
		http.Error(w, "invalid hosted response", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

// credential exchanges the standing node identity locally. A task must present
// its existing scoped token and inherits the parent bound to that token.
func (h *hostedRelay) credential(ctx context.Context, creds []string) ([]byte, string, error) {
	if len(creds) == 0 {
		token, err := mintTaskToken(ctx, h.localNode, hostedOwnerClaim, time.Now().Add(taskDeadline))
		if err != nil {
			return nil, "", err
		}
		raw, err := decodeToken(token)
		return raw, "", err
	}
	token, ok := strings.CutPrefix(creds[0], "Bearer ")
	if len(creds) != 1 || !ok {
		return nil, "", errors.New("invalid task credential")
	}
	raw, err := decodeToken(token)
	if err != nil {
		return nil, "", err
	}
	claims, err := h.node.VerifyLocalBiscuit(raw)
	if err != nil || claims.NodePeerID != h.peer.String() {
		return nil, "", errors.New("invalid task credential")
	}
	depthAndID, ok := strings.CutPrefix(claims.InnermostTaskName(), taskClaimPrefix)
	depth, id, found := strings.Cut(depthAndID, "/")
	if !ok || !found || (depth != "0" && depth != "1") || !canonicalID(id) {
		return nil, "", errors.New("invalid delegation")
	}
	return raw, id, nil
}

func (h *hostedRelay) sign(up *http.Request, parent string, raw, body []byte) error {
	now := time.Now().UnixMilli()
	ts := fmt.Sprint(now)
	proof := fmt.Sprintf("agentiscale-hosted-v1\n%s\n%s\n%s\n%s\n%s\n%x\n%x", up.Method, up.URL.EscapedPath(), h.peer, ts, parent, sha256.Sum256(raw), sha256.Sum256(body))
	sig, err := h.key.Sign([]byte(proof))
	if err != nil {
		return err
	}
	revProof, err := h.key.Sign(api.RevocationsChallenge(h.peer.String(), now))
	if err != nil {
		return err
	}
	up.Header.Set("X-Agentiscale-Revocation-Proof", base64.RawURLEncoding.EncodeToString(revProof))
	up.Header.Set("X-Agentiscale-Peer", h.peer.String())
	up.Header.Set("X-Agentiscale-Time", ts)
	up.Header.Set("X-Agentiscale-Signature", base64.RawURLEncoding.EncodeToString(sig))
	if parent != "" {
		up.Header.Set("X-Agentiscale-Parent", parent)
	}
	up.Header.Set("Authorization", "Bearer "+base64.RawURLEncoding.EncodeToString(raw))
	return nil
}
