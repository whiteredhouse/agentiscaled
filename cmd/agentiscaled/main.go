// Command agentiscaled is the agentiscale node. It embeds the
// Google Agent Mesh node from agentiscale's fork (third_party/agentmesh)
// instead of running the agentmesh-node binary.
//
// `agentiscaled up` signs the owner in through the browser, joins the node
// to their network and keeps it running as a per-user service;
// started without a command, agentiscaled runs the node in the foreground,
// joining with a join code or a bootstrap token.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/agentmesh/api"
	meshnode "github.com/google/agentmesh/export/node"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

type config struct {
	controlPlane       string
	bootstrapTokenPath string
	control            string
	joinCodePath       string
	dataDir            string
	configFile         string
	bindAddr           string
	apiTokenPath       string
	listen             string
	logLevel           string
	agent              string
	agentDir           string
	agentCommand       string
	agentName          string
	agentDescription   string
	agentPolicy        agentPolicy
	// agentEnv is set in the node's environment before the agent's
	// runtime starts (agentOwnerEnv).
	agentEnv    []string
	machineName string
	// networkID is the network the node joined, which it asks the control
	// service about when it cannot reach it.
	networkID string

	// signInToken is the owner's Auth0 access token during `agentiscaled up`.
	// It lives in memory for enrollment only and is never stored.
	signInToken string
}

// defaultControl is agentiscale's hosted control service.
const defaultControl = "https://control.agentiscale.net"

const usage = `agentiscaled is the agentiscale node.

Commands:
  up       sign in, join this machine to your network and keep the node running
  down     stop the node's background service; its identity stays
  status   show the node's service, network and mesh connection
  version  print the version
  reset    give the node a new identity in your current network; its settings stay
  run      run the node in the foreground with the settings 'up' saved
  mcp      serve the node's MCP tools on stdio, for agent clients
  tasks    stop, resume or show the agent's task execution

Without a command, agentiscaled runs the node in the foreground with the
flags given (see agentiscaled -h).
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		var err error
		switch cmd, args := os.Args[1], os.Args[2:]; cmd {
		case "mcp":
			// Standard output carries MCP messages; errors go to standard error.
			err = runMCP(ctx, args)
		case "tasks":
			err = runTasks(args, os.Stdout)
		case "up":
			err = runUp(ctx, args, os.Stdout)
		case "down":
			err = runDown(args, os.Stdout)
		case "reset":
			err = runReset(ctx, args, os.Stdout)
		case "status":
			err = runStatus(ctx, args, os.Stdout)
		case "version":
			fmt.Println(versionString())
			return
		case "run":
			err = runSaved(ctx, args)
		case "help":
			fmt.Print(usage)
		default:
			fmt.Fprint(os.Stderr, usage)
			err = fmt.Errorf("unknown command %q", cmd)
		}
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	var c config
	fs := flag.CommandLine
	nodeFlags(fs, &c)
	fs.StringVar(&c.controlPlane, "control-plane", "", "https URL of the network's mesh (agentmesh-one)")
	fs.StringVar(&c.bootstrapTokenPath, "bootstrap-token-path", "", "File holding the bootstrap token; used only when the node has no identity yet")
	fs.StringVar(&c.control, "control", "", "URL of the agentiscale control service, used with --join-code-path")
	flag.Parse()
	if err := run(ctx, c); err != nil {
		log.Fatal(err)
	}
}

// nodeFlags registers the flags that configure a running node, shared by the
// foreground mode and `agentiscaled up`.
func nodeFlags(fs *flag.FlagSet, c *config) {
	fs.StringVar(&c.joinCodePath, "join-code-path", "", "File holding a join code from your network; used only when the node has no identity yet")
	fs.StringVar(&c.dataDir, "data-dir", defaultDataDir(), "Directory for the node's key, identity and socket")
	fs.StringVar(&c.configFile, "config", "", "Optional agentmesh-node.yaml (static services, labels)")
	fs.StringVar(&c.bindAddr, "bind-addr", "127.0.0.1:8080", "Local TCP address of the node API; empty serves only the Unix socket")
	fs.StringVar(&c.apiTokenPath, "api-token-path", "", "File holding the bearer token for the TCP node API (default <data-dir>/api-token, generated if missing)")
	fs.StringVar(&c.listen, "listen", "/ip4/0.0.0.0/udp/5001/quic-v1,/ip4/0.0.0.0/tcp/5002", "Comma-separated libp2p listen addresses")
	fs.StringVar(&c.logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	fs.StringVar(&c.agent, "agent", "", "Serve an A2A agent to the network, run by this runtime ("+strings.Join(runtimeNames(), ", ")+"); empty serves none")
	fs.StringVar(&c.agentDir, "agent-dir", "", "Directory the agent works in")
	fs.StringVar(&c.agentCommand, "agent-command", "", "Command of the agent CLI (default: the runtime's, e.g. codex)")
	fs.StringVar(&c.agentName, "agent-name", "", "Service name of the agent in the network (default: the runtime's)")
	fs.StringVar(&c.agentDescription, "agent-description", "", "What the agent does, as other agents see it on its card (default: the runtime's)")
	fs.BoolVar(&c.agentPolicy.DirInstructions, "agent-dir-instructions", true, "Let the agent load the instruction files (AGENTS.md) of -agent-dir; =false ignores them")
	fs.BoolVar(&c.agentPolicy.DirSkills, "agent-dir-skills", true, "Let the agent load the skills of -agent-dir; =false turns them off")
	fs.BoolVar(&c.agentPolicy.OwnerConfig, "agent-owner-config", true, "Run the agent with your own configuration of its runtime (sign-in, skills, plugins, MCP servers, settings); =false gives it its own in -data-dir")
	fs.StringVar(&c.machineName, "hostname", defaultMachineName(), "Name of this machine in your network's member list")
}

// defaultMachineName is the host name, without macOS's ".local".
func defaultMachineName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(name, ".local")
}

// agentDirApart refuses an agent directory that contains the node's data
// directory or lies inside it. The agent's commands may read the whole agent
// directory, and the data directory holds the node's key, its API token and
// the agent's own sign-in.
func agentDirApart(agentDir, dataDir string) error {
	a, d := realAbs(agentDir), realAbs(dataDir)
	if within(d, a) || within(a, d) {
		return fmt.Errorf("-agent-dir %s and -data-dir %s overlap: the agent could read the node's key and the agent's sign-in. Keep the data directory outside the agent directory", agentDir, dataDir)
	}
	return nil
}

// realAbs is path made absolute with symlinks resolved, as far as it exists.
func realAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	missing := ""
	for p := abs; ; p = filepath.Dir(p) {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, missing)
		}
		if filepath.Dir(p) == p {
			return abs
		}
		missing = filepath.Join(filepath.Base(p), missing)
	}
}

// within says whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// newAgent checks the agent's configuration before the node starts: a
// callable service needs a runtime that can run.
func newAgent(c *config) (*agentService, error) {
	spec, ok := runtimes[c.agent]
	if !ok {
		return nil, fmt.Errorf("-agent %q: unknown runtime (have %s)", c.agent, strings.Join(runtimeNames(), ", "))
	}
	if c.agentDir == "" {
		return nil, fmt.Errorf("-agent %s needs -agent-dir", c.agent)
	}
	dir, err := filepath.Abs(c.agentDir)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("-agent-dir %s is not a directory", dir)
	}
	if err := agentDirApart(dir, c.dataDir); err != nil {
		return nil, err
	}
	for _, kv := range c.agentEnv {
		name, value, _ := strings.Cut(kv, "=")
		if err := os.Setenv(name, value); err != nil {
			return nil, err
		}
	}
	if c.agentPolicy.OwnerConfig {
		if err := ownerEnv(context.Background(), c.dataDir); err != nil {
			return nil, err
		}
	}
	rt, err := spec.new(c.agentCommand, c.dataDir, c.agentPolicy)
	if err != nil {
		return nil, err
	}
	if c.agentName == "" {
		c.agentName = spec.defaultName
	}
	c.agentDir = dir
	s, err := newAgentService(dir, rt, c.dataDir)
	if err != nil {
		return nil, err
	}
	s.description = c.agentDescription
	return s, nil
}

func runtimeNames() []string {
	names := make([]string, 0, len(runtimes))
	for name := range runtimes {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func defaultDataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ".agentiscale"
	}
	return filepath.Join(dir, "agentiscale")
}

func run(ctx context.Context, c config) error {
	meshnode.SetupLogging(meshnode.LoggingOptions{Level: c.logLevel, Format: "text", RingBuffer: true})

	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return err
	}
	if h, err := loadHalt(c.dataDir); err != nil {
		return err
	} else if h != nil {
		return waitHalted(ctx, *h)
	}
	store, err := meshnode.NewStore(c.dataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	var agent *agentService
	if c.agent != "" {
		if agent, err = newAgent(&c); err != nil {
			return err
		}
	}

	nodeConfig, err := meshnode.LoadNodeConfig(c.configFile)
	if err != nil {
		return fmt.Errorf("load node config: %w", err)
	}
	// The node refuses a TCP API without a token, so settle it before enrolling.
	var apiToken string
	if c.bindAddr != "" {
		if apiToken, err = apiTokenFor(c); err != nil {
			return err
		}
	}

	priv := meshnode.GetOrGenerateKey(store)
	options, err := nodeOptions(c, store, priv)
	if err != nil {
		return err
	}

	identity, _ := store.LoadIdentity()
	if len(identity) == 0 {
		if _, err := enroll(ctx, c, store, priv, options); err != nil {
			return err
		}
	} else if stored, _ := store.LoadControlPlaneURL(); stored == "" && c.controlPlane != "" {
		// An earlier enrollment may have stored its credential and then
		// failed before the URL was saved.
		if err := store.SaveControlPlaneURL(c.controlPlane); err != nil {
			return fmt.Errorf("save control plane URL: %w", err)
		}
	}

	cpPub, routers, err := store.LoadMeshConfig()
	if err != nil {
		return fmt.Errorf("load mesh config: %w", err)
	}
	if len(cpPub) == 0 {
		return errors.New("stored identity has no control plane key; remove the data directory and enroll again")
	}
	node, err := meshnode.NewAgentMeshNode(options(cpPub, parseAddrs(routers)))
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	diag := diagnosis{dataDir: c.dataDir, control: c.control, network: c.networkID, priv: priv}
	if diag.network == "" {
		meshURL, _ := store.LoadControlPlaneURL()
		diag.network = networkFromMeshURL(meshURL)
	}
	if err := syncForDiagnosis(ctx, node); err != nil {
		log.Printf("control plane sync before start failed, using stored config: %v", err)
		if meshAnswerOf(err).suspect() {
			if h := diag.check(ctx, err); h != nil {
				return halt(ctx, c.dataDir, *h)
			}
		}
	}
	// The node runs until ctx ends or its network turns out to be gone.
	nodeCtx, stopNode := context.WithCancel(ctx)
	defer stopNode()
	if err := node.Start(nodeCtx); err != nil {
		if h := diag.check(ctx, err); h != nil {
			node.Teardown()
			return halt(ctx, c.dataDir, *h)
		}
		return fmt.Errorf("start node: %w", err)
	}
	var teardown sync.Once
	defer teardown.Do(func() { node.Teardown() })

	if nodeConfig != nil && len(nodeConfig.Services) > 0 {
		if err := node.RegisterStaticServices(ctx, nodeConfig.Services); err != nil {
			return fmt.Errorf("register services: %w", err)
		}
	}
	if agent != nil {
		target, err := serveAgent(nodeCtx, agent, c.agentName)
		if err != nil {
			return fmt.Errorf("start agent %s: %w", c.agentName, err)
		}
		if err := node.RegisterStaticServices(ctx, []api.ServiceConfig{{
			Type:        api.ServiceTypeStringA2A,
			Name:        c.agentName,
			Description: agent.card(c.agentName).Description,
			TargetURL:   target,
		}}); err != nil {
			return fmt.Errorf("register agent %s: %w", c.agentName, err)
		}
		localAPI, err := serveLocalAgent(nodeCtx, node, nodeConfig, c.agentName, target, c.dataDir)
		if err != nil {
			return fmt.Errorf("start local agent API: %w", err)
		}
		defer localAPI.Close()
		log.Printf("serving a2a://%s: %s agent in %s", c.agentName, c.agent, c.agentDir)
	}
	node.StartRenewalLoop(nodeCtx, "", "", "", "")

	socket := filepath.Join(c.dataDir, meshnode.DefaultSocketName)
	srv, err := meshnode.StartSidecarServer(node, c.bindAddr, socket, apiToken, "", "", "")
	if err != nil {
		return fmt.Errorf("start node API: %w", err)
	}
	log.Printf("agentiscaled running")

	var halted *nodeHalt
	select {
	case <-ctx.Done():
	case h := <-watchNetwork(nodeCtx, node, diag):
		halted = &h
	}
	stopNode()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("node API shutdown: %v", err)
	}
	if halted == nil {
		return nil
	}
	teardown.Do(func() { node.Teardown() })
	return halt(ctx, c.dataDir, *halted)
}

// nodeOptions builds the embedded node's options for its mesh's control plane
// key and routers, which a node learns when it enrolls.
func nodeOptions(c config, store *meshnode.Store, priv crypto.PrivKey) (func([]byte, []multiaddr.Multiaddr) meshnode.Options, error) {
	nodeConfig, err := meshnode.LoadNodeConfig(c.configFile)
	if err != nil {
		return nil, fmt.Errorf("load node config: %w", err)
	}
	return func(cpPub []byte, routers []multiaddr.Multiaddr) meshnode.Options {
		announcePrivate := true
		return meshnode.Options{
			PrivKey:                  priv,
			ControlPlanePubKey:       cpPub,
			RouterAddrs:              routers,
			Store:                    store,
			DiscoveryInterval:        meshnode.DefaultDiscoveryInterval,
			ListenAddrs:              splitList(c.listen),
			NodeConfig:               nodeConfig,
			KeyGracePeriod:           24 * time.Hour,
			AnnouncePrivateAddrs:     &announcePrivate,
			Reachability:             meshnode.ReachabilityPrivate,
			Routers:                  meshnode.DefaultRouters,
			MonitorBootstrap:         2 * time.Minute,
			MonitorInterval:          time.Minute,
			AutoRelayMinInterval:     30 * time.Second,
			AutoRelayBackoff:         3 * time.Second,
			RouterConnectTimeout:     meshnode.DefaultRouterConnectTimeout,
			RequiredRole:             api.RoleNode,
			ControlPlaneSyncInterval: meshnode.DefaultControlPlaneSyncInterval,
			BootstrapTokenPath:       c.bootstrapTokenPath,
			SecretsDir:               meshnode.DefaultSecretsDir,
		}
	}, nil
}

// enrollOnly enrolls a node that has no identity yet and stops, for
// `agentiscaled up`, which then hands the node to its background service.
func enrollOnly(ctx context.Context, c config) (networkID string, err error) {
	store, err := meshnode.NewStore(c.dataDir)
	if err != nil {
		return "", fmt.Errorf("open store: %w", err)
	}
	defer store.Close()
	priv := meshnode.GetOrGenerateKey(store)
	options, err := nodeOptions(c, store, priv)
	if err != nil {
		return "", err
	}
	return enroll(ctx, c, store, priv, options)
}

// enroll joins the network on first start: with a join code, which the
// control service exchanges for a bootstrap token on the network's mesh,
// or with a bootstrap token directly, the way agentmesh-node run
// --bootstrap-token-path does.
func enroll(ctx context.Context, c config, store *meshnode.Store, priv crypto.PrivKey, options func([]byte, []multiaddr.Multiaddr) meshnode.Options) (networkID string, err error) {
	var token string
	switch {
	case c.joinCodePath != "":
		if c.control == "" {
			return "", errors.New("--join-code-path needs --control, the control service URL")
		}
		code, err := readSecret(c.joinCodePath)
		if err != nil {
			return "", err
		}
		peerID, err := peer.IDFromPrivateKey(priv)
		if err != nil {
			return "", err
		}
		joined, err := redeemJoinCode(ctx, c.control, code, peerID.String(), c.machineName)
		if err != nil {
			return "", err
		}
		c.controlPlane, token, networkID = joined.MeshURL, joined.BootstrapToken, joined.NetworkID
		log.Printf("join code accepted for network %s", joined.NetworkID)
	case c.signInToken != "":
		peerID, err := peer.IDFromPrivateKey(priv)
		if err != nil {
			return "", err
		}
		joined, err := enrollSignedIn(ctx, c.control, c.signInToken, peerID.String(), c.machineName)
		if err != nil {
			return "", err
		}
		c.controlPlane, token, networkID = joined.MeshURL, joined.BootstrapToken, joined.NetworkID
	case c.controlPlane != "" && c.bootstrapTokenPath != "":
		var err error
		if token, err = readSecret(c.bootstrapTokenPath); err != nil {
			return "", err
		}
	default:
		return "", errors.New("no identity yet: pass --join-code-path, or --control-plane and --bootstrap-token-path, to enroll")
	}
	// EnrollBootstrap stores the credential before it joins a router, so
	// save the URL first: a failed join must not leave a credential without it.
	if err := store.SaveControlPlaneURL(c.controlPlane); err != nil {
		return "", fmt.Errorf("save control plane URL: %w", err)
	}
	enrollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	node, err := meshnode.NewAgentMeshNode(options(nil, nil))
	if err != nil {
		return "", fmt.Errorf("create node for enrollment: %w", err)
	}
	if err := node.Start(enrollCtx); err != nil {
		return "", fmt.Errorf("start node for enrollment: %w", err)
	}
	defer node.Teardown()
	if err := node.EnrollBootstrap(enrollCtx, c.controlPlane, token); err != nil {
		return "", fmt.Errorf("enroll: %w", err)
	}
	return networkID, nil
}

type joinAnswer struct {
	NetworkID      string `json:"network_id"`
	MeshURL        string `json:"mesh_url"`
	BootstrapToken string `json:"bootstrap_token"`
}

// redeemJoinCode asks the control service to admit this node to the code's
// network.
func redeemJoinCode(ctx context.Context, control, code, peerID, machineName string) (joinAnswer, error) {
	body := map[string]string{"code": code, "peer_id": peerID}
	if machineName != "" {
		body["machine_name"] = machineName
	}
	answer, _, err := postJoin(ctx, strings.TrimSuffix(control, "/")+"/v1/join", "", body)
	return answer, err
}

// postJoin sends a join or enrollment request to the control service. On a
// refusal it returns the status code with the error, so a caller can tell a
// network that is still starting (503) from a final answer.
func postJoin(ctx context.Context, url, bearer string, body map[string]string) (joinAnswer, int, error) {
	var answer joinAnswer
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return answer, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return answer, 0, fmt.Errorf("join: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return answer, resp.StatusCode, fmt.Errorf("join refused: %s (%s)", e.Error, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return answer, resp.StatusCode, fmt.Errorf("join: %w", err)
	}
	if answer.MeshURL == "" || answer.BootstrapToken == "" {
		return answer, resp.StatusCode, errors.New("join: the control service answered without a mesh URL or token")
	}
	return answer, resp.StatusCode, nil
}

// httpClient bounds every call to the control service and Auth0.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// apiTokenFor returns the TCP API token from --api-token-path, or from
// <data-dir>/api-token, generating that file on first start.
func apiTokenFor(c config) (string, error) {
	if c.apiTokenPath != "" {
		token, err := readSecret(c.apiTokenPath)
		if err == nil && token == "" {
			err = fmt.Errorf("%s is empty", c.apiTokenPath)
		}
		return token, err
	}
	path := filepath.Join(c.dataDir, "api-token")
	if token, err := readSecret(path); err == nil && token != "" {
		return token, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	// A new file replaces whatever is there, so the token never inherits
	// looser permissions from an existing empty file.
	tmp, err := os.CreateTemp(c.dataDir, ".api-token-*")
	if err != nil {
		return "", fmt.Errorf("write API token: %w", err)
	}
	_, err = tmp.WriteString(token + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("write API token: %w", err)
	}
	log.Printf("generated the node API token in %s", path)
	return token, nil
}

func readSecret(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func parseAddrs(addrs []string) []multiaddr.Multiaddr {
	out := make([]multiaddr.Multiaddr, 0, len(addrs))
	for _, s := range addrs {
		ma, err := multiaddr.NewMultiaddr(s)
		if err != nil {
			log.Printf("ignoring stored router address %q: %v", s, err)
			continue
		}
		out = append(out, ma)
	}
	return out
}
