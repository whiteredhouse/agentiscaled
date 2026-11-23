// Command agentiscaled is the agentiscale node. It embeds the
// Google Agent Mesh node from agentiscale's fork (whiteredhouse/agentmesh)
// instead of running the agentmesh-node binary.
//
// `agentiscaled up` signs the owner in through the browser, joins the node
// to their network and keeps it running as a per-user service;
// started without a command, agentiscaled runs the node in the foreground,
// joining with a join code or a bootstrap token.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/whiteredhouse/agentiscaled/internal/caller"
	"github.com/whiteredhouse/agentiscaled/internal/node"
)

const usage = `agentiscaled is the agentiscale node.

Use the network:
  task     send a task to an agent, wait for it to finish, or cancel it
           (send, wait, cancel)
  mcp      serve the node's MCP tools on stdio for agent clients
  skill    print or install the skill for finding and calling agents

Run this node:
  up       sign in, join this machine to your network and keep the node running
  down     stop the background service and keep the node's identity
  status   show the node's service, network and mesh connection
  agent    offer and manage local agents (add, remove, list) and control task
           execution for all agents on this node (stop, resume, status)
  runtime  list runtimes and check an agent's runtime support
  client   connect or disconnect agent clients such as Claude Desktop and Codex CLI
  run      run the node in the foreground with its saved settings
  reset    drop the node's identity and keep its settings
  version  print the version

Without a command, agentiscaled runs the node in the foreground with the
flags given (see agentiscaled -h).
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	node.ReleaseVersion = version
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		var err error
		switch cmd, args := os.Args[1], os.Args[2:]; cmd {
		case "mcp":
			// Standard output carries MCP messages; errors go to standard error.
			err = caller.RunMCP(ctx, args)
		case "task":
			err = runTasks(ctx, args, os.Stdin, os.Stdout, os.Stderr)
		case "up":
			err = node.RunUp(ctx, args, os.Stdout)
		case "client":
			err = node.RunClients(ctx, args, os.Stdout)
		case "agent":
			err = node.RunAgent(ctx, args, os.Stdout)
		case "runtime":
			err = node.RunRuntimes(ctx, args, os.Stdout)
		case "skill":
			err = caller.RunSkill(args, os.Stdout, os.Stderr)
		case "down":
			err = node.RunDown(args, os.Stdout)
		case "reset":
			err = node.RunReset(ctx, args, os.Stdout)
		case "status":
			err = node.RunStatus(ctx, args, os.Stdout)
		case "version":
			fmt.Println(versionString())
			return
		case "run":
			err = node.RunSaved(ctx, args)
		case "help":
			fmt.Print(usage)
		default:
			fmt.Fprint(os.Stderr, usage)
			err = fmt.Errorf("unknown command %q", cmd)
		}
		if err != nil {
			// task send and task wait say with the exit status how the
			// task ended.
			log.Print(err)
			os.Exit(caller.ExitCode(err))
		}
		return
	}
	if err := node.RunForeground(ctx); err != nil {
		log.Fatal(err)
	}
}
