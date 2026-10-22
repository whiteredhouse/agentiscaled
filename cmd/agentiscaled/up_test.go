package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAuth0 is a stand-in for Auth0's device authorization endpoints. Each
// token poll takes the next answer from polls; the last one repeats.
type fakeAuth0 struct {
	mu    sync.Mutex
	polls []string // OAuth error codes, or "" for a token
	forms []map[string]string
	srv   *httptest.Server
}

func newFakeAuth0(t *testing.T, polls ...string) *fakeAuth0 {
	f := &fakeAuth0{polls: polls}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{"path": r.URL.Path}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		f.mu.Lock()
		f.forms = append(f.forms, form)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "DEV", "user_code": "ABCD-EFGH",
				"verification_uri":          f.srv.URL + "/activate",
				"verification_uri_complete": f.srv.URL + "/activate?user_code=ABCD-EFGH",
				"expires_in":                900, "interval": 0,
			})
		case "/oauth/token":
			f.mu.Lock()
			next := f.polls[0]
			if len(f.polls) > 1 {
				f.polls = f.polls[1:]
			}
			f.mu.Unlock()
			if next == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "ACCESS", "token_type": "Bearer"})
				return
			}
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": next, "error_description": "test"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	t.Setenv("AGENTISCALE_NO_BROWSER", "1")
	return f
}

func (f *fakeAuth0) config() authConfig {
	return authConfig{issuer: f.srv.URL + "/", clientID: "CLIENT", audience: "https://control.test"}
}

// The device flow asks for the control service's audience,
// waits while the owner has not approved, backs off when told to, and
// returns the access token.
func TestDeviceLogin(t *testing.T) {
	f := newFakeAuth0(t, "authorization_pending", "slow_down", "")
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token, err := deviceLogin(ctx, f.config(), &out)
	if err != nil || token != "ACCESS" {
		t.Fatalf("deviceLogin = %q, %v", token, err)
	}
	if !strings.Contains(out.String(), "/activate?user_code=ABCD-EFGH") || !strings.Contains(out.String(), "ABCD-EFGH") {
		t.Errorf("output does not show the link and code:\n%s", out.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	start := f.forms[0]
	if start["path"] != "/oauth/device/code" || start["client_id"] != "CLIENT" || start["audience"] != "https://control.test" {
		t.Errorf("device code request = %v", start)
	}
	// No refresh token is asked for: the sign-in is for enrollment only (c3).
	if strings.Contains(start["scope"], "offline_access") {
		t.Errorf("scope %q asks for a refresh token", start["scope"])
	}
	if polls := len(f.forms) - 1; polls != 3 {
		t.Errorf("token polls = %d, want 3", polls)
	}
	for _, poll := range f.forms[1:] {
		if poll["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" || poll["device_code"] != "DEV" {
			t.Errorf("token poll = %v", poll)
		}
	}
}

func TestDeviceLoginEnds(t *testing.T) {
	for code, want := range map[string]string{
		"expired_token": "expired",
		"access_denied": "cancelled",
		"invalid_grant": "invalid_grant",
	} {
		f := newFakeAuth0(t, code)
		_, err := deviceLogin(context.Background(), f.config(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", code, err, want)
		}
	}
}

// The node sends the owner's token and its peer ID, and waits
// while the network's mesh is still starting.
func TestEnrollSignedInWaitsForTheNetwork(t *testing.T) {
	old := enrollRetry
	enrollRetry = 10 * time.Millisecond
	defer func() { enrollRetry = old }()

	var calls int
	var got []map[string]string
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" || r.Header.Get("Authorization") != "Bearer ACCESS" {
			http.Error(w, `{"error":"sign in required"}`, http.StatusUnauthorized)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		if calls++; calls < 3 {
			http.Error(w, `{"error":"network is not ready yet"}`, http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(joinAnswer{NetworkID: "n1", MeshURL: "https://n1.mesh.test", BootstrapToken: "bt"})
	}))
	defer control.Close()

	answer, err := enrollSignedIn(context.Background(), control.URL+"/", "ACCESS", "12D3KooWPeer", "mac")
	if err != nil || answer.BootstrapToken != "bt" {
		t.Fatalf("enrollSignedIn = %+v, %v", answer, err)
	}
	if calls != 3 || got[0]["peer_id"] != "12D3KooWPeer" || got[0]["machine_name"] != "mac" {
		t.Fatalf("calls %d, bodies %v", calls, got)
	}

	// A refusal is final.
	if _, err := enrollSignedIn(context.Background(), control.URL, "WRONG", "p", ""); err == nil || !strings.Contains(err.Error(), "sign in required") {
		t.Fatalf("refused enroll: %v", err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := settings{Control: "https://c", BindAddr: "", Listen: "/ip4/0.0.0.0/tcp/1", LogLevel: "info",
		Agent: "codex", AgentDir: "/repo", AgentCommand: "/bin/codex", AgentDescription: "Answers questions", MachineName: "mac", AgentEnv: []string{"CODEX_HOME=/codex"},
		PeerID: "12D3", NetworkID: "af75fb5f-b7ee-4dac-aaab-d88de92dba80", MeshURL: "https://af75fb5f-b7ee-4dac-aaab-d88de92dba80.mesh.agentiscale.net"}
	if err := saveSettings(dir, want); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, settingsFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file: %v, mode %v", err, fi.Mode())
	}
	got, err := loadSettings(dir)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded %+v, %v", got, err)
	}
	c := got.config(dir)
	if c.bindAddr != "" || c.agent != "codex" || c.dataDir != dir || c.signInToken != "" || !c.agentPolicy.OwnerConfig || len(c.agentEnv) != 1 {
		t.Fatalf("config %+v", c)
	}
	if id := networkFromMeshURL(got.MeshURL); id != "af75fb5f-b7ee-4dac-aaab-d88de92dba80" {
		t.Fatalf("network ID %q", id)
	}
	if id := networkFromMeshURL("http://127.0.0.1:18080"); id != "" {
		t.Fatalf("network ID of a local mesh %q", id)
	}
}

// The LaunchAgent runs `agentiscaled run` for its data directory,
// keeps it alive, starts it at login, and survives odd characters in paths.
func TestLaunchdPlist(t *testing.T) {
	spec := serviceSpec{exe: "/opt/homebrew/bin/agentiscaled", dataDir: "/Users/a/Library/Application Support/agentiscale & <x>", path: "/opt/homebrew/bin:/usr/bin"}
	var plist struct {
		Dict struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"dict"`
	}
	text := launchdPlist("net.agentiscale.agentiscaled", spec)
	if err := xml.Unmarshal([]byte(text), &plist); err != nil {
		t.Fatalf("plist is not XML: %v\n%s", err, text)
	}
	for _, want := range []string{
		"<string>/opt/homebrew/bin/agentiscaled</string>\n\t\t<string>run</string>",
		"<string>/Users/a/Library/Application Support/agentiscale &amp; &lt;x&gt;</string>",
		"<key>RunAtLoad</key>\n\t<true/>", "<key>KeepAlive</key>\n\t<true/>",
		"<string>/opt/homebrew/bin:/usr/bin</string>",
		"agentiscale &amp; &lt;x&gt;/agentiscaled.log",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plist lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "UserName") || strings.Contains(text, "root") {
		t.Error("the agent must run as the user")
	}
}

func TestSystemdService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	var ran []string
	old := commandRunner
	commandRunner = func(name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if name == "loginctl" {
			return []byte("no\n"), nil
		}
		return nil, nil
	}
	defer func() { commandRunner = old }()

	svc := &systemdService{unit: unitName("/srv/node b"), home: home}
	if !strings.HasPrefix(svc.unit, "agentiscaled-") || svc.unit == unitName(defaultDataDir()) {
		t.Fatalf("unit for another data directory = %q", svc.unit)
	}
	if err := svc.start(serviceSpec{exe: "/usr/bin/agentiscaled", dataDir: "/srv/node b", path: "/usr/bin"}); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(svc.unitPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`ExecStart="/usr/bin/agentiscaled" run -data-dir "/srv/node b"`, "Restart=always", "WantedBy=default.target", `Environment="PATH=/usr/bin"`} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if want := []string{"systemctl --user daemon-reload", "systemctl --user enable " + svc.unit, "systemctl --user restart " + svc.unit}; strings.Join(ran, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran %q, want %q", ran, want)
	}
	if hint := svc.hint(); !strings.Contains(hint, "loginctl enable-linger") {
		t.Errorf("hint = %q", hint)
	}
	ran = nil
	if err := svc.remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(svc.unitPath()); !os.IsNotExist(err) {
		t.Errorf("unit still there: %v", err)
	}
	if ran[0] != "systemctl --user disable --now "+svc.unit {
		t.Errorf("remove ran %q", ran)
	}
	if state, running := svc.state(); state != "not installed" || running {
		t.Errorf("state = %q, %v", state, running)
	}
}

// A service installed from Homebrew starts Homebrew's stable
// link, so `brew upgrade` moves it to the new version.
func TestStablePathFollowsHomebrewLinks(t *testing.T) {
	for _, versioned := range [][]string{
		{"Cellar", "agentiscaled", "0.1.0", "bin"}, // formula
		{"Caskroom", "agentiscaled", "0.1.0"},      // cask
	} {
		prefix := t.TempDir()
		dir := filepath.Join(append([]string{prefix}, versioned...)...)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		exe := filepath.Join(dir, "agentiscaled")
		if err := os.WriteFile(exe, nil, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(prefix, "bin", "agentiscaled")
		if err := os.Symlink(exe, link); err != nil {
			t.Fatal(err)
		}
		resolvedLink, _ := filepath.EvalSymlinks(filepath.Dir(link))
		if got := stablePath(exe); filepath.Dir(got) != filepath.Dir(link) && filepath.Dir(got) != resolvedLink {
			t.Errorf("stablePath(%s) = %q, want %q", versioned[0], got, link)
		}
		if got := stablePath(link); !strings.HasSuffix(got, "/bin/agentiscaled") || strings.Contains(got, versioned[0]) {
			t.Errorf("stablePath(%s link) = %q, want the link", versioned[0], got)
		}
	}
	other := filepath.Join(t.TempDir(), "agentiscaled")
	if err := os.WriteFile(other, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := stablePath(other); !strings.HasSuffix(got, "agentiscaled") || strings.Contains(got, "Cellar") {
		t.Errorf("stablePath(plain) = %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/bin/agentiscaled":                            "/usr/bin/agentiscaled",
		"/Users/a/Library/Application Support/agentiscale": "'/Users/a/Library/Application Support/agentiscale'",
		"it's": `'it'\''s'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// status tells a node that is starting from one that is down by whether its
// service runs, in each service manager's own words.
func TestServiceStateSaysWhetherItRuns(t *testing.T) {
	old := commandRunner
	defer func() { commandRunner = old }()
	home := t.TempDir()
	launchd := &launchdService{label: "net.agentiscale.agentiscaled", home: home, uid: 501}
	systemd := &systemdService{unit: "agentiscaled.service", home: home}
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, path := range []string{launchd.plistPath(), systemd.unitPath()} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		svc     service
		out     string
		desc    string
		running bool
	}{
		{launchd, "gui/501/net.agentiscale.agentiscaled = {\n\tstate = running\n\tpid = 824\n}", "installed, running", true},
		{launchd, "gui/501/net.agentiscale.agentiscaled = {\n\tstate = not running\n}", "installed, not running", false},
		{systemd, "active\n", "installed, active", true},
		{systemd, "activating\n", "installed, activating", false},
		{systemd, "failed\n", "installed, failed", false},
	} {
		commandRunner = func(string, ...string) ([]byte, error) { return []byte(c.out), nil }
		if desc, running := c.svc.state(); desc != c.desc || running != c.running {
			t.Errorf("%s with %q: %q, %v; want %q, %v", c.svc.name(), c.out, desc, running, c.desc, c.running)
		}
	}
}
