package main

import (
	"fmt"
	goruntime "runtime"
	"runtime/debug"
)

// version and commit are set by the release build with
// -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = ""
)

func versionString() string {
	c := commit
	if c == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" {
					c = s.Value
				}
			}
		}
	}
	if len(c) > 12 {
		c = c[:12]
	}
	if c == "" {
		c = "unknown"
	}
	return fmt.Sprintf("agentiscaled %s (commit %s, %s/%s)", version, c, goruntime.GOOS, goruntime.GOARCH)
}
