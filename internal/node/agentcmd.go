package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/whiteredhouse/agentiscaled/internal/caller"
	"github.com/whiteredhouse/agentiscaled/internal/nodeclient"
)

const agentUsage = `usage: agentiscaled agent add|remove|list|drain|undrain

  add [RUNTIME] [-dir DIR] [-name NAME] ...  offer an agent from this machine to your network
                                              (-definition FILE gives it a subagent's role)
  remove [NAME]                               stop offering an agent
  list                                        show the agents this machine offers
  drain [NAME] [-reason TEXT]                 let an agent finish its tasks but take no new ones
                                              (without NAME, every agent this machine offers)
  undrain [NAME]                              let a drained agent take new tasks again

Run 'agentiscaled agent add -h' for its flags.`

// RunAgent is `agentiscaled agent`: it offers agents from this machine to
// the owner's network, apart from bringing the node up.
func RunAgent(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "help" {
		return errors.New(agentUsage)
	}
	switch args[0] {
	case "add":
		return runAgentAdd(ctx, args[1:], out)
	case "remove":
		return runAgentRemove(args[1:], out)
	case "list":
		return runAgentList(args[1:], out)
	case "drain":
		return runAgentDrain(ctx, args[1:], out, true)
	case "undrain":
		return runAgentDrain(ctx, args[1:], out, false)
	}
	return fmt.Errorf("unknown agent command %q\n%s", args[0], agentUsage)
}

func runAgentAdd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent add", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node that offers the agent")
	dir := fs.String("dir", "", "Directory the agent works in (default: a new workspace of its own)")
	name := fs.String("name", "", "Name of the agent in your network (default: the runtime's)")
	description := fs.String("description", "", "What the agent does, as other agents see it on its card (default: the runtime's)")
	command := fs.String("command", "", "Command of the agent CLI (default: the runtime's, e.g. codex); for acp, the command line that starts the agent on stdio, split at spaces (required)")
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
		if _, ok := runtimes[rt]; !ok {
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
		if rt == "acp" && strings.TrimSpace(*command) == "" {
			return errors.New("the acp runtime needs the agent's command: agentiscaled agent add acp -command \"my-agent acp\"")
		}
		if offer.name == "" {
			program, _, _ := splitACPCommand(*command)
			if rt != "acp" {
				program = *command
			}
			offer.name = runtimeDefaultName(rt, program)
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
		if offer, err = ask.chooseAgent(false, policy, set); err != nil {
			return err
		} else if offer == nil {
			return errors.New("no agent offered")
		}
	default:
		return fmt.Errorf("name the agent's runtime: agentiscaled agent add %s [-dir DIR]", strings.Join(runtimeNames(), "|"))
	}

	offer.command = *command
	if offer.runtime == "acp" {
		if offer.command, offer.args, err = splitACPCommand(*command); err != nil {
			return err
		}
	}
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
	fmt.Fprintf(out, "\nYour network %s agent %q from this machine. 'agentiscaled agent list' shows this machine's agents; 'agentiscaled agent remove %s' stops offering it.\n", verb, offer.name, offer.name)
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
	case len(rest) == 1:
		// The exact saved name picks the agent; the check below then says
		// where a refused one is fixed.
		i := slices.Index(names, rest[0])
		if i < 0 {
			return fmt.Errorf("this machine offers no agent named %s; see 'agentiscaled agent list'", caller.QuotedAgentName(rest[0]))
		}
		name = names[i]
	case len(names) == 0:
		return errors.New("this machine offers no agent")
	case len(names) > 1:
		shown := make([]string, len(names))
		for i, n := range names {
			shown[i] = caller.ShownAgentName(n)
		}
		return fmt.Errorf("name the agent to remove: %s", strings.Join(shown, ", "))
	default:
		name = names[0]
	}
	// However the agent was picked, a refused name is fixed or deleted in
	// the settings file, as agent list says.
	if !caller.ValidAgentName(name) {
		return fmt.Errorf("agent %s has an invalid name; %s", caller.ShownAgentName(name), fixInSettings(dir))
	}
	s.Agents = slices.DeleteFunc(s.Agents, func(a agentConfig) bool { return a.Name == name })
	if err := saveSettings(dir, s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	removeDefinitionCopies(dir, name)
	// An agent offered again starts out taking tasks.
	if _, err := removeDrain(dir, name); err != nil {
		fmt.Fprintf(out, "Could not lift its drain: %v\n", err)
	}
	fmt.Fprintf(out, "This machine no longer offers %s; its directory stays as it is.\n", caller.ShownAgentName(name))
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

func runAgentList(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent list", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	s, err := loadSettings(*dataDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load settings: %w", err)
	}
	if len(s.Agents) == 0 {
		fmt.Fprintln(out, "This machine offers no agent. Offer one with 'agentiscaled agent add'.")
		return nil
	}
	activity, running := nodeActivity(*dataDir)
	defined := slices.ContainsFunc(s.Agents, func(a agentConfig) bool { return a.Definition != "" })
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if defined {
		fmt.Fprintln(tw, "NAME\tSTATE\tRUNTIME\tDIRECTORY\tDEFINITION")
	} else {
		fmt.Fprintln(tw, "NAME\tSTATE\tRUNTIME\tDIRECTORY")
	}
	now := time.Now()
	for _, a := range s.Agents {
		// The name the node gives it; one it refuses is shown escaped, so
		// that it cannot be copied into a command as it is.
		name := a.effectiveName()
		state := activityText(activity, running, name, now, loadDrain(drainPath(*dataDir, name)))
		if agentNameError(name) != nil {
			name = caller.ShownAgentName(name)
			state = invalidNameState(*dataDir)
		}
		if !defined {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, state, a.Runtime, homeRelative(a.Dir))
			continue
		}
		def := "-"
		if a.Definition != "" {
			def = homeRelative(definitionCopy(*dataDir, a.effectiveName(), a.Definition))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, state, a.Runtime, homeRelative(a.Dir), def)
	}
	return tw.Flush()
}

// invalidNameState is what agent list and status say of a saved agent whose
// name the node refuses.
func invalidNameState(dataDir string) string {
	return "invalid name; the node does not start: " + fixInSettings(dataDir)
}

// fixInSettings says where a saved agent with a refused name is fixed.
func fixInSettings(dataDir string) string {
	return "fix or delete it in " + filepath.Join(dataDir, settingsFile)
}

// activityTimeout bounds `agent list`'s question to the running node.
var activityTimeout = 2 * time.Second

// nodeActivity asks the node running from dataDir what its agents are
// doing. running is false when no node answers on the local agent socket;
// a node too old to say leaves activity nil.
func nodeActivity(dataDir string) (activity map[string]agentActivity, running bool) {
	activity, state := askNodeActivity(context.Background(), dataDir)
	return activity, state != nodeSilent && state != nodeUnreachable && state != nodeDenied
}

// What asking the node for its activity found out.
type nodeAnswer int

const (
	nodeAnswered    nodeAnswer = iota // a valid answer: activity holds it
	nodeSilent                        // nothing listens on the socket: stopped, or not started yet
	nodeDenied                        // this user may not use the node's socket
	nodeNoActivity                    // a node from before the activity API
	nodeUnreachable                   // no answer for another reason, such as a timeout
	nodeBadAnswer                     // an answer that cannot be read
)

// askNodeActivity asks the node running from dataDir what its agents are
// doing, within ctx and activityTimeout. Only nodeAnswered says what the
// agents do. No other outcome proves anything about the node: a node
// that starts serves its agents before it opens the socket, so silence is
// not proof that it stopped.
func askNodeActivity(ctx context.Context, dataDir string) (map[string]agentActivity, nodeAnswer) {
	ctx, cancel := context.WithTimeout(ctx, activityTimeout)
	defer cancel()
	client := nodeclient.SocketClient(filepath.Join(dataDir, localAgentSocketName))
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agentiscaled/agents", nil)
	if err != nil {
		return nil, nodeUnreachable
	}
	resp, err := client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrPermission):
			return nil, nodeDenied
		case errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED):
			return nil, nodeSilent
		}
		return nil, nodeUnreachable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nodeNoActivity
	case resp.StatusCode != http.StatusOK:
		return nil, nodeBadAnswer
	}
	var answer localAgentsActivity
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer); err != nil || answer.Agents == nil {
		return nil, nodeBadAnswer
	}
	activity := map[string]agentActivity{}
	for _, a := range answer.Agents {
		activity[a.Name] = a
	}
	return activity, nodeAnswered
}

// activityText is what `agent list` shows in an agent's STATE column:
// what it is doing, and last its drain, which drain is when the node does
// not say.
func activityText(activity map[string]agentActivity, running bool, name string, now time.Time, drain *drainMark) string {
	drainText := func(busy bool) string {
		if drain == nil {
			return ""
		}
		d := *drain
		d.Drained = !busy
		return d.State(now)
	}
	if !running {
		if drain != nil {
			return "node not running, " + drainText(false)
		}
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
	if a.Drain != nil {
		drain = a.Drain
	}
	var parts []string
	if a.State == activityBusy {
		parts = append(parts, "busy"+caller.SinceText(a.Since, now))
	}
	switch {
	case a.Waiting == 1:
		parts = append(parts, "waiting for an answer"+caller.SinceText(a.WaitingSince, now))
	case a.Waiting > 1:
		parts = append(parts, fmt.Sprintf("waiting for %d answers", a.Waiting)+caller.SinceText(a.WaitingSince, now))
	}
	if drain != nil {
		parts = append(parts, drainText(len(parts) > 0))
	}
	if len(parts) == 0 {
		return activityIdle
	}
	return strings.Join(parts, ", ")
}

// runAgentDrain is `agent drain` (drain) and `agent undrain`. A drained
// agent finishes the tasks it has, answers to its questions included, but
// refuses new ones with the reason. The node reads the drain for each new
// task, so it takes effect at once, without a restart, and outlasts one.
func runAgentDrain(ctx context.Context, args []string, out io.Writer, drain bool) error {
	verb := "undrain"
	if drain {
		verb = "drain"
	}
	fs := flag.NewFlagSet("agent "+verb, flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node that offers the agent")
	var reason *string
	var wait *bool
	var timeout *time.Duration
	if drain {
		reason = fs.String("reason", "", "Why the agent takes no new tasks, which callers see when it refuses one (one line, up to 200 bytes)")
		wait = fs.Bool("wait", false, "Wait until the agent has no task running or waiting for an answer, then exit")
		timeout = fs.Duration("timeout", 0, "With -wait, give up after this long, exiting with an error; the agent stays drained (default: no limit)")
	}
	fs.Usage = func() {
		if drain {
			fmt.Fprint(fs.Output(), "usage: agentiscaled agent drain [NAME] [-reason TEXT] [-wait [-timeout DURATION]]\n\nThe agent finishes the tasks it has, answers to its questions included, and refuses new ones.\nWithout NAME, every agent this machine offers drains; agents added later do not.\nWith -wait, the command returns once the agent has no task left, so a script can then restart it.\n\n")
		} else {
			fmt.Fprint(fs.Output(), "usage: agentiscaled agent undrain [NAME]\n\nThe agent takes new tasks again. Without NAME, every agent this machine offers does.\n\n")
		}
		fs.PrintDefaults()
	}
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(rest[1:], " "))
	}
	if drain && *timeout != 0 && !*wait {
		return errors.New("-timeout needs -wait")
	}
	if drain && *timeout < 0 {
		return errors.New("-timeout must not be negative")
	}
	if drain {
		*reason = strings.TrimSpace(*reason)
		if why := caller.DrainReasonError(*reason); why != "" {
			return fmt.Errorf("-reason %s", why)
		}
	}
	dir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	s, err := loadSettings(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load settings: %w", err)
	}
	// A saved name the node refuses serves nothing, so there is nothing to
	// drain; agent list shows it.
	var names []caller.AgentName
	for _, a := range s.Agents {
		if name, err := caller.ParseAgentName(a.Name); err == nil {
			names = append(names, name)
		}
	}
	switch {
	case len(names) == 0:
		return errors.New("this machine offers no agent")
	case len(rest) == 1:
		name, err := caller.ParseAgentName(rest[0])
		if err != nil || !slices.Contains(names, name) {
			return fmt.Errorf("this machine offers no agent named %s; see 'agentiscaled agent list'", caller.QuotedAgentName(rest[0]))
		}
		names = []caller.AgentName{name}
	}
	if !drain {
		for _, name := range names {
			lifted, err := removeDrain(dir, name.String())
			switch {
			case err != nil:
				return fmt.Errorf("undrain %s: %w", name, err)
			case lifted:
				fmt.Fprintf(out, "%s takes new tasks again.\n", name)
			default:
				fmt.Fprintf(out, "%s was not drained; it takes new tasks.\n", name)
			}
		}
		return nil
	}
	now := time.Now().UTC()
	// held is when each drain began, as saved: the drain this command
	// waits on, told apart from one lifted and set again.
	held := make(map[caller.AgentName]time.Time, len(names))
	for _, name := range names {
		mark := drainMark{Reason: *reason, Since: now, By: caller.DrainSetOnNode}
		// Draining again changes the reason; the drain still began when it
		// first did.
		if prior := loadDrain(drainPath(dir, name.String())); prior != nil && !prior.Since.IsZero() {
			mark.Since = prior.Since
		}
		if err := saveDrain(dir, name.String(), mark); err != nil {
			return fmt.Errorf("drain %s: %w", name, err)
		}
		held[name] = mark.Since
	}
	// -timeout bounds the whole wait, the first question to the node too.
	if *wait && *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	activity, answer := askNodeActivity(ctx, dir)
	if *wait && ctx.Err() != nil {
		return waitEnded(ctx, nil, *timeout)
	}
	switch answer {
	case nodeNoActivity:
		return errOldNode(ctx, dir, s)
	case nodeAnswered:
		if err := checkDrains(ctx, dir, s, activity, held); err != nil {
			return err
		}
	case nodeDenied:
		return errDenied(dir)
	case nodeSilent:
		for _, name := range names {
			fmt.Fprintf(out, "%s is drained: no node answers on this machine now, and whenever one runs the agent takes no new tasks.\n", name)
		}
		if *wait {
			// Silence is not proof that no work runs: a node that starts
			// serves its agents before it answers.
			return errors.New("-wait confirms that a running node finished the agent's tasks, and no node answers; the drain is saved")
		}
		fmt.Fprintf(out, "'agentiscaled agent undrain%s' lifts it.\n", target(rest))
		return nil
	default: // unreachable, or an answer that cannot be read
		if *wait {
			// The wait asks again.
			return waitDrained(ctx, dir, s, held, *timeout, out, target(rest))
		}
		return errors.New("the drain is saved, but the node did not answer whether it holds; check with 'agentiscaled agent list'")
	}
	for _, name := range names {
		a, served := activity[name.String()]
		switch {
		case !served:
			fmt.Fprintf(out, "%s is drained: it takes no new tasks.\n", name)
		case a.State == activityBusy || a.Waiting > 0:
			fmt.Fprintf(out, "%s is draining: it finishes %s and takes no new tasks.\n", name, unfinishedText(a))
		default:
			fmt.Fprintf(out, "%s is drained: it has no task and takes no new ones.\n", name)
		}
	}
	if *wait {
		return waitDrained(ctx, dir, s, held, *timeout, out, target(rest))
	}
	fmt.Fprintf(out, "'agentiscaled agent list' shows when it is drained; 'agentiscaled agent undrain%s' lets it take tasks again.\n", target(rest))
	return nil
}

// target is the agent name agent undrain takes, as a suffix of the command.
func target(rest []string) string {
	if len(rest) == 1 {
		return " " + rest[0]
	}
	return ""
}

// errOldNode says that the running node is from before drains: it ignores
// the drain file until it restarts, so the drain does not hold yet.
func errOldNode(ctx context.Context, dir string, st settings) error {
	return fmt.Errorf("the drain is saved, but the running node is older than this command and ignores it until it restarts; %s, which ends the tasks it runs", restartAdvice(ctx, dir, st, newService(dir)))
}

// errDenied says that this user may not ask the node whether the drain
// holds.
func errDenied(dir string) error {
	return fmt.Errorf("the drain is saved, but this user may not use the node's socket in %s to see whether it holds; run the command as the user that runs the node", dir)
}

// serviceStateWait bounds the question to launchd or systemd whether the
// node's background service runs.
var serviceStateWait = 2 * time.Second

// restartAdvice says how to restart the node that runs from dir: its
// background service when that is what runs now, else the agentiscaled run
// process in the foreground (also when an installed service is stopped and
// the node runs in the foreground instead). The question to the service
// manager ends with ctx, so it keeps within -timeout; without its answer
// the advice names both ways.
func restartAdvice(ctx context.Context, dir string, st settings, svc service) string {
	foreground := fmt.Sprintf("stop the 'agentiscaled run -data-dir %s' process (Ctrl-C, or stop its container) and start it again", shellQuote(dir))
	if svc == nil || st.Foreground || !svc.installed() {
		return "restart the node: " + foreground
	}
	// The service manager's process ends with ctx too.
	ctx, cancel := context.WithTimeout(ctx, serviceStateWait)
	defer cancel()
	_, running := svc.state(ctx)
	switch {
	case ctx.Err() != nil:
		return fmt.Sprintf("restart the node: its background service (%s) if that runs it, else %s", svc.name(), foreground)
	case running:
		return fmt.Sprintf("restart its background service (%s)", svc.name())
	}
	return "restart the node: " + foreground
}

// joinNames lists names for output.
func joinNames(names []caller.AgentName) string {
	shown := make([]string, len(names))
	for i, n := range names {
		shown[i] = n.String()
	}
	return strings.Join(shown, ", ")
}

// checkDrains checks that the node holds each drain this command saved
// (held), for the agents it serves. A drain the node does not report is
// told apart by its file: still saved, the node ignores drains (it is from
// before them); gone or begun anew, someone lifted it.
func checkDrains(ctx context.Context, dir string, st settings, activity map[string]agentActivity, held map[caller.AgentName]time.Time) error {
	for name, since := range held {
		a, served := activity[name.String()]
		switch {
		case !served:
			continue
		case a.Drain != nil && a.Drain.Since.Equal(since):
			continue
		case a.Drain != nil:
			return fmt.Errorf("%s was undrained and drained again while this command waited", name)
		}
		if saved := loadDrain(drainPath(dir, name.String())); saved == nil || !saved.Since.Equal(since) {
			return fmt.Errorf("%s was undrained while this command waited: it takes new tasks", name)
		}
		return errOldNode(ctx, dir, st)
	}
	return nil
}

// drainWaitEvery is how often agent drain -wait asks the node, and
// drainWaitSilence how long it goes on without any answer from it.
var (
	drainWaitEvery   = time.Second
	drainWaitSilence = 30 * time.Second
)

// waitDrained asks the node to answer once the drains this command saved
// (held) hold and the agents have no task left. The node judges that under
// its own locks and clock (POST /agents/drain/wait); the command only asks
// again while the node says work remains. Only the node's answer that the
// drains hold and no task is left ends the wait with success. ctx carries
// the -timeout, which timeout names for the error.
func waitDrained(ctx context.Context, dir string, st settings, held map[caller.AgentName]time.Time, timeout time.Duration, out io.Writer, target string) error {
	names := slices.Sorted(maps.Keys(held))
	var left []string // nil until the node answers
	heard := time.Now()
	tick := time.NewTicker(drainWaitEvery)
	defer tick.Stop()
	for {
		// Each question gets only the silence left, so that no answer
		// counts after drainWaitSilence without one.
		budget := drainWaitSilence - time.Since(heard)
		if budget <= 0 {
			return fmt.Errorf("the node has not answered for %s; the drain is saved", drainWaitSilence)
		}
		answer, kind := askDrainWait(ctx, dir, held, budget)
		// An answer that came in after the deadline does not count.
		if ctx.Err() != nil {
			return waitEnded(ctx, left, timeout)
		}
		switch kind {
		case nodeAnswered:
			heard = time.Now()
			switch answer.State {
			case drainHeld:
				fmt.Fprintf(out, "%s: no task left.\n", joinNames(names))
				fmt.Fprintf(out, "'agentiscaled agent undrain%s' lets it take tasks again.\n", target)
				return nil
			case drainLifted:
				return fmt.Errorf("%s was undrained while this command waited, so it may take new tasks", answer.Agent)
			case drainWorking:
				left = answer.Left
				continue // the node held the request while work remained
			}
			return fmt.Errorf("the node answered %q, which this command does not know", answer.State)
		case nodeNoActivity:
			return fmt.Errorf("the drain holds, but the running node is older than this command and cannot say when its tasks are done; %s to use -wait, or watch 'agentiscaled agent list'", restartAdvice(ctx, dir, st, newService(dir)))
		case nodeDenied:
			return errDenied(dir)
		case nodeSilent:
			return errors.New("the node stopped answering before it said the agent had no task left; the drain is saved")
		default:
			if time.Since(heard) >= drainWaitSilence {
				return fmt.Errorf("the node has not answered for %s; the drain is saved", drainWaitSilence)
			}
		}
		select {
		case <-ctx.Done():
			return waitEnded(ctx, left, timeout)
		case <-tick.C:
		}
	}
}

// askDrainWait asks the running node to answer once the drains in held
// hold and no task is left, or a drain is lifted, or it has waited long
// enough (POST /agents/drain/wait). The kinds are askNodeActivity's;
// nodeNoActivity is a node without this question.
func askDrainWait(ctx context.Context, dir string, held map[caller.AgentName]time.Time, budget time.Duration) (drainWaitAnswer, nodeAnswer) {
	// The node holds the request up to drainWaitLongest; a node that holds
	// it much longer is not answering. Within budget, the node is asked to
	// answer early enough for its answer to arrive.
	limit := min(drainWaitLongest+activityTimeout, budget)
	hold := max(min(drainWaitLongest, limit-activityTimeout), limit/2)
	body, err := json.Marshal(drainWaitRequest{Agents: held, WithinMS: max(hold.Milliseconds(), 1)})
	if err != nil {
		return drainWaitAnswer{}, nodeBadAnswer
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	client := nodeclient.SocketClient(filepath.Join(dir, localAgentSocketName))
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agentiscaled/agents/drain/wait", bytes.NewReader(body))
	if err != nil {
		return drainWaitAnswer{}, nodeUnreachable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrPermission):
			return drainWaitAnswer{}, nodeDenied
		case errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED):
			return drainWaitAnswer{}, nodeSilent
		}
		return drainWaitAnswer{}, nodeUnreachable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return drainWaitAnswer{}, nodeNoActivity
	case resp.StatusCode != http.StatusOK:
		return drainWaitAnswer{}, nodeBadAnswer
	}
	var answer drainWaitAnswer
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&answer); err != nil {
		return drainWaitAnswer{}, nodeBadAnswer
	}
	return answer, nodeAnswered
}

// waitEnded is agent drain -wait's error when its context ends first;
// left is nil when the node never answered.
func waitEnded(ctx context.Context, left []string, timeout time.Duration) error {
	switch {
	case !errors.Is(ctx.Err(), context.DeadlineExceeded):
		return ctx.Err()
	case left == nil:
		return fmt.Errorf("the node did not say within %s whether the agent still has work; it stays drained", timeout)
	}
	return fmt.Errorf("%s still has work after %s; it stays drained and finishes it", strings.Join(left, ", "), timeout)
}

// unfinishedText names the tasks a draining agent still finishes.
func unfinishedText(a agentActivity) string {
	var parts []string
	if a.State == activityBusy {
		parts = append(parts, "its running task")
	}
	switch {
	case a.Waiting == 1:
		parts = append(parts, "the task that waits for an answer")
	case a.Waiting > 1:
		parts = append(parts, fmt.Sprintf("%d tasks that wait for answers", a.Waiting))
	}
	return strings.Join(parts, " and ")
}

// runtimeTitle is how a runtime's agent files are called.
func runtimeTitle(rt string) string {
	switch rt {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "agy":
		return "Antigravity"
	}
	return rt
}
