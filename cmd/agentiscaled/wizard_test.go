package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	repo := filepath.Join(t.TempDir(), "My Repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"", "\n", "n\n"} {
		w, _ := testWizard(input)
		if o, err := w.offerAgent(); o != nil || err != nil {
			t.Errorf("input %q: offered %+v, %v", input, o, err)
		}
	}
	w, out := testWizard("y\n\n/does/not/exist\n" + repo + "\n")
	o, err := w.offerAgent()
	if err != nil || o == nil {
		t.Fatalf("%+v, %v\n%s", o, err, out)
	}
	wantDir := filepath.Join(filepath.Dir(defaultDataDir()), "agentiscale-codex-my-repo")
	if o.runtime != "codex" || o.agentDir != repo || o.dataDir != wantDir {
		t.Errorf("offer %+v, want data dir %s", o, wantDir)
	}
	if !strings.Contains(out.String(), "/does/not/exist is not a directory") {
		t.Errorf("output:\n%s", out)
	}
	w, _ = testWizard("y\n")
	if o, err := w.offerAgent(); o != nil || err != nil {
		t.Errorf("no directory: offered %+v, %v", o, err)
	}
}

func TestSetUpAgentSignsInOnce(t *testing.T) {
	withDefaultDataDir(t)
	var ran [][]string
	old := runAttached
	runAttached = func(_ context.Context, cmd *exec.Cmd) error {
		ran = append(ran, cmd.Args)
		if cmd.Args[len(cmd.Args)-1] == "login" {
			if !contains(cmd.Env, "CODEX_HOME="+filepath.Join(o(t).dataDir, "codex")) {
				t.Errorf("login env lacks the agent's CODEX_HOME")
			}
		}
		return nil
	}
	t.Cleanup(func() { runAttached = old })
	offer := o(t)
	w, out := testWizard("")
	if err := w.setUpAgent(context.Background(), "/bin/agentiscaled", offer); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 2 || ran[0][len(ran[0])-1] != "login" ||
		strings.Join(ran[1], " ") != "/bin/agentiscaled up -y -data-dir "+offer.dataDir+" -agent codex -agent-dir "+offer.agentDir {
		t.Errorf("ran %q\n%s", ran, out)
	}

	// Signed in already: only the node is set up.
	if err := os.WriteFile(filepath.Join(offer.dataDir, "codex", "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ran = nil
	if err := w.setUpAgent(context.Background(), "/bin/agentiscaled", offer); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0][1] != "up" {
		t.Errorf("ran %q", ran)
	}
}

func o(t *testing.T) *agentOffer {
	dir := filepath.Join(os.Getenv("HOME"), "repo")
	return &agentOffer{runtime: "codex", agentDir: dir, dataDir: agentDataDir("codex", dir)}
}
