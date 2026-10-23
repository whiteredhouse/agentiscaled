package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func testWizard(input string) (*wizard, *bytes.Buffer) {
	var out bytes.Buffer
	return &wizard{in: bufio.NewReader(strings.NewReader(input)), out: &out}, &out
}

func TestChooseClients(t *testing.T) {
	both := []string{"claude-desktop", "codex"}
	for _, tc := range []struct {
		input       string
		servesAgent bool
		want        string
	}{
		{"\n\n", false, "claude-desktop,codex"},
		{"", false, "claude-desktop,codex"}, // no answer is every default
		{"n\ny\n", false, "codex"},
		{"no\nNO\n", false, "none"},
		{"maybe\ny\n\n", false, "claude-desktop,codex"},
		{"\n\n", true, "none"},
		{"", true, "none"},
		{"y\n", true, "claude-desktop"},
	} {
		w, out := testWizard(tc.input)
		got, err := w.chooseClients(both, tc.servesAgent)
		if err != nil || got != tc.want {
			t.Errorf("input %q, serves agent %v: got %q, %v, want %q\n%s", tc.input, tc.servesAgent, got, err, tc.want, out)
		}
	}
	w, out := testWizard("")
	if got, _ := w.chooseClients(nil, false); got != clientsAuto || out.Len() != 0 {
		t.Errorf("no installed client: %q, asked %q", got, out)
	}
}

func TestOfferAgent(t *testing.T) {
	withDefaultDataDir(t)
	t.Setenv("XDG_DATA_HOME", "")
	dir := filepath.Join(t.TempDir(), "My Project")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"", "\n", "n\n"} {
		w, _ := testWizard(input)
		if o, err := w.offerAgent(); o != nil || err != nil {
			t.Errorf("input %q: offered %+v, %v", input, o, err)
		}
	}
	w, out := testWizard("y\n\n\n/does/not/exist\n" + dir + "\n")
	o, err := w.offerAgent()
	if err != nil || o == nil {
		t.Fatalf("%+v, %v\n%s", o, err, out)
	}
	wantDir := filepath.Join(filepath.Dir(defaultDataDir()), "agentiscale-codex-my-project")
	if o.runtime != "codex" || o.agentDir != dir || o.dataDir != wantDir || o.policy != (agentPolicy{DirInstructions: true, DirSkills: true, OwnerConfig: true}) {
		t.Errorf("offer %+v, want data dir %s and the policy on", o, wantDir)
	}
	if !strings.Contains(out.String(), "/does/not/exist is not a directory") {
		t.Errorf("output:\n%s", out)
	}
	w, _ = testWizard("y\n\nAnswers questions about the code\n" + dir + "\nn\ny\nn\n")
	if o, err := w.offerAgent(); err != nil || o == nil || o.policy != (agentPolicy{DirInstructions: true}) || o.description != "Answers questions about the code" {
		t.Errorf("own config and skills turned off: offered %+v, %v", o, err)
	}
	w, _ = testWizard("y\n")
	if o, err := w.offerAgent(); o != nil || err != nil {
		t.Errorf("no directory: offered %+v, %v", o, err)
	}
	for _, c := range []struct{ input, name, dataDir string }{
		{"y\n\n\n\n", "codex", "agentiscale-codex"},
		{"y\nBad Name\nhelper\n\n\n", "helper", "agentiscale-codex-helper"},
		{"y\ncodex-helper\n\n\n", "codex-helper", "agentiscale-codex-helper"},
		{"y\n" + strings.Repeat("a", 64) + "\n" + strings.Repeat("a", 63) + "\n\n\n", strings.Repeat("a", 63), "agentiscale-codex-" + strings.Repeat("a", 63)},
	} {
		w, out = testWizard(c.input)
		o, err = w.offerAgent()
		ownDir := filepath.Join(userDataDir(), "agentiscaled", "workspaces", c.name)
		if err != nil || o == nil || o.name != c.name || o.agentDir != ownDir || o.dataDir != filepath.Join(filepath.Dir(defaultDataDir()), c.dataDir) {
			t.Fatalf("input %q: offered %+v, %v\n%s", c.input, o, err, out)
		}
		for _, dataDir := range []string{defaultDataDir(), o.dataDir} {
			if err := agentDirApart(ownDir, dataDir); err != nil {
				t.Errorf("the agent's own directory and a node's: %v", err)
			}
		}
		if fi, err := os.Stat(ownDir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("the agent's own directory: %v, %v", fi, err)
		}
		if !strings.Contains(out.String(), "Directory the agent works in ["+homeRelative(ownDir)+"]: ") {
			t.Errorf("output:\n%s", out)
		}
	}
	if !strings.HasPrefix(homeRelative(defaultAgentDir("codex")), "~/") || homeRelative("/elsewhere") != "/elsewhere" {
		t.Errorf("home-relative default %s", homeRelative(defaultAgentDir("codex")))
	}
	if !strings.Contains(out.String(), "Name of the agent in your network [codex]: ") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSetUpAgentSignsInOnce(t *testing.T) {
	withDefaultDataDir(t)
	offer := o(t)
	var ran [][]string
	old := runAttached
	runAttached = func(_ context.Context, cmd *exec.Cmd) error {
		ran = append(ran, cmd.Args)
		if cmd.Args[len(cmd.Args)-1] == "login" {
			if !contains(cmd.Env, "CODEX_HOME="+filepath.Join(offer.dataDir, "codex")) {
				t.Errorf("login env lacks the agent's CODEX_HOME")
			}
		}
		return nil
	}
	t.Cleanup(func() { runAttached = old })
	w, out := testWizard("")
	if err := w.setUpAgent(context.Background(), "/bin/agentiscaled", offer); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 2 || ran[0][len(ran[0])-1] != "login" ||
		strings.Join(ran[1], " ") != "/bin/agentiscaled up -y -data-dir "+offer.dataDir+" -agent codex -agent-dir "+offer.agentDir+" -agent-owner-config=false" {
		t.Errorf("ran %q\n%s", ran, out)
	}

	// Signed in already: only the node is set up, with the owner's name and
	// policy.
	offer.policy, offer.name, offer.description = agentPolicy{}, "helper", "Answers questions"
	if err := os.WriteFile(filepath.Join(offer.dataDir, "codex", "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ran = nil
	if err := w.setUpAgent(context.Background(), "/bin/agentiscaled", offer); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0][1] != "up" || !strings.HasSuffix(strings.Join(ran[0], " "), " -agent-dir "+offer.agentDir+" -agent-name helper -agent-description Answers questions -agent-dir-instructions=false -agent-dir-skills=false -agent-owner-config=false") {
		t.Errorf("ran %q", ran)
	}
}

func o(t *testing.T) *agentOffer {
	dir := filepath.Join(os.Getenv("HOME"), "repo")
	return &agentOffer{runtime: "codex", agentDir: dir, dataDir: agentDataDir("codex", dir), policy: agentPolicy{DirInstructions: true, DirSkills: true}} // the agent's own config
}

// With the owner's configuration, the owner signs in to their own Codex,
// if they have not, and the agent's node gets no flag for it.
func TestSetUpAgentWithTheOwnersConfig(t *testing.T) {
	withDefaultDataDir(t)
	t.Setenv("CODEX_HOME", filepath.Join(os.Getenv("HOME"), "owner-codex"))
	t.Setenv("CODEX_API_KEY", "")
	var ran []*exec.Cmd
	old := runAttached
	runAttached = func(_ context.Context, cmd *exec.Cmd) error {
		ran = append(ran, cmd)
		return nil
	}
	t.Cleanup(func() { runAttached = old })
	offer := o(t)
	offer.policy.OwnerConfig = true
	w, out := testWizard("")
	if err := w.setUpAgent(context.Background(), "/bin/agentiscaled", offer); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 2 || strings.Join(ran[0].Args, " ") != "codex login" || ran[0].Env != nil ||
		strings.Join(ran[1].Args, " ") != "/bin/agentiscaled up -y -data-dir "+offer.dataDir+" -agent codex -agent-dir "+offer.agentDir {
		t.Errorf("ran %v\n%s", ran, out)
	}
	if err := os.MkdirAll(os.Getenv("CODEX_HOME"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ran = nil
	if err := w.setUpAgent(context.Background(), "/bin/agentiscaled", offer); err != nil || len(ran) != 1 {
		t.Errorf("signed in already: ran %v, %v", ran, err)
	}
}

// Two directories with the same name get nodes of their own; the same
// directory gets its node again.
func TestAgentDataDirAvoidsCollisions(t *testing.T) {
	withDefaultDataDir(t)
	a, b := filepath.Join(t.TempDir(), "service"), filepath.Join(t.TempDir(), "service")
	first := agentDataDir("codex", a)
	if err := os.MkdirAll(first, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := saveSettings(first, settings{Agent: "codex", AgentDir: a}); err != nil {
		t.Fatal(err)
	}
	if got := agentDataDir("codex", a); got != first {
		t.Errorf("same directory: %s, want %s", got, first)
	}
	if got, want := agentDataDir("codex", b), first+"-2"; got != want {
		t.Errorf("other directory with the same name: %s, want %s", got, want)
	}

	// A directory that holds a node, but not one for this agent directory,
	// is skipped: a sender node's, one with unreadable settings, and one with
	// none. One that holds only an agent's sign-in, left by a setup that
	// stopped before `up`, is taken.
	c := filepath.Join(t.TempDir(), "service")
	if err := os.MkdirAll(first+"-2", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := saveSettings(first+"-2", settings{Agent: "codex", AgentDir: b}); err != nil {
		t.Fatal(err)
	}
	for n, file := range map[int]string{3: settingsFile, 4: settingsFile, 5: "api-token", 6: filepath.Join("codex", "auth.json")} {
		content := map[int]string{3: `{"control": "https://c"}`, 4: "{not json", 5: "token", 6: "{}"}[n]
		write(t, filepath.Join(fmt.Sprintf("%s-%d", first, n), file), content)
	}
	if got, want := agentDataDir("codex", c), first+"-6"; got != want {
		t.Errorf("past a sender node, unreadable settings and no settings: %s, want %s", got, want)
	}
	if got, want := agentDataDir("codex", b), first+"-2"; got != want {
		t.Errorf("b again: %s, want %s", got, want)
	}
}

func TestUserDataDir(t *testing.T) {
	if goruntime.GOOS == "darwin" || goruntime.GOOS == "windows" {
		t.Skip("the configuration directory is the data directory here")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for xdg, want := range map[string]string{
		"":              filepath.Join(home, ".local", "share"),
		"relative/data": filepath.Join(home, ".local", "share"),
		"/srv/data":     "/srv/data",
	} {
		t.Setenv("XDG_DATA_HOME", xdg)
		if got := userDataDir(); got != want {
			t.Errorf("XDG_DATA_HOME=%q: %s, want %s", xdg, got, want)
		}
	}
}
