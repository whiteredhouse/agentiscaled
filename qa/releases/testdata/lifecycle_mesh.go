// CI companion only: this command never builds the release under test.
package main

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/google/agentmesh/export/standalone"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// inspect copies the stopped database before opening it. NewStore may migrate
// schemas, so the observer must never write the actual upgrade subject.
// Never return key material or credentials to the controller/report.
func inspect(dir string) (map[string]string, error) {
	if _, err := os.Stat(filepath.Join(dir, meshnode.StoreFile)); err != nil {
		return nil, err
	}
	snapshot, err := os.MkdirTemp("", "release-identity-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(snapshot)
	raw, err := os.ReadFile(filepath.Join(dir, meshnode.StoreFile))
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(snapshot, meshnode.StoreFile), raw, 0600); err != nil {
		return nil, err
	}
	store, err := meshnode.NewStore(snapshot)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	key, err := store.LoadKey()
	if err != nil {
		return nil, err
	}
	priv, err := crypto.UnmarshalPrivateKey(key)
	if err != nil {
		return nil, err
	}
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	credential, err := store.Credential()
	if err != nil {
		return nil, err
	}
	identity, err := (proto.MarshalOptions{Deterministic: true}).Marshal(credential)
	if err != nil {
		return nil, err
	}
	meshURL, err := store.LoadControlPlaneURL()
	if err != nil {
		return nil, err
	}
	return map[string]string{"peer_id": id.String(), "key_sha256": digest(key), "identity_sha256": digest(identity), "mesh_url": meshURL}, nil
}

type fixtureEvidence struct {
	Enrollments              []string            `json:"enrollments"`
	JoinCodePeers            []string            `json:"join_code_peers"`
	JoinRequests             []map[string]string `json:"join_requests"`
	RouterPeerID             string              `json:"router_peer_id"`
	OAuthDeviceRequests      int                 `json:"oauth_device_requests"`
	OAuthTokenRequests       int                 `json:"oauth_token_requests"`
	SignInEnrollmentRequests int                 `json:"signin_enrollment_requests"`
	DeniedAuthRequests       int                 `json:"denied_auth_requests"`
	DeniedJoinRequests       int                 `json:"denied_join_requests"`
	RejectedJoinRequests     int                 `json:"rejected_join_requests"`
	UnexpectedRequests       int                 `json:"unexpected_requests"`
	SignInLocked             bool                `json:"signin_locked"`
	EnrollmentLocked         bool                `json:"enrollment_locked"`
}

// fixtureControl is a loopback protocol stand-in, not the account/ownership
// control service. The actual mesh enforces the bootstrap token's peer binding.
type fixtureControl struct {
	mu                                        sync.Mutex
	mesh                                      *standalone.Server
	adminToken, code, nodeData, secondaryData string
	codeUsed                                  bool
	client                                    *http.Client
	evidence                                  fixtureEvidence
}

func newFixtureControl(mesh *standalone.Server, adminToken, code, nodeData, secondaryData string) *fixtureControl {
	return &fixtureControl{
		mesh: mesh, adminToken: adminToken, code: code, nodeData: nodeData, secondaryData: secondaryData,
		client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}},
		evidence: fixtureEvidence{Enrollments: []string{}, JoinCodePeers: []string{},
			JoinRequests: []map[string]string{}, RouterPeerID: mesh.PeerID()},
	}
}

func respond(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// mint uses the real public mesh admin API rather than a reusable standing join
// token. Its one-use node token is bound to the exact requesting peer ID.
func (f *fixtureControl) mint(ctx context.Context, peerID string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"role": api.RoleNode, "ttl_hours": 1, "max_usages": 1,
		"description": "agentiscale-bound-peer:" + peerID + " native CI enrollment",
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.mesh.PublicURL()+"/admin/bootstrap-tokens", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+f.adminToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("mesh token endpoint returned HTTP %d", response.StatusCode)
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&created); err != nil || created.Token == "" {
		return "", fmt.Errorf("mesh token endpoint returned no usable token")
	}
	return created.Token, nil
}

func requestFields(w http.ResponseWriter, r *http.Request) (map[string]string, error) {
	var fields map[string]string
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := decoder.Decode(&fields); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("expected exactly one JSON object")
	}
	return fields, nil
}

func (f *fixtureControl) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.RawQuery != "" {
			f.evidence.UnexpectedRequests++
			http.Error(w, "query parameters are not supported", http.StatusBadRequest)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /lock-signin":
			f.evidence.SignInLocked = true
			respond(w, map[string]bool{"locked": true})
		case "POST /lock-enrollment":
			f.evidence.SignInLocked, f.evidence.EnrollmentLocked = true, true
			respond(w, map[string]bool{"locked": true})
		// Earlier released binaries in upgrade pairs still use the previous
		// provider's device paths; both answer the same way.
		case "POST /oauth2/device/auth", "POST /oauth/device/code":
			f.evidence.OAuthDeviceRequests++
			if f.evidence.SignInLocked {
				f.evidence.DeniedAuthRequests++
				http.Error(w, "fixture sign-in is locked", http.StatusForbidden)
				return
			}
			activate := "http://" + r.Host + "/activate"
			respond(w, map[string]any{"device_code": "ci-device", "user_code": "CI-ONLY", "verification_uri": activate, "verification_uri_complete": activate, "expires_in": 60, "interval": 0})
		case "POST /oauth2/token", "POST /oauth/token":
			f.evidence.OAuthTokenRequests++
			if f.evidence.SignInLocked {
				f.evidence.DeniedAuthRequests++
				http.Error(w, "fixture sign-in is locked", http.StatusForbidden)
				return
			}
			respond(w, map[string]string{"access_token": "ci-access", "token_type": "Bearer"})
		case "POST /v1/enroll":
			f.evidence.SignInEnrollmentRequests++
			if f.evidence.SignInLocked {
				f.evidence.DeniedAuthRequests++
				http.Error(w, "fixture sign-in is locked", http.StatusForbidden)
				return
			}
			if r.Header.Get("Authorization") != "Bearer ci-access" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			fields, err := requestFields(w, r)
			if err != nil || fields["peer_id"] == "" || len(fields) > 2 || (len(fields) == 2 && fields["machine_name"] == "") {
				http.Error(w, "bad enrollment request", http.StatusBadRequest)
				return
			}
			if _, err := peer.Decode(fields["peer_id"]); err != nil {
				http.Error(w, "bad peer", http.StatusBadRequest)
				return
			}
			token, err := f.mint(r.Context(), fields["peer_id"])
			if err != nil {
				http.Error(w, "fixture token mint failed", http.StatusInternalServerError)
				return
			}
			f.evidence.Enrollments = append(f.evidence.Enrollments, fields["peer_id"])
			respond(w, map[string]string{"network_id": "release-ci-network", "mesh_url": f.mesh.PublicURL(), "bootstrap_token": token})
		case "POST /v1/join":
			if f.evidence.EnrollmentLocked {
				f.evidence.DeniedJoinRequests++
				http.Error(w, "fixture enrollment is locked", http.StatusForbidden)
				return
			}
			fields, err := requestFields(w, r)
			if err != nil || len(fields) != 3 || fields["code"] != f.code || fields["machine_name"] != "qa-headless" || r.Header.Get("Authorization") != "" || f.codeUsed {
				f.evidence.RejectedJoinRequests++
				http.Error(w, "fixture join refused", http.StatusForbidden)
				return
			}
			if _, err := peer.Decode(fields["peer_id"]); err != nil {
				f.evidence.RejectedJoinRequests++
				http.Error(w, "fixture join refused", http.StatusForbidden)
				return
			}
			token, err := f.mint(r.Context(), fields["peer_id"])
			if err != nil {
				http.Error(w, "fixture token mint failed", http.StatusInternalServerError)
				return
			}
			f.codeUsed = true
			f.evidence.JoinCodePeers = append(f.evidence.JoinCodePeers, fields["peer_id"])
			f.evidence.JoinRequests = append(f.evidence.JoinRequests, map[string]string{"peer_id": fields["peer_id"], "machine_name": fields["machine_name"]})
			respond(w, map[string]string{"network_id": "release-ci-network", "mesh_url": f.mesh.PublicURL(), "bootstrap_token": token})
		case "GET /inspect", "GET /inspect-secondary":
			dir := f.nodeData
			if r.URL.Path == "/inspect-secondary" {
				dir = f.secondaryData
			}
			if dir == "" {
				http.NotFound(w, r)
				return
			}
			result, err := inspect(dir)
			if err != nil {
				http.Error(w, "cannot inspect stopped fixture node", http.StatusInternalServerError)
				return
			}
			respond(w, result)
		case "GET /evidence":
			respond(w, f.evidence)
		default:
			f.evidence.UnexpectedRequests++
			http.Error(w, "unexpected fixture request", http.StatusNotFound)
		}
	})
}

func main() {
	data := flag.String("data", "", "isolated mesh directory")
	nodeData := flag.String("node-data", "", "stopped node database to inspect")
	secondaryData := flag.String("secondary-node-data", "", "optional second stopped node database to inspect")
	ready := flag.String("ready", "", "output JSON endpoint descriptor")
	flag.Parse()
	if os.Getenv("CI") != "true" || *data == "" || *nodeData == "" || *ready == "" {
		log.Fatal("requires CI=true and --data, --node-data, --ready")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	secret := func() string {
		var raw [32]byte
		if _, err := cryptorand.Read(raw[:]); err != nil {
			log.Fatal("cannot generate fixture secret")
		}
		return hex.EncodeToString(raw[:])
	}
	adminToken, code := secret(), secret()
	mesh, err := standalone.New(standalone.Options{BindAddress: "127.0.0.1:0", DataDir: *data, DisableJoinToken: true, AdminToken: adminToken})
	if err != nil {
		log.Fatal(err)
	}
	if err := mesh.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer mesh.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	base := "http://" + listener.Addr().String()
	control := newFixtureControl(mesh, adminToken, code, *nodeData, *secondaryData)
	server := &http.Server{Handler: control.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Print(err)
			cancel()
		}
	}()
	descriptor, _ := json.Marshal(map[string]string{"control": base, "mesh_url": mesh.PublicURL(), "join_code": code})
	if err := os.WriteFile(*ready, descriptor, 0600); err != nil {
		log.Fatal(err)
	}
	<-ctx.Done()
	shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_ = server.Shutdown(shutdown)
}
