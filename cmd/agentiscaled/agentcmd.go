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
	"strings"
	"text/tabwriter"
)

const agentUsage = `usage: agentiscaled agent add|remove|list

  add [RUNTIME] [-dir DIR] [-name NAME] ...  offer an agent from this machine to your network
                                              (-definition FILE gives it a subagent's role)
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

func runAgentAdd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent add", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node that offers the agent")
	dir := fs.String("dir", "", "Directory the agent works in (default: a new workspace of its own)")
	name := fs.String("name", "", "Name of the agent in your network (default: the runtime's)")
	description := fs.String("description", "", "What the agent does, as other agents see it on its card (default: the runtime's)")
	command := fs.String("command", "", "Command of the agent CLI (default: the runtime's, e.g. codex)")
	definition := fs.String("definition", "", "Subagent definition to give the agent its role: a Claude Code agent file (.claude/agents/NAME.md) or a Codex agent file (.codex/agents/NAME.toml). Its instructions join the system prompt of each task; it also sets the default runtime, name, description and directory (the project it is in)")
	var policy agentPolicy
	fs.BoolVar(&policy.DirInstructions, "dir-instructions", true, "Let the agent load the instruction files (AGENTS.md, CLAUDE.md) of its directory; =false ignores them")
	fs.BoolVar(&policy.DirSkills, "dir-skills", true, "Let the agent load the skills of its directory; =false turns them off")
	fs.BoolVar(&policy.OwnerConfig, "owner-config", true, "Run the agent with your own configuration of its runtime (sign-in, skills, plugins, MCP servers, settings and permissions); =false uses an isolated read-only configuration in the node's data directory")
	assumeYes := fs.Bool("y", false, "Ask nothing; take every default")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentiscaled agent add [RUNTIME] [flags]\n\nRUNTIME is one of %s; without one, add takes the -definition file's, or asks in a terminal.\n\n", strings.Join(runtimeNames(), ", "))
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
	case len(rest) == 1 || def != nil:
		rt := ""
		if len(rest) == 1 {
			rt = rest[0]
		} else {
			rt = def.Runtime
		}
		spec, ok := runtimes[rt]
		if !ok {
			return fmt.Errorf("unknown runtime %q (have %s)", rt, strings.Join(runtimeNames(), ", "))
		}
		offer = &agentOffer{runtime: rt, name: *name, description: *description, agentDir: *dir, definition: defPath, policy: policy}
		if def != nil {
			if offer.name == "" {
				offer.name = def.Name
			}
			if offer.description == "" {
				offer.description = def.Description
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
	replaced := false
	if s, err := loadSettings(offer.dataDir); err == nil {
		replaced = slices.ContainsFunc(s.Agents, func(a agentConfig) bool { return a.Name == offer.name })
	}
	// The node signs in and joins first if it has not; a node that runs
	// restarts to serve the agent next to the ones it offers already.
	add := offer.config()
	if err := bringUp(ctx, []string{"-data-dir", offer.dataDir, "-y"}, out, upOptions{addAgent: &add}); err != nil {
		return err
	}
	verb := "has"
	if replaced {
		verb = "has the changed"
	}
	fmt.Fprintf(out, "\nYour network %s agent %q from this machine. 'agentiscaled agent list' shows this machine's agents; 'agentiscaled agent remove %s' stops offering it.\n", verb, offer.name, offer.name)
	return nil
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
	tasks := "running"
	if _, err := os.Stat(stopFilePath(*dataDir)); err == nil {
		tasks = "stopped"
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tRUNTIME\tDIRECTORY\tTASKS")
	for _, a := range s.Agents {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Name, a.Runtime, homeRelative(a.Dir), tasks)
	}
	return tw.Flush()
}
