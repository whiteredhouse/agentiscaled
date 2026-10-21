package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
)

// The per-user background service that keeps a node running: a
// launchd LaunchAgent on macOS and a systemd user unit on Linux. Never root.

const logFile = "agentiscaled.log"

// serviceSpec is what the service runs: `<exe> run -data-dir <dataDir>`.
type serviceSpec struct {
	exe     string
	dataDir string
	path    string // PATH for the node and its agent CLI
}

type service interface {
	name() string
	// start installs the service, or replaces it, and starts it now and at
	// every login.
	start(serviceSpec) error
	// stop stops a running service and keeps it installed.
	stop() error
	// remove stops the service and uninstalls it.
	remove() error
	// state says whether the service is installed and running.
	state() string
	// hint is advice to print after start, if any.
	hint() string
}

// commandRunner runs a system command; tests replace it.
var commandRunner = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// newService returns the background service for a node's data directory on
// this system, or nil where agentiscaled has none.
func newService(dataDir string) service {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	switch goruntime.GOOS {
	case "darwin":
		return &launchdService{label: serviceLabel(dataDir), home: home, uid: os.Getuid()}
	case "linux":
		return &systemdService{unit: unitName(dataDir), home: home}
	}
	return nil
}

// serviceSuffix tells apart the services of several nodes on one machine,
// each with its own data directory. The default data
// directory has none.
func serviceSuffix(dataDir string) string {
	if dataDir == defaultDataDir() {
		return ""
	}
	sum := sha256.Sum256([]byte(dataDir))
	return "-" + hex.EncodeToString(sum[:4])
}

func serviceLabel(dataDir string) string {
	return "net.agentiscale.agentiscaled" + strings.Replace(serviceSuffix(dataDir), "-", ".", 1)
}

func unitName(dataDir string) string {
	return "agentiscaled" + serviceSuffix(dataDir) + ".service"
}

// launchdService is a LaunchAgent in ~/Library/LaunchAgents, run in the
// user's GUI session.
type launchdService struct {
	label string
	home  string
	uid   int
}

func (s *launchdService) name() string { return "launchd agent " + s.label }

func (s *launchdService) plistPath() string {
	return filepath.Join(s.home, "Library", "LaunchAgents", s.label+".plist")
}

func (s *launchdService) target() string { return "gui/" + strconv.Itoa(s.uid) + "/" + s.label }

func (s *launchdService) start(spec serviceSpec) error {
	if err := os.MkdirAll(filepath.Dir(s.plistPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.plistPath(), []byte(launchdPlist(s.label, spec)), 0o644); err != nil {
		return err
	}
	_ = s.stop()
	if out, err := commandRunner("launchctl", "bootstrap", "gui/"+strconv.Itoa(s.uid), s.plistPath()); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *launchdService) stop() error {
	// bootout fails when the agent is not loaded, which is fine.
	_, _ = commandRunner("launchctl", "bootout", s.target())
	return nil
}

func (s *launchdService) remove() error {
	_ = s.stop()
	if err := os.Remove(s.plistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *launchdService) state() string {
	if _, err := os.Stat(s.plistPath()); err != nil {
		return "not installed"
	}
	out, err := commandRunner("launchctl", "print", s.target())
	if err != nil {
		return "installed, not loaded"
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "state = "); ok {
			return "installed, " + v
		}
	}
	return "installed"
}

func (s *launchdService) hint() string { return "" }

func launchdPlist(label string, spec serviceSpec) string {
	esc := html.EscapeString
	log := filepath.Join(spec.dataDir, logFile)
	var env string
	if spec.path != "" {
		env = "\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>PATH</key>\n\t\t<string>" + esc(spec.path) + "</string>\n\t</dict>\n"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + esc(label) + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + esc(spec.exe) + `</string>
		<string>run</string>
		<string>-data-dir</string>
		<string>` + esc(spec.dataDir) + `</string>
	</array>
` + env + `	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>` + esc(log) + `</string>
	<key>StandardErrorPath</key>
	<string>` + esc(log) + `</string>
</dict>
</plist>
`
}

// systemdService is a systemd user unit in ~/.config/systemd/user.
type systemdService struct {
	unit string
	home string
}

func (s *systemdService) name() string { return "systemd user unit " + s.unit }

func (s *systemdService) unitPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(s.home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", s.unit)
}

func (s *systemdService) systemctl(args ...string) error {
	out, err := commandRunner("systemctl", append([]string{"--user"}, args...)...)
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *systemdService) start(spec serviceSpec) error {
	if err := os.MkdirAll(filepath.Dir(s.unitPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.unitPath(), []byte(systemdUnit(spec)), 0o644); err != nil {
		return err
	}
	if err := s.systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := s.systemctl("enable", s.unit); err != nil {
		return err
	}
	return s.systemctl("restart", s.unit)
}

func (s *systemdService) stop() error {
	if _, err := os.Stat(s.unitPath()); err != nil {
		return nil
	}
	// Stopping a unit that is not running succeeds.
	return s.systemctl("stop", s.unit)
}

func (s *systemdService) remove() error {
	if _, err := os.Stat(s.unitPath()); err != nil {
		return nil
	}
	if err := s.systemctl("disable", "--now", s.unit); err != nil {
		return err
	}
	if err := os.Remove(s.unitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.systemctl("daemon-reload")
}

func (s *systemdService) state() string {
	if _, err := os.Stat(s.unitPath()); err != nil {
		return "not installed"
	}
	out, _ := commandRunner("systemctl", "--user", "is-active", s.unit)
	return "installed, " + strings.TrimSpace(string(out))
}

// hint tells the owner of a server how to keep the node running after they
// log out: systemd stops a user's units then unless lingering is on.
func (s *systemdService) hint() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	out, err := commandRunner("loginctl", "show-user", u.Username, "--property=Linger", "--value")
	if err != nil || strings.TrimSpace(string(out)) == "yes" {
		return ""
	}
	return "To keep the node running after you log out (on a server, for example), run:\n  sudo loginctl enable-linger " + u.Username
}

func systemdUnit(spec serviceSpec) string {
	var env string
	if spec.path != "" {
		env = "Environment=" + systemdQuote("PATH="+spec.path) + "\n"
	}
	return `[Unit]
Description=agentiscale node (` + strings.ReplaceAll(spec.dataDir, "%", "%%") + `)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=` + systemdQuote(spec.exe) + ` run -data-dir ` + systemdQuote(spec.dataDir) + `
` + env + `Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`
}

// systemdQuote quotes one word for a unit file: double quotes, with
// backslashes, quotes and specifiers escaped.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")
	return `"` + r.Replace(s) + `"`
}

// stableExecutable is the path the service should start. A Homebrew install
// runs from a versioned Cellar or Caskroom directory that `brew upgrade`
// removes, so the service starts Homebrew's stable link to it instead, and
// picks up upgrades.
func stableExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return stablePath(exe), nil
}

func stablePath(exe string) string {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	for _, store := range []string{"/Cellar/", "/Caskroom/"} {
		prefix, rest, ok := strings.Cut(exe, store)
		if !ok {
			continue
		}
		// <prefix>/Cellar/<formula>/<version>/bin/<name> or
		// <prefix>/Caskroom/<cask>/<version>/<name>
		parts := strings.Split(rest, "/")
		if (store == "/Cellar/" && (len(parts) != 4 || parts[2] != "bin")) || (store == "/Caskroom/" && len(parts) != 3) {
			continue
		}
		name := parts[len(parts)-1]
		link := filepath.Join(prefix, "bin", name)
		if target, err := filepath.EvalSymlinks(link); err == nil && strings.Contains(target, store+parts[0]+"/") {
			return link
		}
	}
	return exe
}
