package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/whiteredhouse/agentiscaled/internal/caller"
)

type drainMark = caller.Drain

// drainFileName holds an agent's drain in its state directory, so that the
// drain outlives a restart of the node.
const drainFileName = "drain.json"

func drainPath(dataDir, name string) string {
	return filepath.Join(agentStateDir(dataDir, name), drainFileName)
}

// drainFile is what a drain file holds: the owner's fields only. Whether
// the agent still has work is never saved; the node reads it from the
// agent's activity each time, so a drained field in the file is ignored.
type drainFile struct {
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	By     string    `json:"by,omitempty"`
}

// maxDrainFileBytes bounds what the node reads of a drain file; the agent
// command writes far less.
const maxDrainFileBytes = 4 << 10

// loadDrain reads an agent's drain, or nil when it has none. A drain file
// that cannot be read still drains the agent, without a reason: the owner
// meant the agent to take no new tasks. A field it may not carry is left
// out, so that a hand-edited file never keeps the node from registering.
func loadDrain(path string) *drainMark {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return &drainMark{}
	}
	defer f.Close()
	var saved drainFile
	b, err := io.ReadAll(io.LimitReader(f, maxDrainFileBytes+1))
	if err != nil || len(b) > maxDrainFileBytes || json.Unmarshal(b, &saved) != nil {
		return &drainMark{}
	}
	d := drainMark{Reason: saved.Reason, Since: saved.Since, By: saved.By}.Valid()
	return &d
}

// saveDrain writes an agent's drain where the node reads it.
func saveDrain(dataDir, name string, d drainMark) error {
	dir := agentStateDir(dataDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(drainFile{Reason: d.Reason, Since: d.Since, By: d.By})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".drain-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), drainPath(dataDir, name)); err != nil {
		return fmt.Errorf("save the drain: %w", err)
	}
	return nil
}

// removeDrain lifts an agent's drain; it reports whether there was one.
func removeDrain(dataDir, name string) (bool, error) {
	err := os.Remove(drainPath(dataDir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
