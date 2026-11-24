package node

import (
	"fmt"
	"io"
)

// RunLegacyTaskControl accepts the previous spelling of the node-wide control.
func RunLegacyTaskControl(args []string, out, errOut io.Writer) error {
	fmt.Fprintf(errOut, "agentiscaled task %s is deprecated; use 'agentiscaled agent %s' instead.\n", args[0], args[0])
	return runAgentControl("task", args, out, errOut)
}
