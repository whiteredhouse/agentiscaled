package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/whiteredhouse/agentiscaled/internal/caller"
	"github.com/whiteredhouse/agentiscaled/internal/node"
)

func runTasks(ctx context.Context, args []string, stdin io.Reader, out, errOut io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: agentiscaled task send|wait|cancel")
	}
	switch args[0] {
	case "send":
		return caller.RunTaskSend(ctx, args[1:], stdin, out, errOut)
	case "wait":
		return caller.RunTaskWait(ctx, args[1:], out, errOut)
	case "cancel":
		return caller.RunTaskCancel(ctx, args[1:], out, errOut)
	case "stop", "resume", "status":
		return node.RunLegacyTaskControl(args, out, errOut)
	}
	return fmt.Errorf("unknown task command %q: use send, wait or cancel", args[0])
}
