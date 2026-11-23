package node

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestACPOffersProbeInTheirWorkspaceAndKeepSeparateProfiles(t *testing.T) {
	data := t.TempDir()
	legacy := filepath.Join(data, "acp", "sh")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "account"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Both offers use the same interpreter and a relative script argument.
	agent, flags, err := splitACPCommand(fakeACPCommand(t, "-status", "-policies", "config-dir"))
	if err != nil {
		t.Fatal(err)
	}
	created := map[string]bool{}
	for _, name := range []string{"writer", "reviewer", "writer"} {
		dir := t.TempDir()
		const adapter = "printf '%s' \"$AGENTISCALE_CONFIG_DIR\" > observed-profile\nexec \"$@\"\n"
		if err := os.WriteFile(filepath.Join(dir, "adapter.sh"), []byte(adapter), 0o600); err != nil {
			t.Fatal(err)
		}
		a := agentConfig{Runtime: "acp", Name: name, Dir: dir, Command: "sh", Args: append([]string{"adapter.sh", agent}, flags...), OwnConfig: true}
		if err := checkAgent(t.Context(), &a, data, false); err != nil {
			t.Fatalf("validate offer %s: %v", name, err)
		}
		observed := filepath.Join(dir, "observed-profile")
		want := filepath.Join(data, "agents", name, "profile")
		readProfile := func() {
			t.Helper()
			got, err := os.ReadFile(observed)
			if err != nil || string(got) != want {
				t.Fatalf("%s profile = %q, err %v; want %q", name, got, err, want)
			}
		}
		readProfile()
		if err := os.Remove(observed); err != nil {
			t.Fatal(err)
		}
		if _, err := newAgent(t.Context(), &a, data); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		readProfile()
		account := filepath.Join(want, "account")
		got, err := os.ReadFile(account)
		if created[name] {
			if err != nil || string(got) != name {
				t.Fatalf("reopened %s account = %q, %v", name, got, err)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("new service %s inherited an account: %q, %v", name, got, err)
		}
		if err := os.WriteFile(account, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		created[name] = true
	}
	if got, err := os.ReadFile(filepath.Join(legacy, "account")); err != nil || string(got) != "legacy" {
		t.Fatalf("legacy profile changed: %q, %v", got, err)
	}
}

func TestACPCheckPrivateProfileIdentity(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	data, fixtures := t.TempDir(), t.TempDir()
	script := filepath.Join(fixtures, "record-profile.sh")
	record := filepath.Join(fixtures, "observed")
	// Record what the real checker supplies at initialize. The probe can
	// end here because the identity is the same for its subsequent turns.
	if err := os.WriteFile(script, []byte("printf '%s' \"$AGENTISCALE_CONFIG_DIR\" > \"$1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseCommand := "sh " + script + " " + record
	check := func(command string, extra ...string) string {
		t.Helper()
		args := append([]string{"-data-dir", data, "-owner-config=false", "-command", command}, extra...)
		if err := runRuntimeCheck(t.Context(), args, io.Discard); err == nil {
			t.Fatal("the recording agent did not reply to initialize")
		}
		raw, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(record); err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	unnamed := check(baseCommand)
	if filepath.Dir(unnamed) != filepath.Join(data, "acp-checks") || check(baseCommand) != unnamed {
		t.Fatalf("unstable unnamed profile: %q", unnamed)
	}
	if check(baseCommand+" --another") == unnamed || check(baseCommand, "-dir", fixtures) == unnamed {
		t.Fatal("different launch arguments or working directories shared a check profile")
	}
	if got, want := check(baseCommand, "-name", "reviewer"), filepath.Join(data, "agents", "reviewer", "profile"); got != want {
		t.Fatalf("named check selected %s, want %s", got, want)
	}
	seen := map[string]bool{}
	for _, name := range []string{"reviewer", "Reviewer", "other/../reviewer", "team.agent"} {
		path := check(baseCommand, "-name", name)
		if filepath.Base(path) != "profile" || filepath.Dir(filepath.Dir(path)) != filepath.Join(data, "agents") || seen[strings.ToLower(path)] {
			t.Fatalf("service name %q selected aliased or nested path %q", name, path)
		}
		seen[strings.ToLower(path)] = true
	}
}

func TestACPRejectsDuplicateEffectiveServiceName(t *testing.T) {
	c := config{dataDir: t.TempDir(), agents: []agentConfig{
		{Runtime: "acp", Command: "python3"},
		{Runtime: "acp", Name: "python3", Command: "some-other-agent"},
	}}
	if _, err := newAgents(t.Context(), &c); err == nil || !strings.Contains(err.Error(), `two agents are named "python3"`) {
		t.Fatalf("duplicate effective name: %v", err)
	}
}
