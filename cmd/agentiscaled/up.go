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
	Agent        string `json:"agent,omitempty"`
	AgentDir     string `json:"agent_dir,omitempty"`
	AgentCommand string `json:"agent_command,omitempty"`
	AgentName    string `json:"agent_name,omitempty"`
	// AgentDescription replaces the runtime's description on the card.
	AgentDescription string `json:"agent_description,omitempty"`
	// What the agent may not take from AgentDir; it takes all when absent.
	AgentIgnoreDirInstructions bool `json:"agent_ignore_dir_instructions,omitempty"`
	AgentIgnoreDirSkills       bool `json:"agent_ignore_dir_skills,omitempty"`
	AgentOwnConfig             bool `json:"agent_own_config,omitempty"`
	// AgentEnv locates the owner's configuration of the agent's runtime,
	// such as CODEX_HOME, as `up` found it (agentOwnerEnv).
	AgentEnv    []string `json:"agent_env,omitempty"`
	MachineName string   `json:"machine_name,omitempty"`
	// PATH for the service, captured at `up`, since launchd and systemd
	// start services with a minimal one and an agent CLI may need more.
	Path string `json:"path,omitempty"`

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
		control:          s.Control,
		dataDir:          dataDir,
		configFile:       s.ConfigFile,
		bindAddr:         s.BindAddr,
		apiTokenPath:     s.APITokenPath,
		listen:           s.Listen,
		logLevel:         s.LogLevel,
		agent:            s.Agent,
		agentDir:         s.AgentDir,
		agentCommand:     s.AgentCommand,
		agentName:        s.AgentName,
		agentDescription: s.AgentDescription,
		agentPolicy:      agentPolicy{DirInstructions: !s.AgentIgnoreDirInstructions, DirSkills: !s.AgentIgnoreDirSkills, OwnerConfig: !s.AgentOwnConfig},
		machineName:      s.MachineName,
		agentEnv:         s.AgentEnv,
		networkID:        s.network(),
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
func runUp(ctx context.Context, args []string, out io.Writer) (runErr error) {
	var c config
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	nodeFlags(fs, &c)
	fs.StringVar(&c.control, "control", defaultControl, "URL of the agentiscale control service")
	var auth authConfig
	fs.StringVar(&auth.issuer, "auth-issuer", defaultAuth.issuer, "Auth0 tenant that signs you in")
	fs.StringVar(&auth.clientID, "auth-client-id", defaultAuth.clientID, "Auth0 application agentiscaled signs in with")
	fs.StringVar(&auth.audience, "auth-audience", defaultAuth.audience, "Auth0 API the sign-in is for")
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
	preserveAgentEnv := haveSaved && !set["agent"] && !set["agent-owner-config"]
	if err == nil {
		values := map[string]string{
			"control": saved.Control, "config": saved.ConfigFile, "bind-addr": saved.BindAddr,
			"api-token-path": saved.APITokenPath, "listen": saved.Listen, "log-level": saved.LogLevel,
			"agent": saved.Agent, "agent-dir": saved.AgentDir, "agent-command": saved.AgentCommand,
			"agent-name": saved.AgentName, "agent-description": saved.AgentDescription, "hostname": saved.MachineName,
			"agent-dir-instructions": strconv.FormatBool(!saved.AgentIgnoreDirInstructions),
			"agent-dir-skills":       strconv.FormatBool(!saved.AgentIgnoreDirSkills),
			"agent-owner-config":     strconv.FormatBool(!saved.AgentOwnConfig),
		}
		for name, value := range values {
			if !set[name] {
				if err := fs.Set(name, value); err != nil {
					return err
				}
			}
		}
		if preserveAgentEnv {
			c.agentEnv = saved.AgentEnv
		}
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
	if err := checkUpAgent(&c, !*noService, preserveAgentEnv); err != nil {
		return err
	}
	if err := checkClients(*clients); err != nil {
		return err
	}

	// Gather and validate changes while the existing node remains available.
	// Runtime login or an invalid workspace must not stop a working service.
	if ask != nil {
		if !set["clients"] {
			if *clients, err = ask.chooseClients(installedClients()); err != nil {
				return err
			}
		}
		if c.agent == "" && !set["agent"] {
			offer, err := ask.offerAgent()
			if err != nil {
				return err
			}
			if offer != nil {
				preserveAgentEnv = false
				offer.dataDir = c.dataDir
				if err := ask.prepareAgent(ctx, offer); err != nil {
					return err
				}
				c.agent, c.agentDir, c.agentName, c.agentDescription, c.agentPolicy = offer.runtime, offer.agentDir, offer.name, offer.description, offer.policy
				c.agentEnv = agentOwnerEnv(c.agent, c.agentPolicy)
				if err := checkAgent(&c, !*noService); err != nil {
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
	if !preserveAgentEnv {
		c.agentEnv = agentOwnerEnv(c.agent, c.agentPolicy)
	}
	s := settings{
		Control:          c.control,
		ConfigFile:       c.configFile,
		BindAddr:         c.bindAddr,
		APITokenPath:     c.apiTokenPath,
		Listen:           c.listen,
		LogLevel:         c.logLevel,
		Agent:            c.agent,
		AgentDir:         c.agentDir,
		AgentCommand:     c.agentCommand,
		AgentName:        c.agentName,
		AgentDescription: c.agentDescription,
		MachineName:      c.machineName,

		AgentIgnoreDirInstructions: !c.agentPolicy.DirInstructions,
		AgentIgnoreDirSkills:       !c.agentPolicy.DirSkills,
		AgentOwnConfig:             !c.agentPolicy.OwnerConfig,
		AgentEnv:                   c.agentEnv,
		Path:                       os.Getenv("PATH"),
		PeerID:                     joined.peerID,
		NetworkID:                  joined.networkID,
		MeshURL:                    joined.meshURL,
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
		nextSteps(ctx, out, *clients, c, exe)
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
	nextSteps(ctx, out, *clients, c, exe)
	return nil
}

// checkUpAgent limits the saved runtime environment to validation. Client
// setup and interactive questions use the invoking shell's configuration.
func checkUpAgent(c *config, service, preserveEnv bool) error {
	if preserveEnv && c.agentPolicy.OwnerConfig {
		for _, name := range runtimes[c.agent].ownerEnv {
			previous, existed := os.LookupEnv(name)
			defer func() {
				if existed {
					_ = os.Setenv(name, previous)
				} else {
					_ = os.Unsetenv(name)
				}
			}()
			value := ""
			for _, kv := range c.agentEnv {
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
	return checkAgent(c, service)
}

// nextSteps connects the chosen agent clients to the node, or prints how to.
func nextSteps(ctx context.Context, out io.Writer, clients string, c config, exe string) {
	switch {
	case clients == "none":
		printNextSteps(out, exe, c.dataDir)
	default:
		connectClients(ctx, out, selectClients(clients), exe, c.dataDir)
	}
}

// checkAgent checks an agent's settings before anything is enrolled or
// started, so a background service does not restart forever on a bad flag,
// and fixes the agent command to an absolute path that the service finds.
func checkAgent(c *config, service bool) error {
	if c.agent == "" {
		return nil
	}
	spec, ok := runtimes[c.agent]
	if !ok {
		return fmt.Errorf("-agent %q: unknown runtime (have %s)", c.agent, strings.Join(runtimeNames(), ", "))
	}
	if c.agentDir == "" {
		return fmt.Errorf("-agent %s needs -agent-dir", c.agent)
	}
	if c.agentName != "" {
		if err := agentNameError(c.agentName); err != nil {
			return fmt.Errorf("-agent-name: %w", err)
		}
	}
	dir, err := filepath.Abs(c.agentDir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("-agent-dir %s is not a directory", dir)
	}
	c.agentDir = dir
	if err := agentDirApart(dir, c.dataDir); err != nil {
		return err
	}
	command := c.agentCommand
	if command == "" {
		command = spec.defaultCommand
	}
	if path, err := exec.LookPath(command); err == nil {
		if abs, err := filepath.Abs(path); err == nil {
			c.agentCommand = abs
		}
	} else {
		return fmt.Errorf("agent command %q not found: %w", command, err)
	}
	// The runtime checks the rest, such as the agent's own sign-in.
	if _, err = spec.new(c.agentCommand, c.dataDir, c.agentPolicy); err != nil {
		return err
	}
	// A credential in this shell's environment does not reach the
	// background service.
	if service && spec.signedIn != nil && !spec.signedIn(c.dataDir, c.agentPolicy) {
		_, shown := spec.login(context.Background(), c.agentCommand, c.dataDir, c.agentPolicy)
		return fmt.Errorf("%s is signed in only through this shell's environment, which the background service does not get: run\n  %s\nor use -foreground and start 'agentiscaled run' from this shell", c.agent, shown)
	}
	return nil
}

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
	network, err := enrollOnly(ctx, *c)
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
	fmt.Fprintf(out, "Node:     %s (%s)\n", s.PeerID, s.MachineName)
	fmt.Fprintf(out, "Network:  %s\n", s.network())
	fmt.Fprintf(out, "Mesh:     %s\n", s.MeshURL)
	if s.Agent != "" {
		fmt.Fprintf(out, "Agent:    %s in %s\n", s.Agent, s.AgentDir)
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
