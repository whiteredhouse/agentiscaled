package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/whiteredhouse/agentiscaled/internal/caller"
)

type drainMark = caller.Drain

// drainFileName holds an agent's drain in its state directory, so that the
// drain outlives a restart of the node.
const drainFileName = "drain.json"

func drainPath(dataDir, name string) string {
	return filepath.Join(agentStateDir(dataDir, name), drainFileName)
}

// maxDrainFileBytes bounds what the node reads of a drain file; the agent
// command writes far less.
const maxDrainFileBytes = 4 << 10

// loadDrain reads an agent's drain, or nil when it has none. A drain file
// that cannot be read still drains the agent, without a reason: the owner
// meant the agent to take no new tasks.
func loadDrain(path string) *drainMark {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return &drainMark{}
	}
	defer f.Close()
	var d drainMark
	b, err := io.ReadAll(io.LimitReader(f, maxDrainFileBytes+1))
	if err != nil || len(b) > maxDrainFileBytes || json.Unmarshal(b, &d) != nil {
		return &drainMark{}
	}
	if caller.DrainReasonError(d.Reason) != "" {
		d.Reason = ""
	}
	return &d
}

// saveDrain writes an agent's drain where the node reads it.
func saveDrain(dataDir, name string, d drainMark) error {
	dir := agentStateDir(dataDir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(d)
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
