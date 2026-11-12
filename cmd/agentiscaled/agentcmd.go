package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

const agentUsage = `usage: agentiscaled agent add|remove|list

  add [RUNTIME] [-dir DIR] [-name NAME] ...  offer an agent from this machine to your network
  remove [NAME]                               stop offering an agent
  list                                        show the agents this machine offers

Run 'agentiscaled agent add -h' for its flags.`

// runAgent is `agentiscaled agent`: it offers agents from this machine to
// the owner's network, apart from bringing the node up.
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
		return runAgentList(args[1:], out)
	}
	return fmt.Errorf("unknown agent command %q\n%s", args[0], agentUsage)
}

// agentNode is a node on this machine and its saved settings.
type agentNode struct {
	dir string
	s   settings
}

// agentName is the name of the agent a node offers in the network.
func (n agentNode) agentName() string {
	if n.s.AgentName != "" {
		return n.s.AgentName
	}
	return runtimes[n.s.Agent].defaultName
}

// extraNodeDir is the data directory of the node that `agent add` sets up
// for an agent when the default node already offers another one: one node
// offers one agent.
func extraNodeDir(name string) string {
	return filepath.Join(filepath.Dir(defaultDataDir()), "agentiscale-"+name)
}

// localNodes are the nodes set up on this machine where `agent` looks for
// them: the default node, the nodes `agent add` set up next to it, and the
// one in dir, if given.
func localNodes(dir string) []agentNode {
	dirs := []string{defaultDataDir()}
	extra, _ := filepath.Glob(filepath.Join(filepath.Dir(defaultDataDir()), "agentiscale-*"))
	slices.Sort(extra)
	dirs = append(dirs, extra...)
	if dir != "" {
		dirs = append(dirs, dir)
	}
	var nodes []agentNode
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[realAbs(d)] {
			continue
		}
		seen[realAbs(d)] = true
		if s, err := loadSettings(d); err == nil {
			nodes = append(nodes, agentNode{dir: d, s: s})
		}
	}
	return nodes
}

func runAgentAdd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent add", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "Data directory of the node that offers the agent (default: this machine's node, or a new node of the agent's own when that one offers another agent)")
	dir := fs.String("dir", "", "Directory the agent works in (default: a new workspace of its own)")
	name := fs.String("name", "", "Name of the agent in your network (default: the runtime's)")
	description := fs.String("description", "", "What the agent does, as other agents see it on its card (default: the runtime's)")
	command := fs.String("command", "", "Command of the agent CLI (default: the runtime's, e.g. codex)")
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

	var ask *wizard
	if !*assumeYes && interactive() {
		ask = &wizard{in: bufio.NewReader(os.Stdin), out: out}
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
		for _, f := range []string{"dir", "name", "description", "command"} {
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

	if fi, err := os.Stat(offer.agentDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("-dir %s is not a directory", offer.agentDir)
	}
	if offer.agentDir, err = filepath.Abs(offer.agentDir); err != nil {
		return err
	}

	// Pick the node: one node offers one agent.
	def := defaultDataDir()
	defSettings, defErr := loadSettings(def)
	target, extra := *dataDir, false
	if target == "" {
		target = def
		if defErr == nil && defSettings.Agent != "" && (agentNode{dir: def, s: defSettings}).agentName() != offer.name {
			target, extra = extraNodeDir(offer.name), true
		}
	}
	if target, err = filepath.Abs(target); err != nil {
		return err
	}
	if s, err := loadSettings(target); err == nil {
		if n := (agentNode{dir: target, s: s}); s.Agent != "" && n.agentName() != offer.name {
			return fmt.Errorf("the node in %s already offers %q; one node offers one agent. Remove it first with 'agentiscaled agent remove %s', or give another -data-dir", target, n.agentName(), n.agentName())
		}
		extra = false
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load settings: %w", err)
	}
	if a, d := realAbs(offer.agentDir), realAbs(target); within(d, a) || within(a, d) {
		return fmt.Errorf("the agent's directory %s and the node's data directory %s overlap: the agent could read the node's key and its own sign-in. Pick a -dir outside the data directory", offer.agentDir, target)
	}
	for _, n := range localNodes(target) {
		if realAbs(n.dir) != realAbs(target) && n.s.Agent != "" && n.agentName() == offer.name {
			return fmt.Errorf("the node in %s already offers an agent named %q; remove it first ('agentiscaled agent remove %s') or pick another -name", n.dir, offer.name, offer.name)
		}
	}
	if extra {
		fmt.Fprintf(out, "This machine's node offers %q already, and one node offers one agent, so %q gets a node of its own in %s, which joins your network.\n", (agentNode{dir: def, s: defSettings}).agentName(), offer.name, homeRelative(target))
	}
	offer.dataDir = target
	if ask != nil {
		if err := ask.prepareAgent(ctx, offer); err != nil {
			return err
		}
	}

	up := []string{
		"-data-dir", target, "-y",
		"-agent", offer.runtime, "-agent-dir", offer.agentDir, "-agent-name", offer.name,
		"-agent-description", offer.description,
		"-agent-dir-instructions=" + strconv.FormatBool(offer.policy.DirInstructions),
		"-agent-dir-skills=" + strconv.FormatBool(offer.policy.DirSkills),
		"-agent-owner-config=" + strconv.FormatBool(offer.policy.OwnerConfig),
	}
	if *command != "" {
		up = append(up, "-agent-command", *command)
	}
	if extra {
		// The clients reach this agent through the node they use already.
		up = append(up, "-clients", "none", "-hostname", defaultMachineName()+"-"+offer.name)
		if defErr == nil && defSettings.Foreground {
			up = append(up, "-foreground")
		}
	}
	if err := bringUp(ctx, up, out, upOptions{agentFlags: true}); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nYour network has the agent %q from this machine. 'agentiscaled agent list' shows this machine's agents; 'agentiscaled agent remove %s' stops offering it.\n", offer.name, offer.name)
	return nil
}

func runAgentRemove(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent remove", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "Data directory of the node that offers the agent")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(rest[1:], " "))
	}
	if *dataDir != "" {
		if *dataDir, err = filepath.Abs(*dataDir); err != nil {
			return err
		}
	}
	var offering []agentNode
	for _, n := range localNodes(*dataDir) {
		if n.s.Agent == "" || *dataDir != "" && realAbs(n.dir) != realAbs(*dataDir) || len(rest) == 1 && n.agentName() != rest[0] {
			continue
		}
		offering = append(offering, n)
	}
	var names []string
	for _, n := range offering {
		names = append(names, n.agentName())
	}
	switch {
	case len(offering) == 0 && len(rest) == 1:
		return fmt.Errorf("this machine offers no agent named %q; see 'agentiscaled agent list'", rest[0])
	case len(offering) == 0:
		return errors.New("this machine offers no agent")
	case len(offering) > 1:
		return fmt.Errorf("name the agent to remove: %s", strings.Join(names, ", "))
	}
	n := offering[0]
	name := n.agentName()
	s := n.s
	s.Agent, s.AgentDir, s.AgentCommand, s.AgentName, s.AgentDescription, s.AgentEnv = "", "", "", "", "", nil
	s.AgentIgnoreDirInstructions, s.AgentIgnoreDirSkills, s.AgentOwnConfig = false, false, false
	if err := saveSettings(n.dir, s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	fmt.Fprintf(out, "This machine no longer offers %q; its directory %s stays as it is.\n", name, homeRelative(n.dir))
	svc := newService(n.dir)
	// A node `agent add` set up for this agent alone has nothing left to do.
	if realAbs(n.dir) == realAbs(extraNodeDir(name)) && s.Clients == "none" {
		if svc != nil && svc.installed() {
			if err := svc.remove(); err != nil {
				return err
			}
		}
		fmt.Fprintf(out, "Stopped its node in %s. The node keeps its place in your network until you remove it in the console (%s).\n", homeRelative(n.dir), consoleURL)
		return nil
	}
	return restartNode(n.dir, s, svc, out)
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
	dataDir := fs.String("data-dir", "", "Also look at the node in this data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *dataDir != "" {
		dir, err := filepath.Abs(*dataDir)
		if err != nil {
			return err
		}
		*dataDir = dir
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	any := false
	for _, n := range localNodes(*dataDir) {
		if n.s.Agent == "" {
			continue
		}
		if !any {
			fmt.Fprintln(tw, "NAME\tRUNTIME\tDIRECTORY\tNODE\tTASKS")
			any = true
		}
		tasks := "running"
		if _, err := os.Stat(stopFilePath(n.dir)); err == nil {
			tasks = "stopped"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n.agentName(), n.s.Agent, homeRelative(n.s.AgentDir), homeRelative(n.dir), tasks)
	}
	if !any {
		fmt.Fprintln(out, "This machine offers no agent. Offer one with 'agentiscaled agent add'.")
		return nil
	}
	return tw.Flush()
}
