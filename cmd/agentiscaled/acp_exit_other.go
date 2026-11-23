//go:build !linux && !darwin

package main

// On other platforms close gives the agent the full grace period before reaping.
func waitACPExit(int) bool { return false }
