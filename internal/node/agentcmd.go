package node

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
	case len(rest) == 1 && !slices.Contains(names, rest[0]):
		return fmt.Errorf("this machine offers no agent named %q; see 'agentiscaled agent list'", rest[0])
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
	// An agent offered again starts out taking tasks.
	if _, err := removeDrain(dir, name); err != nil {
		fmt.Fprintf(out, "Could not lift its drain: %v\n", err)
	}
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
		state := activityText(activity, running, a.Name, now, loadDrain(drainPath(*dataDir, a.Name)))
		if !defined {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Name, state, a.Runtime, homeRelative(a.Dir))
			continue
		}
		def := "-"
		if a.Definition != "" {
			def = homeRelative(definitionCopy(*dataDir, a.Name, a.Definition))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Name, state, a.Runtime, homeRelative(a.Dir), def)
	}
	return tw.Flush()
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
		text := "drained"
		if busy {
			text = "draining"
		}
		if !drain.Since.IsZero() {
			text += sinceText(&drain.Since, now)
		}
		if drain.Reason != "" {
			text += ": " + drain.Reason
		}
		return text
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
		parts = append(parts, "busy"+sinceText(a.Since, now))
	}
	switch {
	case a.Waiting == 1:
		parts = append(parts, "waiting for an answer"+sinceText(a.WaitingSince, now))
	case a.Waiting > 1:
		parts = append(parts, fmt.Sprintf("waiting for %d answers", a.Waiting)+sinceText(a.WaitingSince, now))
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
	var names []string
	for _, a := range s.Agents {
		names = append(names, a.Name)
	}
	switch {
	case len(names) == 0:
		return errors.New("this machine offers no agent")
	case len(rest) == 1 && !slices.Contains(names, rest[0]):
		return fmt.Errorf("this machine offers no agent named %q; see 'agentiscaled agent list'", rest[0])
	case len(rest) == 1:
		names = rest
	}
	if !drain {
		for _, name := range names {
			lifted, err := removeDrain(dir, name)
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
	for _, name := range names {
		mark := drainMark{Reason: *reason, Since: now, By: caller.DrainSetOnNode}
		// Draining again changes the reason; the drain still began when it
		// first did.
		if prior := loadDrain(drainPath(dir, name)); prior != nil && !prior.Since.IsZero() {
			mark.Since = prior.Since
		}
		if err := saveDrain(dir, name, mark); err != nil {
			return fmt.Errorf("drain %s: %w", name, err)
		}
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
		return errOldNode(dir, s)
	case nodeAnswered:
		if !nodeReadsDrains(activity, names) {
			return errOldNode(dir, s)
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
			return waitDrained(ctx, dir, s, names, *timeout, out, target(rest))
		}
		return errors.New("the drain is saved, but the node did not answer whether it holds; check with 'agentiscaled agent list'")
	}
	for _, name := range names {
		a, served := activity[name]
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
		return waitDrained(ctx, dir, s, names, *timeout, out, target(rest))
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
func errOldNode(dir string, st settings) error {
	return fmt.Errorf("the drain is saved, but the running node is older than this command and ignores it until it restarts; %s, which ends the tasks it runs", restartAdvice(dir, st))
}

// errDenied says that this user may not ask the node whether the drain
// holds.
func errDenied(dir string) error {
	return fmt.Errorf("the drain is saved, but this user may not use the node's socket in %s to see whether it holds; run the command as the user that runs the node", dir)
}

// restartAdvice says how to restart the node that runs from dir: its
// background service, or the agentiscaled run process in the foreground.
func restartAdvice(dir string, st settings) string {
	if svc := newService(dir); svc != nil && !st.Foreground && svc.installed() {
		return fmt.Sprintf("restart its background service (%s)", svc.name())
	}
	return fmt.Sprintf("restart the node: stop the 'agentiscaled run -data-dir %s' process (Ctrl-C, or stop its container) and start it again", shellQuote(dir))
}

// nodeReadsDrains reports whether the running node reads drain files: it
// then reports the drain of each agent it serves that was just drained.
func nodeReadsDrains(activity map[string]agentActivity, names []string) bool {
	for _, name := range names {
		if a, served := activity[name]; served && a.Drain == nil {
			return false
		}
	}
	return true
}

// drainWaitEvery is how often agent drain -wait asks the node, and
// drainWaitSilence how long it goes on without any answer from it.
var (
	drainWaitEvery   = time.Second
	drainWaitSilence = 30 * time.Second
)

// waitDrained waits until the node says that none of the named agents has
// a task running or waiting for an answer, and that their drain holds. Only
// that answer ends the wait with success: a node that stops answering,
// refuses this user, or stays silent for drainWaitSilence ends it with an
// error, since none of these proves that no task runs. ctx carries the
// -timeout, which timeout names for the error.
func waitDrained(ctx context.Context, dir string, st settings, names []string, timeout time.Duration, out io.Writer, target string) error {
	tick := time.NewTicker(drainWaitEvery)
	defer tick.Stop()
	var left []string // nil until the node answers
	heard := time.Now()
	for {
		activity, answer := askNodeActivity(ctx, dir)
		// An answer that came in after the deadline does not count.
		if ctx.Err() != nil {
			return waitEnded(ctx, left, timeout)
		}
		switch answer {
		case nodeAnswered:
			if !nodeReadsDrains(activity, names) {
				return errOldNode(dir, st)
			}
			heard = time.Now()
			left = []string{}
			for _, name := range names {
				if a, served := activity[name]; served && (a.State == activityBusy || a.Waiting > 0) {
					left = append(left, name)
				}
			}
			if len(left) == 0 {
				fmt.Fprintf(out, "%s: no task left.\n", strings.Join(names, ", "))
				fmt.Fprintf(out, "'agentiscaled agent undrain%s' lets it take tasks again.\n", target)
				return nil
			}
		case nodeNoActivity:
			return errOldNode(dir, st)
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
