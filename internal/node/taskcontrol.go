package node

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// RunTaskControl reads or changes the node-wide stop marker.
func RunTaskControl(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("task", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "Data directory of the node")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	path := stopFilePath(*dataDir)
	switch args[0] {
	case "stop":
		if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Fprintln(out, "task execution stopped: the running task is cancelled and new tasks are refused")
	case "resume":
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fmt.Fprintln(out, "task execution resumed")
	case "status":
		if b, err := os.ReadFile(path); err == nil {
			fmt.Fprintf(out, "stopped since %s", b)
		} else {
			fmt.Fprintln(out, "running")
		}
	default:
		return fmt.Errorf("unknown task command %q: use send, wait, cancel, stop, resume or status", args[0])
	}
	return nil
}
