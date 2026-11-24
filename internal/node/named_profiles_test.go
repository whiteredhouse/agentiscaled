package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamedServicesSignInAndRunWithTheirOwnProfile(t *testing.T) {
	const cli = `#!/bin/sh
set -eu
case "$1" in
  --version) echo test-cli ;;
  --help) echo '--restricted --permission-prompts' ;;
  login) printf '%s' "$TEST_SIGN_IN" > "$CODEX_HOME/auth.json" ;;
  auth)
    case "$2" in
      login) printf '%s' "$TEST_SIGN_IN" > "$CLAUDE_CONFIG_DIR/test-credentials" ;;
      status)
        if [ -f "$CLAUDE_CONFIG_DIR/test-credentials" ]; then
          echo '{"loggedIn":true}'
        else
          echo '{"loggedIn":false}'
        fi ;;
      *) exit 1 ;;
    esac ;;
  exec)
    printf '%s' "$CODEX_HOME" > observed-profile
    while [ "$1" != '--output-last-message' ]; do shift; done
    cat "$CODEX_HOME/auth.json" > "$2" ;;
  -p)
    printf '%s' "$CLAUDE_CONFIG_DIR" > observed-profile
    printf '{"type":"result","result":"%s"}\n' "$(cat "$CLAUDE_CONFIG_DIR/test-credentials")" ;;
  *) exit 1 ;;
esac
`
	bin := filepath.Join(t.TempDir(), "local tools")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "node data")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	w, _ := testWizard("")
	for _, provider := range []struct{ name, command, env, login string }{
		{"codex", "", "CODEX_HOME", "login"},
		{"claude", filepath.Join(bin, "custom claude"), "CLAUDE_CONFIG_DIR", "auth login"},
	} {
		command := filepath.Join(bin, provider.name)
		if provider.command != "" {
			// Make accidentally using the default CLI fail even on hosts
			// where it is installed and signed in.
			if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			command = provider.command
		}
		if err := os.WriteFile(command, []byte(cli), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Run(provider.name, func(t *testing.T) {
			var services []agentConfig
			// Reusing the same two service names after switching providers
			// checks that the profile belongs to the service.
			for _, name := range []string{"reviewer", "writer"} {
				offer := agentOffer{runtime: provider.name, command: provider.command, name: name, dataDir: dataDir, agentDir: t.TempDir()}
				cfg := offer.config()
				profile := filepath.Join(dataDir, "agents", name, "profile")
				wantLogin := provider.env + "=" + shellQuote(profile) + " " + shellQuote(command) + " " + provider.login
				if err := checkAgent(t.Context(), &cfg, dataDir, true); err == nil || !strings.HasSuffix(err.Error(), wantLogin) {
					t.Fatalf("signed-out %s error = %v, want login command %q", name, err, wantLogin)
				}
				t.Setenv("TEST_SIGN_IN", provider.name+"-"+name)
				if err := w.prepareAgent(t.Context(), &offer); err != nil {
					t.Fatalf("sign in %s: %v", name, err)
				}
				t.Setenv("TEST_SIGN_IN", "unexpected-second-login")
				if err := w.prepareAgent(t.Context(), &offer); err != nil {
					t.Fatalf("reuse sign-in for %s: %v", name, err)
				}
				if err := checkAgent(t.Context(), &cfg, dataDir, true); err != nil {
					t.Fatalf("check %s: %v", name, err)
				}
				services = append(services, cfg)
			}

			// Both accounts are signed in before either service handles a task.
			for _, cfg := range services {
				rt, err := cfg.newRuntime(t.Context(), dataDir)
				if err != nil {
					t.Fatal(err)
				}
				result, err := rt.Run(t.Context(), cfg.Dir, taskRequest{Sender: testSender, Text: "who am I?"})
				if want := provider.name + "-" + cfg.Name; err != nil || result.Text != want {
					t.Errorf("%s task returned %q, %v; want account %q", cfg.Name, result.Text, err, want)
				}
				if got, want := readRecord(t, filepath.Join(cfg.Dir, "observed-profile")), filepath.Join(dataDir, "agents", cfg.Name, "profile"); got != want {
					t.Errorf("%s task ran with profile %q; want %q", cfg.Name, got, want)
				}
			}
		})
	}
}
