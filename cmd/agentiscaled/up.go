package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	meshnode "github.com/google/agentmesh/export/node"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
)

// settings is what `agentiscaled up` keeps in <data-dir>/agentiscaled.json for
// the background service's `agentiscaled run`, and for `status`.
// It holds no secret: the node's key and credential stay in its store, and the
// owner's sign-in is never written.
type settings struct {
	Control      string `json:"control"`
	ConfigFile   string `json:"config,omitempty"`
	BindAddr     string `json:"bind_addr"`
	APITokenPath string `json:"api_token_path,omitempty"`
	Listen       string `json:"listen"`
	LogLevel     string `json:"log_level"`
	// Agents are the agents the node offers, which `agents add` and
	// `agents remove` change.
	Agents      []agentConfig `json:"agents,omitempty"`
	MachineName string        `json:"machine_name,omitempty"`
	// PATH for the service, captured at `up`, since launchd and systemd
	// start services with a minimal one and an agent CLI may need more.
	Path string `json:"path,omitempty"`
	// The owner's choices of agent clients and of running in the
	// foreground, which a later `up` keeps unless given again.
	Clients    string `json:"clients,omitempty"`
	Foreground bool   `json:"foreground,omitempty"`

	// What the node joined, shown by `status`.
	PeerID    string `json:"peer_id,omitempty"`
	NetworkID string `json:"network_id,omitempty"`
	MeshURL   string `json:"mesh_url,omitempty"`
}

const settingsFile = "agentiscaled.json"

func loadSettings(dataDir string) (settings, error) {
	var s settings
	b, err := os.ReadFile(filepath.Join(dataDir, settingsFile))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func saveSettings(dataDir string, s settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, settingsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s settings) config(dataDir string) config {
	return config{
		control:      s.Control,
		dataDir:      dataDir,
		configFile:   s.ConfigFile,
		bindAddr:     s.BindAddr,
		apiTokenPath: s.APITokenPath,
		listen:       s.Listen,
		logLevel:     s.LogLevel,
		agents:       s.Agents,
		machineName:  s.MachineName,
		networkID:    s.network(),
	}
}

// network is the network the node joined.
func (s settings) network() string {
	if s.NetworkID != "" {
		return s.NetworkID
	}
	return networkFromMeshURL(s.MeshURL)
}

// networkFromMeshURL reads the network ID from a hosted mesh URL, whose first
// host label is the ID. It names the network of a node that joined before `up`
// recorded it.
func networkFromMeshURL(meshURL string) string {
	u, err := url.Parse(meshURL)
	if err != nil {
		return ""
	}
	id, _, _ := strings.Cut(u.Hostname(), ".")
	if _, err := uuid.Parse(id); err != nil {
		return ""
	}
	return id
}

// runSaved is `agentiscaled run`, what the background service starts: the
// node in the foreground with the settings `up` saved.
func runSaved(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := loadSettings(*dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no settings in %s: run 'agentiscaled up' first", *dataDir)
	} else if err != nil {
		return err
	}
	return run(ctx, s.config(*dataDir))
}

// runUp is `agentiscaled up`: it signs the owner in through the
// browser if the node has no identity yet, joins the node to their network,
// saves its settings and (re)starts it as a per-user background service.
// In a terminal it first asks which agent clients to connect and whether to
// offer an agent, as `clients connect` and `agents add` do on their own.
func runUp(ctx context.Context, args []string, out io.Writer) error {
	return bringUp(ctx, args, out, upOptions{askClients: true, askAgent: true})
}

// upOptions are what differs between `up` and the commands that bring a
// node up on its behalf, such as `agents add`.
type upOptions struct {
	// addAgent is the agent that `agents add` offers from the node, in
	// place of a saved one of the same name. `up` keeps the saved agents
	// and adds one only through its wizard.
	addAgent *agentConfig
	// askClients and askAgent let the wizard ask which clients to connect
	// and whether to offer an agent.
	askClients, askAgent bool
}

func bringUp(ctx context.Context, args []string, out io.Writer, opts upOptions) (runErr error) {
	var c config
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	nodeFlags(fs, &c)
	fs.StringVar(&c.control, "control", defaultControl, "URL of the agentiscale control service")
	var auth authConfig
	fs.StringVar(&auth.issuer, "auth-issuer", defaultAuth.issuer, "OIDC issuer that signs you in")
	fs.StringVar(&auth.clientID, "auth-client-id", defaultAuth.clientID, "Public client ID agentiscaled signs in with")
	fs.StringVar(&auth.audience, "auth-audience", defaultAuth.audience, "API audience the sign-in is for")
	noService := fs.Bool("foreground", false, "Do not install a background service; join and exit, then run 'agentiscaled run' yourself")
	assumeYes := fs.Bool("y", false, "Ask nothing; take every default, as when 'up' runs without a terminal")
	clients := fs.String("clients", clientsAuto, "Agent clients to connect to this node: auto (every installed one, including when it hosts an agent), none, or a list of "+strings.Join(clientNames, ","))
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var ask *wizard
	if !*assumeYes && interactive() {
		ask = &wizard{in: bufio.NewReader(os.Stdin), out: out}
	}
	dataDir, err := filepath.Abs(c.dataDir)
	if err != nil {
		return err
	}
	c.dataDir = dataDir
	saved, err := loadSettings(c.dataDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load settings: %w", err)
	}
	haveSaved := err == nil
	if err == nil {
		values := map[string]string{
			"control": saved.Control, "config": saved.ConfigFile, "bind-addr": saved.BindAddr,
			"api-token-path": saved.APITokenPath, "listen": saved.Listen, "log-level": saved.LogLevel,
			"hostname":   saved.MachineName,
			"foreground": strconv.FormatBool(saved.Foreground),
			"clients":    saved.Clients,
		}
		for name, value := range values {
			if !set[name] && !(name == "clients" && value == "") {
				if err := fs.Set(name, value); err != nil {
					return err
				}
			}
		}
		c.agents = slices.Clone(saved.Agents)
	}
	// A second node on the machine must not take the first one's ports, so
	// unless told otherwise it listens on free ones and serves its API on its
	// Unix socket only.
	if c.dataDir != defaultDataDir() && errors.Is(err, os.ErrNotExist) {
		if !set["listen"] {
			c.listen = "/ip4/0.0.0.0/udp/0/quic-v1,/ip4/0.0.0.0/tcp/0"
		}
		if !set["bind-addr"] {
			c.bindAddr = ""
		}
	}
	for i := range c.agents {
		if err := checkSavedAgent(&c.agents[i], c.dataDir, !*noService); err != nil {
			return err
		}
	}
	if opts.addAgent != nil {
		if err := addAgent(&c, *opts.addAgent, !*noService); err != nil {
			return err
		}
	}
	if err := checkClients(*clients); err != nil {
		return err
	}

	// Gather and validate changes while the existing node remains available.
	// Runtime login or an invalid workspace must not stop a working service.
	if ask != nil {
		if opts.askClients && !set["clients"] && (!haveSaved || saved.Clients == "") {
			if *clients, err = ask.chooseClients(installedClients()); err != nil {
				return err
			}
		}
		if opts.askAgent && len(c.agents) == 0 {
			offer, err := ask.offerAgent()
			if err != nil {
				return err
			}
			if offer != nil {
				offer.dataDir = c.dataDir
				if err := ask.prepareAgent(ctx, offer); err != nil {
					return err
				}
				if err := addAgent(&c, offer.config(), !*noService); err != nil {
					return err
				}
			}
		}
	}

	exe, err := stableExecutable()
	if err != nil {
		return err
	}
	svc := newService(c.dataDir)
	wasRunning := false
	if svc != nil {
		_, wasRunning = svc.state()
		// Unload an installed job even between automatic restart attempts.
		if svc.installed() {
			if err := svc.stop(); err != nil {
				return err
			}
		}
	}
	settingsChanged := false
	// Once the store must be opened, recover only a service we actually stopped.
	// Restore its settings before restarting it; surface recovery failures too.
	defer func() {
		if runErr == nil || !wasRunning || !haveSaved {
			return
		}
		if settingsChanged {
			if err := saveSettings(c.dataDir, saved); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("restore previous node settings: %w", err))
				return
			}
		}
		if err := svc.start(serviceSpec{exe: exe, dataDir: c.dataDir, path: saved.Path}); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("restart previous background service: %w", err))
		} else {
			fmt.Fprintln(out, "Restored the previous background service and settings.")
		}
	}()
	joined, err := ensureJoined(ctx, &c, auth, out)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "This node is %s in network %s.\n", joined.peerID, joined.networkID)
	s := settings{
		Control:      c.control,
		ConfigFile:   c.configFile,
		BindAddr:     c.bindAddr,
		APITokenPath: c.apiTokenPath,
		Listen:       c.listen,
		LogLevel:     c.logLevel,
		Agents:       c.agents,
		MachineName:  c.machineName,
		Path:         os.Getenv("PATH"),
		Clients:      *clients,
		Foreground:   *noService,
		PeerID:       joined.peerID,
		NetworkID:    joined.networkID,
		MeshURL:      joined.meshURL,
	}
	if err := saveSettings(c.dataDir, s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	settingsChanged = true
	if *noService || svc == nil {
		if svc == nil {
			fmt.Fprintln(out, "No background service on this system; run the node yourself:")
		} else {
			fmt.Fprintln(out, "Run the node with:")
		}
		fmt.Fprintf(out, "  %s run -data-dir %s\n", shellQuote(exe), shellQuote(c.dataDir))
		if opts.askClients || *clients != "none" {
			nextSteps(ctx, out, *clients, c, exe)
		}
		return nil
	}
	if err := svc.start(serviceSpec{exe: exe, dataDir: c.dataDir, path: s.Path}); err != nil {
		if wasRunning && haveSaved {
			return fmt.Errorf("start updated background service: %w", err)
		}
		return fmt.Errorf("start the background service: %w\nThe node has joined; run it yourself with:\n  %s run -data-dir %s", err, shellQuote(exe), shellQuote(c.dataDir))
	}
	fmt.Fprintf(out, "Started the background service %s; it starts again when you log in.\n", svc.name())
	if hint := svc.hint(); hint != "" {
		fmt.Fprintln(out, hint)
	}
	if err := waitReady(ctx, c.dataDir, 30*time.Second); err != nil {
		fmt.Fprintf(out, "The node has not connected to the mesh yet (%v). See 'agentiscaled status' and %s.\n", err, filepath.Join(c.dataDir, logFile))
	} else {
		fmt.Fprintln(out, "Connected to the mesh.")
	}
	if opts.askClients || *clients != "none" {
		nextSteps(ctx, out, *clients, c, exe)
	}
	return nil
}

// checkSavedAgent checks a saved agent with the owner's configuration as
// it was saved, which may differ from this shell's. Client setup and
// interactive questions use the invoking shell's configuration.
func checkSavedAgent(a *agentConfig, dataDir string, service bool) error {
	if a.policy().OwnerConfig {
		for _, name := range runtimes[a.Runtime].ownerEnv {
			previous, existed := os.LookupEnv(name)
			defer func() {
				if existed {
					_ = os.Setenv(name, previous)
				} else {
					_ = os.Unsetenv(name)
				}
			}()
			value := ""
			for _, kv := range a.Env {
				key, v, _ := strings.Cut(kv, "=")
				if key == name {
					value = v
					break
				}
			}
			if err := os.Setenv(name, value); err != nil {
				return err
			}
		}
	}
	return checkAgent(a, dataDir, service)
}

// addAgent checks a new agent, with the owner's configuration as this shell
// has it, and adds it to the node's agents, in place of one of the same name.
func addAgent(c *config, a agentConfig, service bool) error {
	if err := checkAgent(&a, c.dataDir, service); err != nil {
		return err
	}
	a.Env = agentOwnerEnv(a.Runtime, a.policy())
	for i := range c.agents {
		if c.agents[i].Name == a.Name {
			c.agents[i] = a
			return nil
		}
	}
	c.agents = append(c.agents, a)
	return nil
}

// nextSteps connects the chosen agent clients to the node, or prints how to.
func nextSteps(ctx context.Context, out io.Writer, clients string, c config, exe string) {
	switch {
	case clients == "none":
		printNextSteps(out, exe, c.dataDir)
	default:
		connectClients(ctx, out, selectClients(clients), exe, c.dataDir)
	}
	dirFlag := ""
	if realAbs(c.dataDir) != realAbs(defaultDataDir()) {
		dirFlag = " -data-dir " + shellQuote(c.dataDir)
	}
	fmt.Fprintf(out, "\nConnect or disconnect agent clients later with 'agentiscaled clients connect%s' or 'clients disconnect'.\n", dirFlag)
	if len(c.agents) == 0 {
		fmt.Fprintln(out, "Offer an agent from this machine with 'agentiscaled agents add'.")
	}
}

// checkAgent checks an agent's settings before anything is enrolled or
// started, so a background service does not restart forever on a bad
// setting, and fixes its name to the runtime's default when it has none and
// its command to an absolute path that the service finds.
func checkAgent(a *agentConfig, dataDir string, service bool) error {
	spec, ok := runtimes[a.Runtime]
	if !ok {
		return fmt.Errorf("agent runtime %q: unknown (have %s)", a.Runtime, strings.Join(runtimeNames(), ", "))
	}
	if a.Name == "" {
		a.Name = spec.defaultName
	}
	if err := agentNameError(a.Name); err != nil {
		return err
	}
	if a.Dir == "" {
		return fmt.Errorf("agent %s has no directory to work in", a.Name)
	}
	dir, err := filepath.Abs(a.Dir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("agent %s: %s is not a directory", a.Name, dir)
	}
	a.Dir = dir
	if err := agentDirApart(dir, dataDir); err != nil {
		return err
	}
	command := a.Command
	if command == "" {
		command = spec.defaultCommand
	}
	if path, err := exec.LookPath(command); err == nil {
		if abs, err := filepath.Abs(path); err == nil {
			a.Command = abs
		}
	} else {
		return fmt.Errorf("agent command %q not found: %w", command, err)
	}
	policy := a.policy()
	if a.Definition != "" {
		if _, err := loadDefinition(definitionCopy(dataDir, a.Name, a.Definition)); err != nil {
			return fmt.Errorf("agent %s: its definition: %w; run 'agentiscaled agents add' with -definition again", a.Name, err)
		}
	}
	// The runtime checks the rest, such as the agent's own sign-in.
	if _, err = spec.new(a.Command, dataDir, policy); err != nil {
		return err
	}
	// A credential in this shell's environment does not reach the
	// background service.
	if service && spec.signedIn != nil && !spec.signedIn(a.Command, dataDir, policy) {
		_, shown := spec.login(context.Background(), a.Command, dataDir, policy)
		return fmt.Errorf("%s is signed in only through this shell's environment, which the background service does not get: run\n  %s\nor use -foreground and start 'agentiscaled run' from this shell", a.Runtime, shown)
	}
	return nil
}

// enrollNew enrolls the node with its new key; tests stand in for the mesh.
var enrollNew = enrollOnly

type joinedNode struct {
	peerID    string
	networkID string
	meshURL   string
}

// ensureJoined enrolls the node unless it already has an identity: with a
// join code if one is given, otherwise on the owner's sign-in through the
// browser. A joined node is left as it is.
func ensureJoined(ctx context.Context, c *config, auth authConfig, out io.Writer) (joinedNode, error) {
	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return joinedNode{}, err
	}
	store, err := meshnode.NewStore(c.dataDir)
	if err != nil && strings.Contains(err.Error(), "file lock") {
		return joinedNode{}, fmt.Errorf("another agentiscaled is using %s; stop it first", c.dataDir)
	} else if err != nil {
		return joinedNode{}, fmt.Errorf("open store: %w", err)
	}
	identity, _ := store.LoadIdentity()
	meshURL, _ := store.LoadControlPlaneURL()
	priv := meshnode.GetOrGenerateKey(store)
	store.Close()
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return joinedNode{}, err
	}
	if len(identity) > 0 && meshURL != "" {
		if h, err := loadHalt(c.dataDir); err != nil {
			return joinedNode{}, err
		} else if h != nil {
			return joinedNode{}, errors.New(h.Message)
		}
		fmt.Fprintln(out, "This node has already joined; keeping its identity.")
		network := networkFromMeshURL(meshURL)
		if s, err := loadSettings(c.dataDir); err == nil && s.NetworkID != "" {
			network = s.NetworkID
		}
		return joinedNode{peerID: pid.String(), networkID: network, meshURL: meshURL}, nil
	}
	if c.joinCodePath == "" {
		token, err := deviceLogin(ctx, auth, out)
		if err != nil {
			return joinedNode{}, err
		}
		c.signInToken = token
		defer func() { c.signInToken = "" }()
	}
	// A stop recorded for an identity the node no longer has, as after
	// `reset`, does not apply to the new one. Clear it before enrolling: an
	// enrollment that saves its credential and then fails to reach a router
	// is retried as an already joined node, which a stale stop would block.
	if err := os.Remove(filepath.Join(c.dataDir, haltFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return joinedNode{}, err
	}
	network, err := enrollNew(ctx, *c)
	if err != nil {
		return joinedNode{}, err
	}
	store, err = meshnode.NewStore(c.dataDir)
	if err != nil {
		return joinedNode{}, fmt.Errorf("open store: %w", err)
	}
	defer store.Close()
	meshURL, _ = store.LoadControlPlaneURL()
	return joinedNode{peerID: pid.String(), networkID: network, meshURL: meshURL}, nil
}

// enrollSignedIn asks the control service to admit this node to the signed-in
// owner's network. A network whose mesh is still starting, as on
// a first sign-in, answers 503, so it asks again for up to five minutes.
func enrollSignedIn(ctx context.Context, control, token, peerID, machineName string) (joinAnswer, error) {
	body := map[string]string{"peer_id": peerID}
	if machineName != "" {
		body["machine_name"] = machineName
	}
	deadline := time.Now().Add(enrollWait)
	told := false
	for {
		answer, status, err := postJoin(ctx, strings.TrimSuffix(control, "/")+"/v1/enroll", token, body)
		if status == http.StatusForbidden && err != nil && strings.Contains(err.Error(), "scheduled for deletion") {
			// Account management is in the console, not in agentiscaled.
			return answer, fmt.Errorf("%w; restore your account in the console (%s), then try again", err, consoleURL)
		}
		if status != http.StatusServiceUnavailable || time.Now().After(deadline) {
			return answer, err
		}
		if !told {
			log.Printf("your network is starting; waiting for it (this takes about a minute the first time)")
			told = true
		}
		select {
		case <-ctx.Done():
			return answer, ctx.Err()
		case <-time.After(enrollRetry):
		}
	}
}

// consoleURL is where owners manage their account and network.
const consoleURL = "https://console.agentiscale.net"

var (
	enrollWait  = 5 * time.Minute
	enrollRetry = 5 * time.Second
)

// runDown is `agentiscaled down`: it stops the background service and removes
// it, so it does not start at login; the node's identity and settings stay,
// and `up` starts it again without signing in.
func runDown(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	svc := newService(dir)
	if svc == nil {
		return errors.New("no background service on this system")
	}
	if err := svc.remove(); err != nil {
		return err
	}
	fmt.Fprintf(out, "Stopped %s. The node keeps its identity; 'agentiscaled up' starts it again.\n", svc.name())
	return nil
}

// runStatus is `agentiscaled status`.
func runStatus(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	s, err := loadSettings(dir)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(out, "No node set up in %s. Run 'agentiscaled up'.\n", dir)
		return nil
	} else if err != nil {
		return err
	}
	fmt.Fprintf(out, "Data:     %s\n", dir)
	if s.PeerID == "" {
		fmt.Fprintf(out, "Node:     not joined (%s)\n", s.MachineName)
		fmt.Fprint(out, rejoinSteps(dir, true))
	} else {
		fmt.Fprintf(out, "Node:     %s (%s)\n", s.PeerID, s.MachineName)
		fmt.Fprintf(out, "Network:  %s\n", s.network())
		fmt.Fprintf(out, "Mesh:     %s\n", s.MeshURL)
	}
	for _, a := range s.Agents {
		fmt.Fprintf(out, "Agent:    %s (%s) in %s\n", a.Name, a.Runtime, a.Dir)
	}
	// The owner's stop holds for all of the node's agents.
	if b, err := os.ReadFile(stopFilePath(dir)); err == nil {
		fmt.Fprintf(out, "Tasks:    stopped since %s; 'agentiscaled tasks resume' takes them again\n", strings.TrimSpace(string(b)))
	} else if len(s.Agents) > 0 {
		fmt.Fprintln(out, "Tasks:    accepted")
	}
	serviceRuns := false
	if svc := newService(dir); svc != nil {
		var state string
		state, serviceRuns = svc.state()
		fmt.Fprintf(out, "Service:  %s, %s\n", svc.name(), state)
	}
	if h, err := loadHalt(dir); err != nil {
		return err
	} else if h != nil {
		fmt.Fprintf(out, "Mesh connection: %s\n", h.Message)
		return nil
	}
	switch err := nodeReady(ctx, dir); {
	case err == nil:
		fmt.Fprintln(out, "Mesh connection: connected")
	case serviceRuns && errors.Is(err, errNodeNotRunning):
		// The node opens its socket once it has reached its network.
		fmt.Fprintf(out, "Mesh connection: node starting; see %s\n", filepath.Join(dir, logFile))
	default:
		fmt.Fprintf(out, "Mesh connection: %v\n", err)
	}
	return nil
}

var errNodeNotRunning = errors.New("node not running")

// nodeReady asks the running node, over its Unix socket, whether it holds an
// authenticated connection to its network's router.
func nodeReady(ctx context.Context, dataDir string) error {
	socket := filepath.Join(dataDir, meshnode.DefaultSocketName)
	if _, err := os.Stat(socket); err != nil {
		return errNodeNotRunning
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://node/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := socketClient(socket).Do(req)
	if err != nil {
		return errNodeNotRunning
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("not connected to the mesh")
	}
	return nil
}

func waitReady(ctx context.Context, dataDir string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := nodeReady(ctx, dataDir)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// printNextSteps tells the owner how to connect their agent clients to this
// node (docs/connect-agents.md).
func printNextSteps(out io.Writer, exe, dataDir string) {
	name, e := clientEntryName(dataDir), mcpEntryFor(exe, dataDir)
	fmt.Fprintf(out, "\nConnect your agents to this node:\n\n  Codex CLI:\n%s\n  Claude Desktop:\n%s", newCodexClient().step(name, e), newClaudeDesktop().step(name, e))
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:@+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
