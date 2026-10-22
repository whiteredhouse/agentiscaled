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

// agentOffer is an agent the owner chose to offer from this machine.
type agentOffer struct {
	runtime  string
	agentDir string
	dataDir  string
	policy   agentPolicy
}

// offerAgent asks whether to offer an agent from this machine and, on yes,
// which runtime and directory. The agent gets a node of its own, so the node
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
	var dir string
	for {
		answer, err := w.ask("  Directory the agent works in (for codex, the repository it reviews read-only)", "")
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(w.out, "  No directory given; no agent offered.")
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		if answer == "" {
			continue
		}
		dir = expandHome(answer)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			fmt.Fprintf(w.out, "  %s is not a directory.\n", dir)
			continue
		}
		if dir, err = filepath.Abs(dir); err != nil {
			return nil, err
		}
		break
	}
	offer := &agentOffer{runtime: rt, agentDir: dir, dataDir: agentDataDir(rt, dir)}
	// Its own guidance loads as in any run there, unless the owner says no,
	// such as for code from someone else.
	for _, q := range []struct {
		question string
		allow    *bool
	}{
		{"  Load its instruction files (AGENTS.md)?", &offer.policy.RepoInstructions},
		{"  Load its skills?", &offer.policy.RepoSkills},
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
// such as agentiscale-codex-my-repo.
func agentDataDir(rt, agentDir string) string {
	name := clientEntryName(filepath.Join("/", filepath.Base(agentDir)))
	name = "agentiscale-" + rt + "-" + strings.TrimPrefix(name, "agentiscale-")
	return filepath.Join(filepath.Dir(defaultDataDir()), name)
}

// setUpAgent signs the agent in, if its runtime needs it and it has not,
// and sets up its node with `up` for that data directory.
func (w *wizard) setUpAgent(ctx context.Context, exe string, o *agentOffer) error {
	spec := runtimes[o.runtime]
	if spec.login != nil && !spec.signedIn(o.dataDir) {
		cmd, shown := spec.login(ctx, spec.defaultCommand, o.dataDir)
		fmt.Fprintf(w.out, "\nThe agent signs in to %s on its own, apart from your own sign-in:\n  %s\n", o.runtime, shown)
		if err := runAttached(ctx, cmd); err != nil {
			return fmt.Errorf("the agent's sign-in: %w", err)
		}
	}
	fmt.Fprintf(w.out, "\nSetting up the agent's own node in %s:\n", o.dataDir)
	args := []string{"up", "-y", "-data-dir", o.dataDir, "-agent", o.runtime, "-agent-dir", o.agentDir}
	if !o.policy.RepoInstructions {
		args = append(args, "-agent-repo-instructions=false")
	}
	if !o.policy.RepoSkills {
		args = append(args, "-agent-repo-skills=false")
	}
	return runAttached(ctx, exec.CommandContext(ctx, exe, args...))
}

// runAttached runs a command on this terminal, for steps that talk to the
// owner themselves, such as a sign-in.
var runAttached = func(ctx context.Context, cmd *exec.Cmd) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
