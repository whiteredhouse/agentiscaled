package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// An expired Biscuit is refused by /policies, /egress and /revocations,
// but /refresh can still renew it using proof of possession of the node key.
func TestNodeRefreshesBeforeHalting(t *testing.T) {
	for _, phase := range []struct {
		name                     string
		running, expired, outage bool
	}{
		{name: "startup", expired: true},
		{name: "running", running: true},
		{name: "refresh outage", outage: true},
		{name: "expired refresh outage", expired: true, outage: true},
		{name: "running refresh outage", running: true, outage: true},
	} {
		t.Run(phase.name, func(t *testing.T) {
			mesh := newStandInMesh(t, "fine")
			var needsRefresh atomic.Bool
			needsRefresh.Store(!phase.running)
			var refreshed atomic.Bool
			var refreshAsked atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/refresh" {
					refreshAsked.Store(true)
					if phase.outage {
						http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
						return
					}
					b, _ := io.ReadAll(r.Body)
					var req api.TokenRefreshRequest
					if err := proto.Unmarshal(b, &req); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					pid, err := peer.Decode(req.PeerId)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					exp := time.Now().Add(48 * time.Hour)
					biscuit, err := meshnode.MintBootstrapBiscuitToken(mesh.key, pid, api.RoleNode, exp, nil, nil)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					b, _ = proto.Marshal(&api.TokenRefreshResponse{BiscuitToken: biscuit, ExpireTime: timestamppb.New(exp)})
					needsRefresh.Store(false)
					refreshed.Store(true)
					_, _ = w.Write(b)
					return
				}
				if needsRefresh.Load() && (r.URL.Path == "/policies" || r.URL.Path == "/egress" || r.URL.Path == "/revocations") {
					http.Error(w, "node credential required", http.StatusUnauthorized)
					return
				}
				mesh.serve(w, r)
			}))
			defer srv.Close()
			control := newStandInControl(t, http.StatusOK, "")
			dir := shortTempDir(t)
			pid := seedNode(t, dir, srv.URL, mesh.key)
			store, err := meshnode.NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if phase.expired {
				id, _ := peer.Decode(pid)
				exp := time.Now().Add(-time.Hour)
				biscuit, err := meshnode.MintBootstrapBiscuitToken(mesh.key, id, api.RoleNode, exp, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SaveIdentity(biscuit); err != nil {
					t.Fatal(err)
				}
				if err := store.SaveIdentityExpiration(exp.Unix()); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := store.LoadIdentity()
			store.Close()
			writeSettings(t, dir, settings{Control: control.srv.URL, Listen: "/ip4/127.0.0.1/tcp/0", PeerID: pid, NetworkID: uuid.NewString(), MeshURL: srv.URL})
			node := startNode(t, dir)
			node.waitLog(t, "agentiscaled running", 5*time.Second)
			if phase.running {
				needsRefresh.Store(true)
			}
			deadline := time.Now().Add(5 * time.Second)
			for !refreshAsked.Load() && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if !refreshAsked.Load() {
				t.Fatalf("node did not try to refresh before diagnosing admission: %s", node.output())
			}
			if phase.outage && !phase.expired {
				node.waitLog(t, "retrying", 5*time.Second)
			} else if !phase.outage {
				node.waitLog(t, "refreshed node credential", 5*time.Second)
			}
			node.stop()
			if h, err := loadHalt(dir); err != nil || h != nil {
				t.Fatalf("recoverable node halted: %+v, %v\n%s", h, err, node.output())
			}
			store, err = meshnode.NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			after, _ := store.LoadIdentity()
			if !phase.outage && (!refreshed.Load() || bytes.Equal(before, after)) {
				t.Fatal("node did not persist a fresh credential")
			}
			if phase.outage && !bytes.Equal(before, after) {
				t.Fatal("temporary refresh failure replaced the stored credential")
			}
		})
	}
}

func TestResetForegroundNodeWithoutSavedSettings(t *testing.T) {
	for _, control := range []string{defaultControl, "https://custom.control.test"} {
		t.Run(control, func(t *testing.T) { resetForegroundNode(t, control) })
	}
}

func resetForegroundNode(t *testing.T, control string) {
	t.Helper()
	home := shortTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	dir := filepath.Join(home, "node")
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	oldPeer := seedNode(t, dir, "https://old.mesh.test", key)
	write(t, filepath.Join(dir, "api-token"), "KEEP\n")
	if err := saveHalt(dir, newHalt(haltCredentialRefused, "", dir)); err != nil {
		t.Fatal(err)
	}
	newNetwork := uuid.NewString()
	enrollNew = func(ctx context.Context, c config) (string, error) {
		if c.signInToken != "ACCESS" || c.control != control {
			t.Fatalf("unexpected enrollment config: control=%s, signed in=%t", c.control, c.signInToken != "")
		}
		seedNode(t, dir, "https://"+newNetwork+".mesh.test", key)
		return newNetwork, nil
	}
	t.Cleanup(func() { enrollNew = enrollOnly })
	auth := newFakeAuth0(t, "")
	var out bytes.Buffer
	args := []string{"-data-dir", dir, "-auth-issuer", auth.config().issuer, "-auth-client-id", "CLIENT", "-auth-audience", "https://control.test"}
	if control != defaultControl {
		args = append(args, "-control", control)
	}
	if err := runReset(context.Background(), args, &out); err != nil {
		t.Fatalf("reset foreground node: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, settingsFile)); !os.IsNotExist(err) {
		t.Fatalf("reset invented saved settings for a foreground node: %v", err)
	}
	store, err := meshnode.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pid, _ := peer.IDFromPrivateKey(meshnode.GetOrGenerateKey(store))
	if pid.String() == oldPeer {
		t.Fatal("reset kept the old identity")
	}
	if h, err := loadHalt(dir); err != nil || h != nil {
		t.Fatalf("reset did not clear halt: %+v, %v", h, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "api-token")); string(b) != "KEEP\n" {
		t.Fatal("reset changed the API token")
	}
	if !strings.Contains(out.String(), "original flags") || strings.Contains(out.String(), " run -data-dir ") {
		t.Fatalf("restart instructions require nonexistent saved settings: %s", &out)
	}
}

func TestResetCannotOverrideSavedControl(t *testing.T) {
	dir := shortTempDir(t)
	writeSettings(t, dir, settings{Control: "https://saved.control.test"})
	before, _ := os.ReadFile(filepath.Join(dir, settingsFile))
	err := runReset(context.Background(), []string{"-data-dir", dir, "-control", "https://other.control.test"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "keeps the saved control service URL") {
		t.Fatalf("reset did not reject a control URL override: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, settingsFile))
	if !bytes.Equal(before, after) {
		t.Fatal("reset changed saved settings")
	}
	if _, err := os.Stat(filepath.Join(dir, meshnode.StoreFile)); !os.IsNotExist(err) {
		t.Fatal("reset modified the identity before rejecting the override")
	}
}
