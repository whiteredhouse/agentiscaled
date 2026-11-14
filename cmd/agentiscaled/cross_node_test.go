package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/agentmesh/export/standalone"
)

// A task's token works across nodes as on one: two nodes of a mesh, each its
// own process, A calling B's agent with tokens A mints for its hosted agents'
// tasks. B runs the task at the token's depth and keeps it as the sending
// task's, refuses a deeper one, and A lets no revoked or expired token out.
func TestTaskTokensAcrossNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a mesh and two nodes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	mesh, err := standalone.New(standalone.Options{BindAddress: "127.0.0.1:0", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := mesh.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer mesh.Close()
	joinToken := filepath.Join(t.TempDir(), "join-token")
	if err := os.WriteFile(joinToken, []byte(mesh.JoinToken()), 0o600); err != nil {
		t.Fatal(err)
	}

	// B's helper is Codex as far as the node knows: a script that records
	// each run's arguments and answers.
	runs := t.TempDir()
	helper := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\ncase \"$1\" in --version) echo 'codex-cli 0.169.0'; exit 0;; login) exit 0;; esac\n" +
		"n=$(date +%s%N); printf '%s\\n' \"$@\" > " + runs + "/$n.args; cat > /dev/null\n" +
		"while [ $# -gt 0 ]; do if [ \"$1\" = --output-last-message ]; then echo '{\"status\":\"done\",\"text\":\"helped\"}' > \"$2\"; fi; shift; done\n" +
		"echo '{\"type\":\"thread.started\",\"thread_id\":\"0199a7e0-0000-7000-8000-000000000001\"}'\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"OPENAI_API_KEY": "unused"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	node := func(name string, agents []agentConfig) (dir, peerID string) {
		t.Helper()
		dir = shortTempDir(t)
		c := config{dataDir: dir, controlPlane: mesh.PublicURL(), bootstrapTokenPath: joinToken, listen: "/ip4/127.0.0.1/tcp/0", logLevel: "info", machineName: name}
		if _, err := enrollOnly(ctx, c); err != nil {
			t.Fatalf("enroll %s: %v", name, err)
		}
		writeSettings(t, dir, settings{Listen: c.listen, LogLevel: "info", Clients: "none", Foreground: true, Agents: agents, MeshURL: mesh.PublicURL()})
		p := startNode(t, dir)
		p.waitLog(t, "agentiscaled running", 30*time.Second)
		if len(agents) == 0 {
			return dir, ""
		}
		var info localAgentInfo
		for {
			resp, err := socketClient(filepath.Join(dir, localAgentSocketName)).Get("http://agentiscaled/info")
			if err == nil {
				err = json.NewDecoder(resp.Body).Decode(&info)
				resp.Body.Close()
			}
			if err == nil && info.PeerID != "" {
				return dir, info.PeerID
			}
			if ctx.Err() != nil {
				t.Fatalf("node %s: %v\n%s", name, err, p.output())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	a, _ := node("node-a", nil)
	_, peerB := node("node-b", []agentConfig{{Runtime: "codex", Name: "helper", Dir: t.TempDir(), Command: helper, Env: []string{"CODEX_HOME=" + codexHome}}})

	socketA, agentSocketA := filepath.Join(a, "agentmesh.sock"), filepath.Join(a, localAgentSocketName)
	minter := &taskNetwork{node: socketClient(socketA)}
	mint := func(claim string, ttl time.Duration) string {
		t.Helper()
		token, err := minter.mint(ctx, claim, time.Now().Add(ttl))
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	as := func(token string) *a2aTasks {
		return &a2aTasks{http: meshTokenClient(ctx, socketA, token), base: "http://agentiscaled", local: meshTokenClient(ctx, agentSocketA, token)}
	}
	owner := &a2aTasks{http: socketClient(socketA), base: "http://agentiscaled", local: socketClient(agentSocketA)}
	card := func(token string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://agentiscaled/mesh/"+peerB+"/a2a/helper/.well-known/agent-card.json", nil)
		resp, err := meshTokenClient(ctx, socketA, token).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	helperTarget := taskTarget{PeerID: peerB, Service: "helper"}
	in := func(id string, wait int) getTaskIn {
		return getTaskIn{taskIn: taskIn{taskTarget: helperTarget, TaskID: id}, WaitSeconds: wait}
	}

	parent := mint("agentiscale-task/1/parent", 10*time.Minute)
	// B may not be found at once: the mesh learns of it as it joins.
	for card(parent) != http.StatusOK {
		if ctx.Err() != nil {
			t.Fatal("node A never reached B's helper")
		}
		time.Sleep(200 * time.Millisecond)
	}
	_, sent, err := as(parent).send(ctx, nil, sendTaskIn{taskTarget: helperTarget, Text: "help with x"})
	if err != nil {
		t.Fatal(err)
	}
	_, done, err := as(parent).get(ctx, nil, in(sent.TaskID, 20))
	if err != nil || done.Terminal != "completed" || done.Result != "helped" {
		t.Fatalf("the task across nodes: %+v %v", done, err)
	}
	// The helper worked at the token's depth, without the network.
	args, _ := filepath.Glob(filepath.Join(runs, "*.args"))
	for _, f := range args {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), "mcp_servers") {
			t.Errorf("B's helper ran a delegated task with the network: %s", b)
		}
	}
	if len(args) == 0 {
		t.Fatal("B's helper never ran")
	}
	// B keeps the task as the parent's: not A's owner's, nor another task's.
	for name, by := range map[string]*a2aTasks{"A's owner": owner, "another task of A": as(mint("agentiscale-task/1/other", time.Minute))} {
		if _, _, err := by.get(ctx, nil, in(sent.TaskID, 0)); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("B gave %s the parent's task: %v", name, err)
		}
	}
	// A deeper token than the bound is refused at B.
	deep := as(mint(fmt.Sprintf("%s%d/deep", taskClaimPrefix, maxDelegationDepth+1), time.Minute))
	_, tooDeep, err := deep.send(ctx, nil, sendTaskIn{taskTarget: helperTarget, Text: "deeper"})
	if err != nil {
		t.Fatal(err)
	}
	if _, got, err := deep.get(ctx, nil, in(tooDeep.TaskID, 10)); err != nil || got.Terminal != "rejected" || !strings.Contains(got.Message, "delegation depth") {
		t.Errorf("a delegated task over the bound: %+v %v", got, err)
	}

	// A revoked token and an expired one go no further than A.
	if err := minter.revoke(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if code := card(parent); code != http.StatusForbidden {
		t.Errorf("a revoked token reached B: HTTP %d", code)
	}
	short := mint("agentiscale-task/1/short", 2*time.Second)
	if code := card(short); code != http.StatusOK {
		t.Fatalf("a live token: HTTP %d", code)
	}
	time.Sleep(3 * time.Second)
	if code := card(short); code != http.StatusForbidden {
		t.Errorf("an expired token reached B: HTTP %d", code)
	}
}
