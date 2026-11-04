package main

import (
	"bytes"
	"context"
	"errors"
	meshnode "github.com/google/agentmesh/export/node"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Model the OS supervisor, while retaining the real service config writes.
// Tests never invoke launchctl/systemctl on the user's machine.
type testSupervisor struct {
	running                   bool
	stops, starts, failStarts int
	onStop                    func()
}

func installTestSupervisor(t *testing.T, dir string, running bool) *testSupervisor {
	t.Helper()
	old := commandRunner
	state := &testSupervisor{}
	commandRunner = func(command string, args ...string) ([]byte, error) {
		action := ""
		if len(args) > 0 {
			action = args[0]
		}
		if command == "systemctl" && len(args) > 1 {
			action = args[1]
		}
		switch action {
		case "print":
			if state.running {
				return []byte("state = running\n"), nil
			}
			return []byte("state = waiting\n"), nil
		case "is-active":
			if state.running {
				return []byte("active\n"), nil
			}
			return []byte("inactive\n"), nil
		case "bootout", "stop":
			state.running = false
			state.stops++
			if state.onStop != nil {
				state.onStop()
			}
		case "bootstrap", "restart":
			state.starts++
			if state.failStarts > 0 {
				state.failStarts--
				return nil, errors.New("test service start failed")
			}
			state.running = true
		}
		return []byte("yes\n"), nil
	}
	t.Cleanup(func() { commandRunner = old })
	svc := newService(dir)
	if svc == nil {
		t.Skip("no supported user service")
	}
	if err := svc.start(serviceSpec{exe: "/old/agentiscaled", dataDir: dir, path: "/old/path"}); err != nil {
		t.Fatal(err)
	}
	state.running, state.starts, state.stops = running, 0, 0
	return state
}

func upWizardInput(t *testing.T, text string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	oldInput, oldInteractive := os.Stdin, interactive
	os.Stdin, interactive = f, func() bool { return true }
	t.Cleanup(func() { os.Stdin, interactive = oldInput, oldInteractive; f.Close() })
}

func seededUpSettings(t *testing.T) (string, settings) {
	t.Helper()
	dir := withDefaultDataDir(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	mesh := newStandInMesh(t, "fine")
	pid := seedNode(t, dir, mesh.srv.URL, mesh.key)
	saved := settings{Control: "https://control.test", BindAddr: "", Listen: "/ip4/127.0.0.1/tcp/0", LogLevel: "info", PeerID: pid, NetworkID: "saved-network", MeshURL: mesh.srv.URL, Path: "/old/path"}
	writeSettings(t, dir, saved)
	return dir, saved
}

func TestUpAgentSetupFailureKeepsServiceRunning(t *testing.T) {
	for _, failure := range []string{"login cancelled", "invalid workspace"} {
		t.Run(failure, func(t *testing.T) {
			dir, saved := seededUpSettings(t)
			supervisor := installTestSupervisor(t, dir, true)
			home := filepath.Join(t.TempDir(), "codex-home")
			t.Setenv("CODEX_HOME", home)
			t.Setenv("CODEX_API_KEY", "")
			workspace := t.TempDir()
			if failure == "invalid workspace" {
				workspace = dir
				write(t, filepath.Join(home, "auth.json"), "{}")
			}
			upWizardInput(t, "y\n\n\n"+workspace+"\n\n\n\n")
			oldRun := runAttached
			runAttached = func(context.Context, *exec.Cmd) error {
				if !supervisor.running {
					t.Error("working node was stopped during runtime login")
				}
				return errors.New("login cancelled")
			}
			t.Cleanup(func() { runAttached = oldRun })
			if err := runUp(context.Background(), []string{"-clients", "none"}, io.Discard); err == nil {
				t.Fatal("setup unexpectedly succeeded")
			}
			got, err := loadSettings(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !supervisor.running || supervisor.stops != 0 || !reflect.DeepEqual(got, saved) {
				t.Fatalf("setup disrupted working node: running=%v stops=%d settings=%+v", supervisor.running, supervisor.stops, got)
			}
		})
	}
}

func TestUpRestoresRunningServiceAfterStartFailure(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(map[bool]string{true: "running", false: "stopped"}[running], func(t *testing.T) {
			dir, saved := seededUpSettings(t)
			supervisor := installTestSupervisor(t, dir, running)
			supervisor.failStarts = 1
			err := runUp(context.Background(), []string{"-y", "-clients", "none", "-log-level", "debug"}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "test service start failed") {
				t.Fatalf("startup failure = %v", err)
			}
			got, loadErr := loadSettings(dir)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if supervisor.running != running {
				t.Fatalf("running=%v, was %v", supervisor.running, running)
			}
			if running && (!reflect.DeepEqual(got, saved) || supervisor.starts != 2) {
				t.Fatalf("did not restore previous settings/service: %+v starts=%d", got, supervisor.starts)
			}
			if !running && supervisor.starts != 1 {
				t.Fatalf("restarted a previously stopped service: %d starts", supervisor.starts)
			}
		})
	}
}

func TestUpReportsRecoveryFailure(t *testing.T) {
	dir, _ := seededUpSettings(t)
	supervisor := installTestSupervisor(t, dir, true)
	supervisor.failStarts = 2
	err := runUp(context.Background(), []string{"-y", "-clients", "none"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "previous") {
		t.Fatalf("recovery failure missing from error: %v", err)
	}
	if supervisor.running {
		t.Fatal("reported restored service despite restart failure")
	}
}

func TestUpUsesShellCodexHomeForClientSetup(t *testing.T) {
	for _, savedOverride := range []bool{true, false} {
		t.Run(map[bool]string{true: "saved override", false: "saved default"}[savedOverride], func(t *testing.T) {
			dir, saved := seededUpSettings(t)
			installTestSupervisor(t, dir, false)
			savedHome := filepath.Join(os.Getenv("HOME"), ".codex")
			if savedOverride {
				savedHome = filepath.Join(t.TempDir(), "hosted-config")
				saved.AgentEnv = []string{"CODEX_HOME=" + savedHome}
			}
			write(t, filepath.Join(savedHome, "auth.json"), "{}")
			clientHome := filepath.Join(t.TempDir(), "client-config")
			t.Setenv("CODEX_HOME", clientHome)
			logPath := filepath.Join(t.TempDir(), "client-env")
			bin := t.TempDir()
			command := filepath.Join(bin, "codex")
			write(t, command, "#!/bin/sh\nif [ \"$1\" = --version ]; then echo codex-test; exit 0; fi\nprintf '%s\\n' \"$CODEX_HOME\" >> "+shellQuote(logPath)+"\nif [ \"$2\" = list ]; then echo '[]'; fi\n")
			if err := os.Chmod(command, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			saved.Agent, saved.AgentDir, saved.AgentCommand = "codex", t.TempDir(), command
			writeSettings(t, dir, saved)
			var output bytes.Buffer
			if err := runUp(context.Background(), []string{"-y", "-foreground", "-clients", "codex"}, &output); err != nil {
				t.Fatalf("up: %v\n%s", err, &output)
			}
			env, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("client was not connected: %v\n%s", err, &output)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(env)), "\n") {
				if line != clientHome {
					t.Errorf("client used CODEX_HOME %q, want %q", line, clientHome)
				}
			}
		})
	}
}

func TestUpRestoresServiceAfterSettingsSaveFailure(t *testing.T) {
	dir, saved := seededUpSettings(t)
	supervisor := installTestSupervisor(t, dir, true)
	supervisor.onStop = func() {
		supervisor.onStop = nil
		if err := os.Mkdir(filepath.Join(dir, settingsFile+".tmp"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	err := runUp(context.Background(), []string{"-y", "-clients", "none", "-log-level", "debug"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "save settings") {
		t.Fatalf("save error = %v", err)
	}
	got, loadErr := loadSettings(dir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !supervisor.running || supervisor.starts != 1 || !reflect.DeepEqual(got, saved) {
		t.Fatalf("save failure did not restore service: running=%v starts=%d settings=%+v", supervisor.running, supervisor.starts, got)
	}
}

func TestUpStopsInstalledServiceBeforeStoreAccess(t *testing.T) {
	dir, _ := seededUpSettings(t)
	supervisor := installTestSupervisor(t, dir, false)
	// A restartable job can be between attempts while its last process is
	// releasing the store. Stopping the installed job must settle that first.
	store, err := meshnode.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	supervisor.onStop = func() { supervisor.onStop = nil; store.Close() }
	if err := runUp(context.Background(), []string{"-y", "-foreground", "-clients", "none"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if supervisor.stops != 1 || supervisor.starts != 0 {
		t.Fatalf("service transitions: stops=%d starts=%d", supervisor.stops, supervisor.starts)
	}
}
