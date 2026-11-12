package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeLaunchd models launchctl where bootout returns before launchd has let
// go of the job: print still finds it for a few more calls, and a bootstrap
// until then fails the way launchd does.
type fakeLaunchd struct {
	loaded     bool
	lingering  int // print calls that still find the job after bootout
	bootstraps int
	ran        []string
}

func (f *fakeLaunchd) run(name string, args ...string) ([]byte, error) {
	f.ran = append(f.ran, args[0])
	switch args[0] {
	case "bootout":
		if !f.loaded {
			return []byte("Boot-out failed: 3: No such process"), errors.New("exit status 3")
		}
		f.loaded = false
	case "print":
		if f.lingering > 0 {
			f.lingering--
			return []byte("state = not running\n"), nil
		}
		if !f.loaded {
			return nil, errors.New("exit status 113")
		}
		return []byte("state = running\n"), nil
	case "bootstrap":
		f.bootstraps++
		if f.loaded || f.lingering > 0 {
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		f.loaded = true
	}
	return nil, nil
}

func useFakeLaunchd(t *testing.T, f *fakeLaunchd) *launchdService {
	t.Helper()
	oldRunner, oldWait := commandRunner, launchdWait
	commandRunner = f.run
	launchdWait.unload, launchdWait.load, launchdWait.poll = 200*time.Millisecond, 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() { commandRunner, launchdWait = oldRunner, oldWait })
	return &launchdService{label: "net.agentiscale.agentiscaled", home: t.TempDir(), uid: 501}
}

// Restarting a running node waits until launchd has unloaded the old job
// before it loads the new one, instead of failing with error 5.
func TestLaunchdRestartWaitsForBootout(t *testing.T) {
	f := &fakeLaunchd{loaded: true}
	svc := useFakeLaunchd(t, f)
	orig := f.run
	commandRunner = func(name string, args ...string) ([]byte, error) {
		out, err := orig(name, args...)
		if args[0] == "bootout" && err == nil {
			f.lingering = 3
		}
		return out, err
	}
	if err := svc.stop(); err != nil {
		t.Fatal(err)
	}
	if err := svc.start(serviceSpec{exe: "/usr/local/bin/agentiscaled", dataDir: "/Users/a/.config/agentiscale"}); err != nil {
		t.Fatalf("restart: %v (ran %v)", err, f.ran)
	}
	if !f.loaded || f.bootstraps != 1 {
		t.Fatalf("loaded %v after %d bootstraps; ran %v", f.loaded, f.bootstraps, f.ran)
	}
}

// launchd can refuse a bootstrap for a moment after the job is gone; start
// tries again within its wait.
func TestLaunchdStartRetriesBootstrap(t *testing.T) {
	f := &fakeLaunchd{}
	svc := useFakeLaunchd(t, f)
	refusals := 2
	orig := f.run
	commandRunner = func(name string, args ...string) ([]byte, error) {
		if args[0] == "bootstrap" && refusals > 0 {
			refusals--
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		return orig(name, args...)
	}
	if err := svc.start(serviceSpec{exe: "/usr/local/bin/agentiscaled", dataDir: "/Users/a/.config/agentiscale"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !f.loaded {
		t.Fatalf("not loaded; ran %v", f.ran)
	}
}

// A job that never unloads, and a bootstrap that keeps failing, are
// reported instead of waited on forever.
func TestLaunchdGivesUp(t *testing.T) {
	f := &fakeLaunchd{loaded: true}
	svc := useFakeLaunchd(t, f)
	commandRunner = func(name string, args ...string) ([]byte, error) {
		if args[0] == "bootout" {
			return nil, nil // claims success, but the job stays
		}
		return f.run(name, args...)
	}
	if err := svc.stop(); err == nil || !strings.Contains(err.Error(), "still loaded") {
		t.Fatalf("stop of a job that stays: %v", err)
	}

	f = &fakeLaunchd{}
	svc = useFakeLaunchd(t, f)
	commandRunner = func(name string, args ...string) ([]byte, error) {
		if args[0] == "bootstrap" {
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		return f.run(name, args...)
	}
	err := svc.start(serviceSpec{exe: "/usr/local/bin/agentiscaled", dataDir: "/Users/a/.config/agentiscale"})
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("start with a bootstrap that keeps failing: %v", err)
	}
}
