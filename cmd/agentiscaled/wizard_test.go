package main

import (
	"bufio"
	"bytes"
	"context"
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
		input string
		want  string
	}{
		{"\n\n", "claude-desktop,codex"},
		{"", "claude-desktop,codex"}, // no answer is every default
		{"n\ny\n", "codex"},
		{"no\nNO\n", "none"},
		{"maybe\ny\n\n", "claude-desktop,codex"},
		{"y\n", "claude-desktop,codex"},
	} {
		w, out := testWizard(tc.input)
		got, err := w.chooseClients(both)
		if err != nil || got != tc.want {
			t.Errorf("input %q: got %q, %v, want %q\n%s", tc.input, got, err, tc.want, out)
		}
	}
	w, out := testWizard("")
	if got, _ := w.chooseClients(nil); got != clientsAuto || out.Len() != 0 {
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
	wantDir := defaultDataDir()
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
	for _, c := range []struct{ input, name string }{
		{"y\n\n\n\n", "codex"},
		{"y\nBad Name\nhelper\n\n\n", "helper"},
		{"y\ncodex-helper\n\n\n", "codex-helper"},
		{"y\n" + strings.Repeat("a", 64) + "\n" + strings.Repeat("a", 63) + "\n\n\n", strings.Repeat("a", 63)},
	} {
		w, out = testWizard(c.input)
		o, err = w.offerAgent()
		ownDir := filepath.Join(userDataDir(), "agentiscaled", "workspaces", c.name)
		if err != nil || o == nil || o.name != c.name || o.agentDir != ownDir || o.dataDir != defaultDataDir() {
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
	if err := w.prepareAgent(context.Background(), offer); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0][len(ran[0])-1] != "login" {
		t.Errorf("ran %q\n%s", ran, out)
	}

	// Signed in already: preparation launches no command.
	offer.policy, offer.name, offer.description = agentPolicy{}, "helper", "Answers questions"
	if err := os.WriteFile(filepath.Join(offer.dataDir, "codex", "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ran = nil
	if err := w.prepareAgent(context.Background(), offer); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 0 {
		t.Errorf("signed-in runtime started another command: %q", ran)
	}

}

func o(t *testing.T) *agentOffer {
	dir := filepath.Join(os.Getenv("HOME"), "repo")
	return &agentOffer{runtime: "codex", agentDir: dir, dataDir: defaultDataDir(), policy: agentPolicy{DirInstructions: true, DirSkills: true}} // the agent's own config
}

// With the owner's configuration, preparation signs in to their own Codex.
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
	if err := w.prepareAgent(context.Background(), offer); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || strings.Join(ran[0].Args, " ") != "codex login" || ran[0].Env != nil {
		t.Errorf("ran %v\n%s", ran, out)
	}

	if err := os.MkdirAll(os.Getenv("CODEX_HOME"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ran = nil
	if err := w.prepareAgent(context.Background(), offer); err != nil || len(ran) != 0 {
		t.Errorf("signed in already: ran %v, %v", ran, err)
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
