package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-msgio"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/multiformats/go-multiaddr"
	"google.golang.org/protobuf/proto"
)

// Use the daemon's real saved configuration, socket, MCP discovery and A2A
// task handlers. Only the runtime and hosted enrollment service are fixtures.
func TestSingleNodeTasksSurviveRestartAndReset(t *testing.T) {
	dir := shortTempDir(t)
	mesh := newStandInMesh(t, "fine")
	router, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	routerToken, err := meshnode.MintBootstrapBiscuitToken(mesh.key, router.ID(), api.RoleRouter, time.Now().Add(time.Hour), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	router.SetStreamHandler(api.AuthProtocolID, func(s network.Stream) {
		defer s.Close()
		reader := msgio.NewVarintReaderSize(s, 65536)
		frame, err := reader.ReadMsg()
		if err != nil {
			return
		}
		reader.ReleaseMsg(frame)
		response, _ := proto.Marshal(&api.AuthResponse{Success: true, Biscuit: routerToken})
		_ = msgio.NewVarintWriter(s).WriteMsg(response)
	})
	router.SetStreamHandler(api.DirectoryProtocolID, func(s network.Stream) {
		defer s.Close()
		reader := msgio.NewVarintReaderSize(s, 65536)
		frame, err := reader.ReadMsg()
		if err != nil {
			return
		}
		reader.ReleaseMsg(frame)
		response, _ := proto.Marshal(&api.DirectoryResponse{})
		_ = msgio.NewVarintWriter(s).WriteMsg(response)
	})
	routerAddr, err := multiaddr.NewMultiaddr(router.Addrs()[0].String() + "/p2p/" + router.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	mint := func() string {
		pid := seedNode(t, dir, mesh.srv.URL, mesh.key)
		store, err := meshnode.NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		id, _ := peer.Decode(pid)
		token, err := meshnode.MintBootstrapBiscuitToken(mesh.key, id, api.RoleNode, time.Now().Add(time.Hour), []*api.PolicyRole{{Name: api.RoleNode, AllowedServices: []string{"*"}, AllowedTargets: []string{"*"}, AllowedPublish: []string{"*"}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveIdentity(token); err != nil {
			t.Fatal(err)
		}
		return pid
	}
	pid := mint()
	writeSettings(t, dir, settings{Control: "https://control.test", Listen: "/ip4/127.0.0.1/tcp/0", LogLevel: "error", Agent: "codex", AgentDir: t.TempDir(), AgentName: "helper", AgentOwnConfig: true, PeerID: pid, NetworkID: uuid.NewString(), MeshURL: mesh.srv.URL})
	spec := runtimes["codex"]
	old := spec
	fix := newFixture()
	spec.new = func(string, string, agentPolicy) (runtime, error) { return fix, nil }
	runtimes["codex"] = spec
	t.Cleanup(func() { runtimes["codex"] = old })

	exercise := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		saved, err := loadSettings(dir)
		if err != nil {
			t.Fatal(err)
		}
		c := saved.config(dir)
		agent, err := newAgent(&c)
		if err != nil {
			t.Fatal(err)
		}
		store, err := meshnode.NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		node, err := meshnode.NewAgentMeshNode(meshnode.Options{PrivKey: meshnode.GetOrGenerateKey(store), Store: store, ControlPlanePubKey: mesh.key.Public().(ed25519.PublicKey), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}, RouterAddrs: []multiaddr.Multiaddr{routerAddr}, AllowLoopback: true, NodeConfig: &meshnode.NodeConfigComplete{}})
		if err != nil {
			t.Fatal(err)
		}
		defer node.Teardown()
		if err := node.Start(ctx); err != nil {
			t.Fatal(err)
		}
		targetURL, err := serveAgent(ctx, agent, c.agentName)
		if err != nil {
			t.Fatal(err)
		}
		if err := node.RegisterService(ctx, &api.RegisterServiceRequest{Service: &api.ServiceInfo{Type: api.ServiceType_SERVICE_TYPE_A2A, Name: c.agentName}, Backend: &api.RegisterServiceRequest_TargetUrl{TargetUrl: targetURL}}); err != nil {
			t.Fatal(err)
		}
		sidecar, err := meshnode.StartSidecarServer(node, "", filepath.Join(dir, meshnode.DefaultSocketName), "", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		defer sidecar.Shutdown(context.Background())
		for !node.IsConnected() {
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("router not admitted")
			}
		}
		client := socketClient(filepath.Join(dir, meshnode.DefaultSocketName))
		toBridge, fromClient := io.Pipe()
		toClient, fromBridge := io.Pipe()
		bridged := make(chan error, 1)
		go func() {
			bridged <- bridgeMCP(ctx, &mcp.IOTransport{Reader: toBridge, Writer: fromBridge}, filepath.Join(dir, meshnode.DefaultSocketName))
		}()
		session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: toClient, Writer: fromClient}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			session.Close()
			select {
			case err := <-bridged:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("MCP bridge did not stop")
			}
		}()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "discover_remote_services", Arguments: map[string]any{"type": "a2a", "name": "helper"}})
		if err != nil || result.IsError {
			t.Fatalf("discovery: %+v, %v", result, err)
		}
		var rows []*api.DiscoveredProvider
		if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Fatalf("remote discovery included this node: %+v", rows)
		}
		local, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_local_services", Arguments: map[string]any{"type": "a2a"}})
		if err != nil || local.IsError {
			t.Fatalf("local inventory: %+v %v", local, err)
		}
		var services []*api.ServiceInfo
		if err := json.Unmarshal([]byte(local.Content[0].(*mcp.TextContent).Text), &services); err != nil {
			t.Fatal(err)
		}
		if len(services) != 1 || services[0].Name != "helper" {
			t.Fatalf("local services: %+v", services)
		}
		localAPI, err := serveLocalAgent(ctx, node, &meshnode.NodeConfigComplete{}, c.agentName, targetURL, dir)
		if err != nil {
			t.Fatal(err)
		}
		defer localAPI.Close()
		tasks := &a2aTasks{http: client, base: "http://agentiscaled", local: socketClient(filepath.Join(dir, localAgentSocketName))}
		target := taskTarget{PeerID: pid, Service: "helper"}
		fix.release <- "completed locally"
		// Omitting peer_id must work through the real bridge and its tool schema.
		sentResult, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "send_task", Arguments: map[string]any{"service": "helper", "text": "review locally"}})
		if err != nil || sentResult.IsError {
			t.Fatalf("local send_task through MCP: %+v %v", sentResult, err)
		}
		var sent taskOut
		if err := json.Unmarshal([]byte(sentResult.Content[0].(*mcp.TextContent).Text), &sent); err != nil {
			t.Fatal(err)
		}
		select {
		case <-fix.prompts:
		case <-ctx.Done():
			t.Fatal("runtime did not start")
		}
		_, got, err := tasks.get(ctx, nil, getTaskIn{taskIn: taskIn{taskTarget: target, TaskID: sent.TaskID}, WaitSeconds: 5})
		if err != nil || got.Terminal != "completed" || got.Result != "completed locally" {
			t.Fatalf("task: %+v %v", got, err)
		}
		// Local task controls and delegation limits use the ordinary agent handler.
		a2aClient, err := tasks.client(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("nested"))
		msg.SetMeta(metaDelegationDepth, 1)
		response, err := a2aClient.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
		if err != nil {
			t.Fatal(err)
		}
		rejected, ok := response.(*a2a.Task)
		if !ok {
			t.Fatalf("delegation response %T", response)
		}
		_, rejection, err := tasks.get(ctx, nil, getTaskIn{taskIn: taskIn{taskTarget: target, TaskID: string(rejected.ID)}, WaitSeconds: 5})
		if err != nil || rejection.Terminal != "rejected" {
			t.Fatalf("nested delegation: %+v %v", rejection, err)
		}
		_, pending, err := tasks.send(ctx, nil, sendTaskIn{taskTarget: target, Text: "cancel locally"})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-fix.prompts:
		case <-ctx.Done():
			t.Fatal("runtime did not start")
		}
		_, stopped, err := tasks.cancel(ctx, nil, taskIn{taskTarget: target, TaskID: pending.TaskID})
		if err != nil || stopped.Terminal != "cancelled" {
			t.Fatalf("cancellation: %+v %v", stopped, err)
		}

	}
	exercise()
	exercise() // the same identity and settings after restart
	oldEnroll := enrollNew
	enrollNew = func(context.Context, config) (string, error) { pid = mint(); return uuid.NewString(), nil }
	t.Cleanup(func() { enrollNew = oldEnroll })
	auth := newFakeAuth0(t, "")
	if err := runReset(context.Background(), []string{"-data-dir", dir, "-auth-issuer", auth.config().issuer, "-auth-client-id", "CLIENT", "-auth-audience", "https://control.test"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	// The control plane's key is unchanged; the node identity has been replaced.
	exercise()
}
