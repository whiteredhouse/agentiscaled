package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// wizard asks `up`'s questions in a terminal. Every question has a default,
// and a run that takes every default ends as `up -y` does.
type wizard struct {
	in  *bufio.Reader
	out io.Writer
}

// interactive tells whether `up` may ask: only with a person at a terminal
// on both ends, so that scripts, services and SSH without a TTY never wait
// for an answer.
var interactive = func() bool {
	return isTerminal(os.Stdin) && isTerminal(os.Stdout)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// ask prints a question and reads one line. At the end of the input it
// answers the default, so a closed terminal never stops `up`.
func (w *wizard) ask(question, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(w.out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(w.out, "%s: ", question)
	}
	line, err := w.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if errors.Is(err, io.EOF) && line == "" {
		fmt.Fprintln(w.out)
		return def, io.EOF
	}
	if line = strings.TrimSpace(line); line == "" {
		return def, nil
	}
	return line, nil
}

func (w *wizard) yesNo(question string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		fmt.Fprintf(w.out, "%s [%s]: ", question, hint)
		line, err := w.in.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return def, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			if errors.Is(err, io.EOF) {
				fmt.Fprintln(w.out)
				return def, io.EOF
			}
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		if errors.Is(err, io.EOF) {
			return def, io.EOF
		}
		fmt.Fprintln(w.out, "  Answer y or n.")
	}
}

// installedClients are the clients this system can have that are installed,
// by their -clients names.
func installedClients() []string {
	var names []string
	for _, n := range clientNames {
		if n == "claude-desktop" && goruntime.GOOS != "darwin" {
			continue
		}
		if c := selectClients(n, false); len(c) == 1 && c[0].installed() {
			names = append(names, n)
		}
	}
	return names
}

// chooseClients asks which installed clients to connect, each one chosen by
// default unless the node serves an agent, and answers a -clients value.
func (w *wizard) chooseClients(installed []string, servesAgent bool) (string, error) {
	if len(installed) == 0 {
		return clientsAuto, nil
	}
	fmt.Fprintln(w.out, "\nAgent clients on this machine can use this node.")
	var chosen []string
	for _, n := range installed {
		// At the end of the input each answer is its default.
		yes, err := w.yesNo("  Connect "+selectClients(n, false)[0].name()+"?", !servesAgent)
		if err := ignoreEOF(err); err != nil {
			return "", err
		}
		if yes {
			chosen = append(chosen, n)
		}
	}
	if len(chosen) == 0 {
		return "none", nil
	}
	return strings.Join(chosen, ","), nil
}

// defaultAgentDir is the directory an offered agent works in when the owner
// names none: a workspace of its own, named after the agent, that
// agentiscaled makes for it in the user's application data (such as
// ~/Library/Application Support/agentiscaled/workspaces/codex), apart from
// the owner's files and from every node's data directory.
func defaultAgentDir(name string) string {
	return filepath.Join(userDataDir(), "agentiscaled", "workspaces", name)
}

// userDataDir is where applications keep the user's data: Application
// Support on macOS, $XDG_DATA_HOME (default ~/.local/share) on other Unix
// systems. Go's os package has no such function.
func userDataDir() string {
	if goruntime.GOOS == "darwin" || goruntime.GOOS == "windows" {
		return filepath.Dir(defaultDataDir())
	}
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share")
	}
	return filepath.Dir(defaultDataDir())
}

// validAgentName tells whether a name can be an agent's name in the network
// (agentNameError) and the name of its workspace directory.
func validAgentName(name string) bool {
	for i, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0) {
			return false
		}
	}
	return name != "" && agentNameError(name) == nil
}

// agentOffer is an agent the owner chose to offer from this machine.
type agentOffer struct {
	runtime     string
	name        string
	description string // empty for the runtime's
	agentDir    string
	dataDir     string
	policy      agentPolicy
}

// offerAgent asks whether to offer an agent from this machine and, on yes,
// which runtime, name and directory. The agent gets a node of its own, so the node
// the clients use stays one that only sends.
func (w *wizard) offerAgent() (*agentOffer, error) {
	yes, err := w.yesNo("\nOffer an agent from this machine to your network?", false)
	if err != nil || !yes {
		return nil, ignoreEOF(err)
	}
	names := runtimeNames()
	rt := names[0]
	if len(names) > 1 {
		for {
			answer, err := w.ask("  Runtime ("+strings.Join(names, ", ")+")", rt)
			if err != nil {
				return nil, ignoreEOF(err)
			}
			if _, ok := runtimes[answer]; ok {
				rt = answer
				break
			}
			fmt.Fprintf(w.out, "  Unknown runtime %q.\n", answer)
		}
	} else {
		fmt.Fprintf(w.out, "  Runtime: %s (the only one today)\n", rt)
	}
	name := runtimes[rt].defaultName
	for {
		answer, err := w.ask("  Name of the agent in your network", name)
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(w.out, "  No name given; no agent offered.")
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		if validAgentName(answer) {
			name = answer
			break
		}
		fmt.Fprintln(w.out, "  Use up to 63 lowercase letters, digits and dashes.")
	}
	description, err := w.ask("  What it does, as other agents see it (Enter for the runtime's)", "")
	if errors.Is(err, io.EOF) {
		fmt.Fprintln(w.out, "  No description given; no agent offered.")
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var dir string
	for {
		answer, err := w.ask("  Directory the agent works in", homeRelative(defaultAgentDir(name)))
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(w.out, "  No directory given; no agent offered.")
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		dir = expandHome(answer)
		// The default is a directory of the agent's own, made on first use.
		if dir == defaultAgentDir(name) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			fmt.Fprintf(w.out, "  %s is not a directory.\n", dir)
			continue
		}
		if dir, err = filepath.Abs(dir); err != nil {
			return nil, err
		}
		break
	}
	offer := &agentOffer{runtime: rt, name: name, description: description, agentDir: dir, dataDir: agentDataDir(rt, dir)}
	// The agent runs as the owner's own runtime would, with their
	// configuration and the directory's own guidance, unless the owner says
	// no, such as for code from someone else.
	for _, q := range []struct {
		question string
		allow    *bool
	}{
		{"  Use your own " + rt + " configuration (sign-in, skills, plugins, MCP servers)?", &offer.policy.OwnerConfig},
		{"  Load the directory's instruction files (AGENTS.md)?", &offer.policy.DirInstructions},
		{"  Load the directory's skills?", &offer.policy.DirSkills},
	} {
		yes, err := w.yesNo(q.question, true)
		if err := ignoreEOF(err); err != nil {
			return nil, err
		}
		*q.allow = yes
	}
	return offer, nil
}

func ignoreEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// homeRelative shows a path in the home directory from ~, as expandHome
// reads it back.
func homeRelative(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return filepath.Join("~", rel)
		}
	}
	return path
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// agentDataDir is the data directory of the node that serves an agent,
// next to the default one and named after the runtime and its directory,
// such as agentiscale-codex-my-project, or agentiscale-codex for the
// workspace of the agent codex (defaultAgentDir). Only a directory that
// holds no node yet (nothing, or only the agent's own sign-in from a setup
// that stopped before `up`), or whose node serves this same agent
// directory, is taken: any other, such as the node of /work/a/service when
// this is /work/b/service, a node that serves no agent, or one whose
// settings cannot be read, keeps its directory, and the new one gets -2, -3
// and so on.
func agentDataDir(rt, agentDir string) string {
	name := clientEntryName(filepath.Join("/", filepath.Base(agentDir)))
	base := filepath.Join(filepath.Dir(defaultDataDir()), "agentiscale-"+rt)
	name = strings.TrimPrefix(strings.TrimPrefix(name, "agentiscale-"), rt+"-")
	if name != rt {
		base += "-" + name
	}
	for n := 1; ; n++ {
		dir := base
		if n > 1 {
			dir = fmt.Sprintf("%s-%d", base, n)
		}
		if entries, err := os.ReadDir(dir); errors.Is(err, os.ErrNotExist) ||
			err == nil && (len(entries) == 0 || len(entries) == 1 && entries[0].Name() == "codex") {
			return dir
		}
		if s, err := loadSettings(dir); err == nil && s.AgentDir == agentDir {
			return dir
		}
	}
}

// setUpAgent signs the agent in, if its runtime needs it and it has not,
// and sets up its node with `up` for that data directory.
func (w *wizard) setUpAgent(ctx context.Context, exe string, o *agentOffer) error {
	spec := runtimes[o.runtime]
	if spec.login != nil && !spec.signedIn(o.dataDir, o.policy) {
		cmd, shown := spec.login(ctx, spec.defaultCommand, o.dataDir, o.policy)
		if spec.envSignedIn != nil && spec.envSignedIn(o.policy) {
			fmt.Fprintf(w.out, "\nYour %s credential is only in this shell, which the agent's background service does not get; storing it:\n  %s\n", o.runtime, shown)
		} else if o.policy.OwnerConfig {
			fmt.Fprintf(w.out, "\nYou are not signed in to %s yet:\n  %s\n", o.runtime, shown)
		} else {
			fmt.Fprintf(w.out, "\nThe agent signs in to %s on its own, apart from your own sign-in:\n  %s\n", o.runtime, shown)
		}
		if err := runAttached(ctx, cmd); err != nil {
			return fmt.Errorf("the agent's sign-in: %w", err)
		}
	}
	fmt.Fprintf(w.out, "\nSetting up the agent's own node in %s:\n", o.dataDir)
	args := []string{"up", "-y", "-data-dir", o.dataDir, "-agent", o.runtime, "-agent-dir", o.agentDir}
	if o.name != "" && o.name != spec.defaultName {
		args = append(args, "-agent-name", o.name)
	}
	if o.description != "" {
		args = append(args, "-agent-description", o.description)
	}
	if !o.policy.DirInstructions {
		args = append(args, "-agent-dir-instructions=false")
	}
	if !o.policy.DirSkills {
		args = append(args, "-agent-dir-skills=false")
	}
	if !o.policy.OwnerConfig {
		args = append(args, "-agent-owner-config=false")
	}
	return runAttached(ctx, exec.CommandContext(ctx, exe, args...))
}

// runAttached runs a command on this terminal, for steps that talk to the
// owner themselves, such as a sign-in.
var runAttached = func(ctx context.Context, cmd *exec.Cmd) error {
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
