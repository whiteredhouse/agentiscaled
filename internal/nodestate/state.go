// Package nodestate describes files shared by the running node and its clients.
package nodestate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultDataDir locates this user's node data.
func DefaultDataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ".agentiscale"
	}
	return filepath.Join(dir, "agentiscale")
}

// RuntimeConfigDir resolves the configured or default owner runtime directory.
func RuntimeConfigDir(variable, defaultDir string) (string, error) {
	if dir := os.Getenv(variable); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, defaultDir), nil
}

const HaltFile = "node-halted.json"

// Halt records why the node stopped retrying its network.
type Halt struct {
	Reason  string    `json:"reason"`
	Network string    `json:"network,omitempty"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// LoadHalt returns the persisted reason or nil when the node can run.
func LoadHalt(dataDir string) (*Halt, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, HaltFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var h Halt
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("%s: %w", HaltFile, err)
	}
	return &h, nil
}

// CheckHalt returns the node's own diagnostic before a client tries its socket.
func CheckHalt(dir string) error {
	h, err := LoadHalt(dir)
	if err != nil {
		return err
	}
	if h != nil {
		return errors.New(h.Message)
	}
	return nil
}
