package main

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"text/tabwriter"
)

const runtimesUsage = `usage: agentiscaled runtimes list

List supported runtimes and look up their default commands on PATH.
CONFIG DIR is the user config directory selected by the current shell.
`

func runRuntimes(args []string, out io.Writer) error {
	list := len(args) > 0 && args[0] == "list"
	if list {
		args = args[1:]
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		_, err := fmt.Fprint(out, runtimesUsage)
		return err
	}
	if !list || len(args) != 0 {
		return errors.New(runtimesUsage)
	}

	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "RUNTIME\tCOMMAND\tEXECUTABLE\tCONFIG DIR")
	for _, name := range runtimeNames() {
		spec := runtimes[name]
		executable, err := exec.LookPath(spec.defaultCommand)
		if err != nil {
			executable = "not found on PATH"
		}
		configDir, err := spec.ownerConfigDir()
		if err != nil {
			configDir = "unavailable"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", name, spec.defaultCommand, executable, homeRelative(configDir))
	}
	return table.Flush()
}
