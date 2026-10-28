// CI companion only: this command never builds the release under test.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
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

func main() {
	data := flag.String("data", "", "isolated mesh directory")
	nodeData := flag.String("node-data", "", "stopped node database to inspect")
	ready := flag.String("ready", "", "output JSON endpoint descriptor")
	flag.Parse()
	if os.Getenv("CI") != "true" || *data == "" || *nodeData == "" || *ready == "" {
		log.Fatal("requires CI=true and --data, --node-data, --ready")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	mesh, err := standalone.New(standalone.Options{BindAddress: "127.0.0.1:0", DataDir: *data})
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
	var mu sync.Mutex
	enrollments := []string{}
	mux := http.NewServeMux()
	respond := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("POST /oauth/device/code", func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]any{"device_code": "ci-device", "user_code": "CI-ONLY", "verification_uri": base + "/activate", "verification_uri_complete": base + "/activate", "expires_in": 60, "interval": 0})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]string{"access_token": "ci-access", "token_type": "Bearer"})
	})
	mux.HandleFunc("POST /v1/enroll", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ci-access" {
			http.Error(w, "unauthorized", 401)
			return
		}
		var request struct {
			PeerID string `json:"peer_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if _, err := peer.Decode(request.PeerID); err != nil {
			http.Error(w, "bad peer", 400)
			return
		}
		mu.Lock()
		enrollments = append(enrollments, request.PeerID)
		mu.Unlock()
		respond(w, map[string]string{"network_id": "release-ci-network", "mesh_url": mesh.PublicURL(), "bootstrap_token": mesh.JoinToken()})
	})
	mux.HandleFunc("GET /inspect", func(w http.ResponseWriter, r *http.Request) {
		result, err := inspect(*nodeData)
		if err != nil {
			http.Error(w, fmt.Sprint(err), 500)
			return
		}
		respond(w, result)
	})
	mux.HandleFunc("GET /evidence", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		respond(w, map[string]any{"enrollments": enrollments, "router_peer_id": mesh.PeerID()})
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Print(err)
			cancel()
		}
	}()
	descriptor, _ := json.Marshal(map[string]string{"control": base, "mesh_url": mesh.PublicURL()})
	if err := os.WriteFile(*ready, descriptor, 0600); err != nil {
		log.Fatal(err)
	}
	<-ctx.Done()
	shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_ = server.Shutdown(shutdown)
}
