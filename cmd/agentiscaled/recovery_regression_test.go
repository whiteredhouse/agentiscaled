package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
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

// A foreground node has no saved settings: reset drops its identity and stop
// without inventing settings, and tells the owner to restart it with its own
// flags, which may now carry a join code.
func TestResetForegroundNodeWithoutSavedSettings(t *testing.T) {
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
	var out bytes.Buffer
	if err := runReset(context.Background(), []string{"-data-dir", dir}, &out); err != nil {
		t.Fatalf("reset foreground node: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, settingsFile)); !os.IsNotExist(err) {
		t.Fatalf("reset invented saved settings for a foreground node: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, meshnode.StoreFile)); !os.IsNotExist(err) {
		t.Fatalf("reset kept the old identity (%s): %v", oldPeer, err)
	}
	if h, err := loadHalt(dir); err != nil || h != nil {
		t.Fatalf("reset did not clear halt: %+v, %v", h, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "api-token")); string(b) != "KEEP\n" {
		t.Fatal("reset changed the API token")
	}
	if !strings.Contains(out.String(), "original flags") || !strings.Contains(out.String(), "-join-code-path") || strings.Contains(out.String(), " run -data-dir ") {
		t.Fatalf("restart instructions: %s", &out)
	}
}

func TestResetNeedsANode(t *testing.T) {
	dir := shortTempDir(t)
	err := runReset(context.Background(), []string{"-data-dir", dir}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no node set up") {
		t.Fatalf("reset of an empty directory: %v", err)
	}
}

// After reset, joining again with a join code needs no sign-in and clears a
// stop recorded for the previous identity.
func TestJoinWithCodeAfterReset(t *testing.T) {
	dir := shortTempDir(t)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	seedNode(t, dir, "https://old.mesh.test", key)
	writeSettings(t, dir, settings{Control: "https://control.test", Listen: "/ip4/127.0.0.1/tcp/0", LogLevel: "info"})
	if err := runReset(context.Background(), []string{"-data-dir", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	// A stop written by a node still running when reset began.
	if err := saveHalt(dir, newHalt(haltNetworkGone, "old", dir)); err != nil {
		t.Fatal(err)
	}
	code := filepath.Join(dir, "join-code")
	write(t, code, "ajc_test\n")
	newNetwork := uuid.NewString()
	enrollNew = func(ctx context.Context, c config) (string, error) {
		if c.signInToken != "" || c.joinCodePath != code {
			t.Fatalf("enrolled with sign-in %t, join code %q", c.signInToken != "", c.joinCodePath)
		}
		seedNode(t, dir, "https://"+newNetwork+".mesh.test", key)
		return newNetwork, nil
	}
	t.Cleanup(func() { enrollNew = enrollOnly })
	s, _ := loadSettings(dir)
	c := s.config(dir)
	c.joinCodePath = code
	// An unreachable issuer: any sign-in attempt fails the test.
	joined, err := ensureJoined(context.Background(), &c, authConfig{issuer: "http://127.0.0.1:1"}, io.Discard)
	if err != nil || joined.networkID != newNetwork {
		t.Fatalf("join with code: %+v, %v", joined, err)
	}
	if h, err := loadHalt(dir); err != nil || h != nil {
		t.Fatalf("join did not clear the old stop: %+v, %v", h, err)
	}
}

// An enrollment that saved its credential and then failed is retried as an
// already joined node; a stop left from the previous identity must not block
// that retry.
func TestFailedJoinAfterResetDoesNotKeepTheOldStop(t *testing.T) {
	dir := shortTempDir(t)
	writeSettings(t, dir, settings{Control: "https://control.test", Listen: "/ip4/127.0.0.1/tcp/0", LogLevel: "info"})
	if err := saveHalt(dir, newHalt(haltNetworkGone, "old", dir)); err != nil {
		t.Fatal(err)
	}
	code := filepath.Join(dir, "join-code")
	write(t, code, "ajc_test\n")
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	enrollNew = func(ctx context.Context, c config) (string, error) {
		seedNode(t, dir, "https://new.mesh.test", key)
		return "", errors.New("no router reachable")
	}
	t.Cleanup(func() { enrollNew = enrollOnly })
	c := settings{Control: "https://control.test"}.config(dir)
	c.joinCodePath = code
	if _, err := ensureJoined(context.Background(), &c, authConfig{issuer: "http://127.0.0.1:1"}, io.Discard); err == nil {
		t.Fatal("join succeeded with a failing enrollment")
	}
	joined, err := ensureJoined(context.Background(), &c, authConfig{issuer: "http://127.0.0.1:1"}, io.Discard)
	if err != nil || joined.meshURL != "https://new.mesh.test" {
		t.Fatalf("retry after a partial enrollment: %+v, %v", joined, err)
	}
}

// After reset, a node with saved settings joins again with plain `up`, which
// keeps its choices; a node run directly with flags is restarted with them.
func TestRejoinSteps(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", shortTempDir(t))
	dir := filepath.Join(shortTempDir(t), "node")
	saved := rejoinSteps(dir, true)
	for _, w := range []string{"  agentiscaled up -data-dir " + dir + "\n", "agentiscaled up -data-dir " + dir + " -join-code-path <file>"} {
		if !strings.Contains(saved, w) {
			t.Errorf("missing %q in:\n%s", w, saved)
		}
	}
	if strings.Contains(saved, "-clients") || strings.Contains(saved, "-foreground") {
		t.Errorf("saved node steps repeat saved choices:\n%s", saved)
	}
	direct := rejoinSteps(dir, false)
	for _, w := range []string{"original flags", "adding -join-code-path <file>", "-foreground -clients none"} {
		if !strings.Contains(direct, w) {
			t.Errorf("missing %q in:\n%s", w, direct)
		}
	}
}
