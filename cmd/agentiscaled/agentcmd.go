package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	meshnode "github.com/google/agentmesh/export/node"
)

const agentUsage = `usage: agentiscaled agent add|remove|list

  add [RUNTIME] [-dir DIR] [-name NAME] ...  offer an agent from this machine to your network
                                              (-definition FILE gives it a subagent's role)
  remove [NAME]                               stop offering an agent
  list                                        show agents on this machine, on mesh peers and in the cloud
  list -location local|mesh|cloud             show agents in one location

Run 'agentiscaled agent add -h' for its flags.`

// runAgent manages agents offered from this machine and discovers agents across the network.
func runAgent(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "help" {
		return errors.New(agentUsage)
	}
	switch args[0] {
	case "add":
		return runAgentAdd(ctx, args[1:], out)
	case "remove":
		return runAgentRemove(args[1:], out)
	case "list":
		return runAgentList(ctx, args[1:], out)
	}
	return fmt.Errorf("unknown agent command %q\n%s", args[0], agentUsage)
}

func runAgentAdd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent add", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node that offers the agent")
	dir := fs.String("dir", "", "Directory the agent works in (default: a new workspace of its own)")
	name := fs.String("name", "", "Name of the agent in your network (default: the runtime's)")
	description := fs.String("description", "", "What the agent does, as other agents see it on its card (default: the runtime's)")
	command := fs.String("command", "", "Command of the agent CLI (default: the runtime's, e.g. codex)")
	definition := fs.String("definition", "", "Subagent definition to give the agent its role: a Claude Code agent file (.claude/agents/NAME.md) or a Codex agent file (.codex/agents/NAME.toml). Either runtime runs either format: its instructions join the system prompt of each task. It also sets the default name, description and directory (the project it is in)")
	var policy agentPolicy
	fs.BoolVar(&policy.DirInstructions, "dir-instructions", true, "Let the agent load the instruction files (AGENTS.md, CLAUDE.md) of its directory; =false ignores them")
	fs.BoolVar(&policy.DirSkills, "dir-skills", true, "Let the agent load the skills of its directory; =false turns them off")
	fs.BoolVar(&policy.OwnerConfig, "owner-config", true, "Run the agent with your own configuration of its runtime (sign-in, skills, plugins, MCP servers, settings and permissions); =false uses an isolated read-only configuration in the node's data directory")
	assumeYes := fs.Bool("y", false, "Ask nothing; take every default")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentiscaled agent add [RUNTIME] [flags]\n\nRUNTIME is one of %s; in a terminal, without one, add asks.\n\n", strings.Join(runtimeNames(), ", "))
		fs.PrintDefaults()
	}
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(rest[1:], " "))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	// Joining is up's job: a sign-in and a new node in the network should
	// not happen as a side effect of offering an agent. Check before any
	// question is asked.
	nodeDir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	if s, err := loadSettings(nodeDir); errors.Is(err, os.ErrNotExist) || (err == nil && s.PeerID == "") {
		return notJoinedError(nodeDir)
	} else if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	var ask *wizard
	if !*assumeYes && interactive() {
		ask = &wizard{in: bufio.NewReader(os.Stdin), out: out}
	}
	var def *agentDefinition
	defPath := ""
	if *definition != "" {
		if defPath, err = filepath.Abs(*definition); err != nil {
			return err
		}
		if def, err = loadDefinition(defPath); err != nil {
			return err
		}
	}
	var offer *agentOffer
	switch {
	case len(rest) == 1:
		rt := rest[0]
		spec, ok := runtimes[rt]
		if !ok {
			return fmt.Errorf("unknown runtime %q (have %s)", rt, strings.Join(runtimeNames(), ", "))
		}
		offer = &agentOffer{runtime: rt, name: *name, description: *description, agentDir: *dir, policy: policy}
		if def != nil {
			if ignored := def.ignoredOn(rt); len(ignored) > 0 {
				fmt.Fprintf(out, "%s is a %s agent file: %s ignores its %s, and runs with its own default model and the agent's permissions.\n",
					filepath.Base(defPath), runtimeTitle(def.WrittenFor), rt, strings.Join(ignored, ", "))
			}
			if offer.name == "" {
				offer.name = def.Name
			}
			if offer.agentDir == "" {
				offer.agentDir = definitionRoot(defPath)
			}
		}
		if offer.name == "" {
			offer.name = spec.defaultName
		}
		if !validAgentName(offer.name) {
			return fmt.Errorf("-name %q: use up to 63 lowercase letters, digits and dashes", offer.name)
		}
		if offer.agentDir == "" {
			offer.agentDir = defaultAgentDir(offer.name)
			if err := os.MkdirAll(offer.agentDir, 0o700); err != nil {
				return err
			}
		}
	case ask != nil:
		for _, f := range []string{"dir", "name", "description", "command", "definition"} {
			if set[f] {
				return fmt.Errorf("-%s needs a runtime: agentiscaled agent add %s -%s …", f, strings.Join(runtimeNames(), "|"), f)
			}
		}
		if offer, err = ask.chooseAgent(false); err != nil {
			return err
		} else if offer == nil {
			return errors.New("no agent offered")
		}
	default:
		return fmt.Errorf("name the agent's runtime: agentiscaled agent add %s [-dir DIR]", strings.Join(runtimeNames(), "|"))
	}

	offer.command = *command
	if fi, err := os.Stat(offer.agentDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("-dir %s is not a directory", offer.agentDir)
	}
	if offer.agentDir, err = filepath.Abs(offer.agentDir); err != nil {
		return err
	}
	if offer.dataDir, err = filepath.Abs(*dataDir); err != nil {
		return err
	}
	if a, d := realAbs(offer.agentDir), realAbs(offer.dataDir); within(d, a) || within(a, d) {
		return fmt.Errorf("the agent's directory %s and the node's data directory %s overlap: the agent could read the node's key and its own sign-in. Pick a -dir outside the data directory", offer.agentDir, offer.dataDir)
	}
	if ask != nil {
		if err := ask.prepareAgent(ctx, offer); err != nil {
			return err
		}
	}
	// The node runs the agent with its own copy of the definition, which
	// only another `agent add` replaces.
	if defPath != "" {
		if offer.definition, err = copyDefinition(offer.dataDir, offer.name, defPath); err != nil {
			return err
		}
	}
	replaced := false
	if s, err := loadSettings(offer.dataDir); err == nil {
		replaced = slices.ContainsFunc(s.Agents, func(a agentConfig) bool { return a.Name == offer.name })
	}
	// The node has joined (checked above, and again by bringUp, which never
	// enrolls for agent add); a node that runs restarts to serve the agent
	// next to the ones it offers already.
	add := offer.config()
	if err := bringUp(ctx, []string{"-data-dir", offer.dataDir, "-y"}, out, upOptions{addAgent: &add, noEnroll: true}); err != nil {
		return err
	}
	if defPath == "" {
		removeDefinitionCopies(offer.dataDir, offer.name)
	}
	verb := "has"
	if replaced {
		verb = "has the changed"
	}
	fmt.Fprintf(out, "\nYour network %s agent %q from this machine. 'agentiscaled agent list -location local' shows this machine's agents; 'agentiscaled agent remove %s' stops offering it.\n", verb, offer.name, offer.name)
	return nil
}

// notJoinedError is what `agent add` says on a node that has not joined.
func notJoinedError(dataDir string) error {
	return fmt.Errorf("this machine has not joined your network: run '%s' first; it can offer the agent too", upCommand(dataDir))
}

func runAgentRemove(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent remove", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node that offers the agent")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(rest[1:], " "))
	}
	dir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	s, err := loadSettings(dir)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("this machine offers no agent")
	} else if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	var names []string
	for _, a := range s.Agents {
		names = append(names, a.Name)
	}
	var name string
	switch {
	case len(rest) == 1 && !slices.Contains(names, rest[0]):
		return fmt.Errorf("this machine offers no agent named %q; see 'agentiscaled agent list -location local'", rest[0])
	case len(rest) == 1:
		name = rest[0]
	case len(names) == 0:
		return errors.New("this machine offers no agent")
	case len(names) > 1:
		return fmt.Errorf("name the agent to remove: %s", strings.Join(names, ", "))
	default:
		name = names[0]
	}
	s.Agents = slices.DeleteFunc(s.Agents, func(a agentConfig) bool { return a.Name == name })
	if err := saveSettings(dir, s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	removeDefinitionCopies(dir, name)
	fmt.Fprintf(out, "This machine no longer offers %q; its directory stays as it is.\n", name)
	return restartNode(dir, s, newService(dir), out)
}

// restartNode restarts a node's background service so that it runs with
// changed settings, or says how to when it runs in the foreground.
func restartNode(dir string, s settings, svc service, out io.Writer) error {
	if svc == nil || s.Foreground || !svc.installed() {
		fmt.Fprintf(out, "Restart the node to apply it: agentiscaled run -data-dir %s\n", shellQuote(dir))
		return nil
	}
	exe, err := stableExecutable()
	if err != nil {
		return err
	}
	if err := svc.stop(); err != nil {
		return err
	}
	if err := svc.start(serviceSpec{exe: exe, dataDir: dir, path: s.Path}); err != nil {
		return fmt.Errorf("restart the background service: %w", err)
	}
	fmt.Fprintf(out, "Restarted %s.\n", svc.name())
	return nil
}

func runAgentList(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent list", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node")
	location := fs.String("location", "", "Filter agents by location: local, mesh, or cloud (default: all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if err := validateAgentLocation(*location); err != nil {
		return err
	}

	includeLocal := *location == "" || *location == "local"
	var s settings
	if includeLocal {
		var err error
		s, err = loadSettings(*dataDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("load settings: %w", err)
		}
	}
	mesh := socketClient(filepath.Join(*dataDir, meshnode.DefaultSocketName))
	local := socketClient(filepath.Join(*dataDir, localAgentSocketName))
	defer mesh.CloseIdleConnections()
	defer local.CloseIdleConnections()
	tasks := &a2aTasks{http: mesh, base: "http://agentiscaled", local: local}
	rows, discoveryErr := tasks.discoverAgents(ctx, *location)
	if err := ctx.Err(); err != nil {
		return err
	}
	if *location == "local" && errors.Is(discoveryErr, syscall.ECONNREFUSED) {
		// A stopped daemon may leave its Unix socket behind. Its configured
		// local inventory is still available on disk.
		discoveryErr = nil
	}

	// Settings also describe agents added since the node started and those
	// on a stopped node. Keep them visible and label their actual state.
	liveLocal := map[string]bool{}
	for _, row := range rows {
		if row.Location == "local" {
			liveLocal[row.Service] = true
		}
	}
	configured := map[string]agentConfig{}
	for _, a := range s.Agents {
		configured[a.Name] = a
		if !liveLocal[a.Name] {
			rows = append(rows, agentRow{Service: a.Name, PeerID: s.PeerID, Location: "local"})
		}
	}
	if len(rows) == 0 {
		if discoveryErr != nil {
			return discoveryErr
		}
		switch *location {
		case "local":
			fmt.Fprintln(out, "This machine offers no agent. Offer one with 'agentiscaled agent add'.")
		case "":
			fmt.Fprintln(out, "No agents are visible to this node. Offer one with 'agentiscaled agent add'.")
		default:
			fmt.Fprintf(out, "No %s agents are visible to this node.\n", *location)
		}
	} else if err := printAgentList(out, *dataDir, rows, configured); err != nil {
		return err
	}
	if discoveryErr != nil {
		fmt.Fprintf(out, "Discovery incomplete: %s\n", terminalSafe(discoveryErr.Error()))
	}
	return nil
}

func printAgentList(out io.Writer, dir string, rows []agentRow, configured map[string]agentConfig) error {
	slices.SortFunc(rows, compareAgentRows)
	var activity map[string]agentActivity
	var running bool
	if slices.ContainsFunc(rows, func(r agentRow) bool { return r.Location == "local" }) {
		activity, running = nodeActivity(dir)
	}
	header := []string{"NAME", "LOCATION", "STATE", "TARGET"}
	if len(configured) > 0 {
		header = append(header, "RUNTIME", "DIRECTORY")
	}
	withDefinition := false
	for _, a := range configured {
		if a.Definition != "" {
			withDefinition = true
			header = append(header, "DEFINITION")
			break
		}
	}

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	now := time.Now()
	for _, row := range rows {
		target := row.PeerID
		if target == "" {
			target = "-"
		}
		state := "-"
		switch row.Location {
		case "local":
			state = activityText(activity, running, row.Service, now)
		case "cloud":
			state = "ready"
		}
		fields := []string{row.Service, row.Location, state, target}
		runtime, directory, definition := "-", "-", "-"
		if a, ok := configured[row.Service]; row.Location == "local" && ok {
			runtime, directory = a.Runtime, homeRelative(a.Dir)
			if a.Definition != "" {
				definition = homeRelative(definitionCopy(dir, a.Name, a.Definition))
			}
		}
		if len(configured) > 0 {
			fields = append(fields, runtime, directory)
		}
		if withDefinition {
			fields = append(fields, definition)
		}
		fmt.Fprintln(tw, strings.Join(fields, "\t"))
	}
	return tw.Flush()
}

// activityTimeout bounds `agent list`'s question to the running node.
var activityTimeout = 2 * time.Second

// nodeActivity asks the node running from dataDir what its agents are
// doing. running is false when no node answers on the local agent socket;
// a node too old to say leaves activity nil.
func nodeActivity(dataDir string) (activity map[string]agentActivity, running bool) {
	ctx, cancel := context.WithTimeout(context.Background(), activityTimeout)
	defer cancel()
	client := socketClient(filepath.Join(dataDir, localAgentSocketName))
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agentiscaled/agents", nil)
	if err != nil {
		return nil, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, true
	}
	var answer localAgentsActivity
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer); err != nil {
		return nil, true
	}
	activity = map[string]agentActivity{}
	for _, a := range answer.Agents {
		activity[a.Name] = a
	}
	return activity, true
}

// activityText is what `agent list` shows in an agent's STATE column.
func activityText(activity map[string]agentActivity, running bool, name string, now time.Time) string {
	if !running {
		return "node not running"
	}
	if activity == nil {
		return "-"
	}
	a, ok := activity[name]
	if !ok {
		// Added since the node started; a restart serves it.
		return "not served yet"
	}
	var parts []string
	if a.State == activityBusy {
		parts = append(parts, "busy"+sinceText(a.Since, now))
	}
	switch {
	case a.Waiting == 1:
		parts = append(parts, "waiting for an answer"+sinceText(a.WaitingSince, now))
	case a.Waiting > 1:
		parts = append(parts, fmt.Sprintf("waiting for %d answers", a.Waiting)+sinceText(a.WaitingSince, now))
	}
	if len(parts) == 0 {
		return activityIdle
	}
	return strings.Join(parts, ", ")
}

// sinceText is how long ago t was, rounded for a glance: " (45s)",
// " (12m)", " (2h5m)".
func sinceText(t *time.Time, now time.Time) string {
	if t == nil {
		return ""
	}
	d := max(now.Sub(*t), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf(" (%ds)", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf(" (%dm)", int(d.Minutes()))
	}
	return fmt.Sprintf(" (%dh%dm)", int(d.Hours()), int(d.Minutes())%60)
}

// runtimeTitle is how a runtime's agent files are called.
func runtimeTitle(rt string) string {
	switch rt {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	}
	return rt
}
