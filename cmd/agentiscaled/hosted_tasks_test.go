package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/google/uuid"
)

func TestHostedRelayUsesScopedMeshCredentials(t *testing.T) {
	ctx := t.Context()
	n, _, _, dir := localAgentFixture(t, localAgentRole(), time.Now().Add(time.Hour), nil)
	parent := uuid.NewString()
	network := uuid.NewString()
	var calls atomic.Int32
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/networks/"+network+hostedPath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if err != nil || bytes.Equal(raw, n.GetIdentity()) {
			t.Error("control needs a scoped mesh credential")
			w.WriteHeader(403)
			return
		}
		claims, err := n.VerifyLocalBiscuit(raw)
		if err != nil {
			t.Error(err)
			w.WriteHeader(403)
			return
		}
		claim := claims.InnermostTaskName()
		wantParent := ""
		if claim == "agentiscale-task/1/"+parent {
			wantParent = parent
		} else if claim != hostedOwnerClaim {
			t.Errorf("unexpected claim %q", claim)
		}
		peerID := n.Host.ID().String()
		ts := r.Header.Get("X-Agentiscale-Time")
		ms, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || r.Header.Get("X-Agentiscale-Peer") != peerID || r.Header.Get("X-Agentiscale-Parent") != wantParent {
			t.Error("invalid caller proof headers")
		}
		proof := fmt.Sprintf("agentiscale-hosted-v1\nGET\n%s\n%s\n%s\n%s\n%x\n%x", r.URL.Path, peerID, ts, wantParent, sha256.Sum256(raw), sha256.Sum256(nil))
		pub := n.Host.Peerstore().PubKey(n.Host.ID())
		for header, msg := range map[string][]byte{"X-Agentiscale-Signature": []byte(proof), "X-Agentiscale-Revocation-Proof": api.RevocationsChallenge(peerID, ms)} {
			sig, err := base64.RawURLEncoding.DecodeString(r.Header.Get(header))
			valid, verifyErr := pub.Verify(msg, sig)
			if err != nil || verifyErr != nil || !valid {
				t.Errorf("invalid %s", header)
			}
		}
		io.WriteString(w, `{"agents":[]}`)
	}))
	defer control.Close()
	d := diagnosis{control: control.URL, network: network, dataDir: dir, priv: n.Host.Peerstore().PrivKey(n.Host.ID())}
	relay := newHostedRelay(d, n, nil)
	if relay == nil {
		t.Fatal("missing relay")
	}
	check := func(path, token string, want int) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		if token != "" {
			req.Header.Set(api.HeaderMeshAuthentication, "Bearer "+token)
		}
		// These may never replace the authenticated node and parent.
		req.Header.Set("X-Agentiscale-Parent", uuid.NewString())
		req.Header.Set("Authorization", "Bearer operator")
		w := httptest.NewRecorder()
		relay.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("request %s returned HTTP %d: %s", path, w.Code, w.Body)
		}
	}
	check(hostedPath, "", 200)
	nodeSocket := socketClient(filepath.Join(dir, meshnode.DefaultSocketName))
	delegated, err := mintTaskToken(ctx, nodeSocket, "agentiscale-task/1/"+parent, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	check(hostedPath, delegated, 200)
	check(hostedPath, base64.RawURLEncoding.EncodeToString(n.GetIdentity()), 403)
	check(hostedPath, "malformed", 403)
	check("/v1/networks", "", 404)
	check(hostedPath+"?next=elsewhere", "", 404)
	if calls.Load() != 2 {
		t.Fatal("unexpected signed requests", calls.Load())
	}
	d.control = "http://example.org"
	if newHostedRelay(d, n, nil) != nil {
		t.Error("accepted insecure control origin")
	}
}

func TestHostedClientColdTaskAndDiscovery(t *testing.T) {
	ctx := context.Background()
	netw := newFakeNetwork(t)
	socket := filepath.Join(netw.dir, localAgentSocketName)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	agent, task, request := uuid.NewString(), uuid.NewString(), uuid.NewString()
	localPeer := randomPeer(t)
	var catalogStatus, submits, reads atomic.Int32
	var alsoLocal atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		info := localAgentInfo{PeerID: localPeer, Hosted: true}
		if alsoLocal.Load() {
			info.Services = []string{"helper"}
		}
		json.NewEncoder(w).Encode(info)
	})
	mux.HandleFunc("GET "+hostedPath, func(w http.ResponseWriter, r *http.Request) {
		if status := catalogStatus.Load(); status != 0 {
			w.WriteHeader(int(status))
			return
		}
		fmt.Fprintf(w, `{"agents":[{"id":%q,"name":"helper"}]}`, agent)
	})
	path := hostedPath + "/" + agent + "/tasks"
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		var in hostedSubmission
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if in.TaskID != "" {
			if in.TaskID != task || !canonicalID(in.RequestID) || in.RequestID == request || in.Text != "answer" {
				t.Errorf("bad follow-up %+v", in)
			}
			fmt.Fprintf(w, `{"task_id":%q,"state":"working"}`, task)
			return
		}
		if in.RequestID != request || in.Text != "review" {
			t.Errorf("incorrect submission %+v", in)
		}
		if submits.Add(1) == 1 {
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"task_id":`)
			return
		}
		fmt.Fprintf(w, `{"task_id":%q,"state":"submitted"}`, task)
	})
	mux.HandleFunc("GET "+path+"/"+task, func(w http.ResponseWriter, r *http.Request) {
		if reads.Add(1) == 1 {
			fmt.Fprintf(w, `{"task_id":%q,"state":"working","message":"starting"}`, task)
			return
		}
		fmt.Fprintf(w, `{"task_id":%q,"state":"completed","message":"review result"}`, task)
	})
	mux.HandleFunc("POST "+path+"/"+task+"/cancel", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"task_id":%q,"state":"cancelled"}`, task)
	})
	server := &http.Server{Handler: mux}
	go server.Serve(ln)
	defer server.Close()

	run := startTaskSend("", "-data-dir", netw.dir, "-request-id", request, "helper", "review")
	if err := run.wait(t); err != nil || run.out.String() != "review result\n" || submits.Load() != 2 || reads.Load() != 2 {
		t.Fatalf("CLI cold submission: %v %q submits=%d reads=%d", err, run.out.String(), submits.Load(), reads.Load())
	}
	target := taskTarget{Service: "helper", PeerID: hostedPrefix + agent}
	tasks := &a2aTasks{local: socketClient(socket), http: socketClient(filepath.Join(netw.dir, meshnode.DefaultSocketName)), base: "http://agentiscaled"}
	var listing bytes.Buffer
	if err := runAgent(ctx, []string{"list", "-data-dir", netw.dir, "-hosted"}, &listing); err != nil || !strings.Contains(listing.String(), target.PeerID) {
		t.Fatalf("CLI hosted discovery: %v %s", err, listing.String())
	}
	_, list, err := tasks.listHostedAgents(ctx, nil, struct{}{})
	if err != nil || len(list.Agents) != 1 || list.Agents[0].PeerID != target.PeerID {
		t.Fatalf("catalog %+v %v", list, err)
	}
	if _, _, err := tasks.card(ctx, nil, target); err != nil {
		t.Fatal(err)
	}
	_, follow, err := tasks.send(ctx, nil, sendTaskIn{taskTarget: target, Text: "answer", TaskID: task})
	if err != nil || follow.State != "working" {
		t.Fatalf("follow-up: %+v %v", follow, err)
	}
	_, cancelled, err := tasks.cancel(ctx, nil, taskIn{taskTarget: target, TaskID: task})
	if err != nil || cancelled.State != "cancelled" {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	if _, _, err := tasks.send(ctx, nil, sendTaskIn{taskTarget: taskTarget{Service: "wrong", PeerID: target.PeerID}, Text: "wrong"}); err == nil {
		t.Fatal("accepted wrong service name")
	}
	remote := randomPeer(t)
	netw.offer(remote)
	if _, _, err := findAgent(ctx, tasks, "helper", "", 0); err == nil || !strings.Contains(err.Error(), target.PeerID) || !strings.Contains(err.Error(), remote) {
		t.Fatalf("mesh/hosted collision: %v", err)
	}
	alsoLocal.Store(true)
	if _, _, err := findAgent(ctx, tasks, "helper", "", 0); err == nil || !strings.Contains(err.Error(), localPeer) || !strings.Contains(err.Error(), target.PeerID) {
		t.Fatalf("local/hosted collision: %v", err)
	}
	alsoLocal.Store(false)
	for _, status := range []int32{404, 503} {
		catalogStatus.Store(status)
		found, _, err := findAgent(ctx, tasks, "helper", "", 0)
		if err != nil || found.PeerID != remote {
			t.Fatalf("peer unavailable with control HTTP %d: %+v %v", status, found, err)
		}
	}
}
