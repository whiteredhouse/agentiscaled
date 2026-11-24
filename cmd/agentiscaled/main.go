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

Commands:
  up       sign in, join this machine to your network and keep the node running
  client   connect agent clients (Claude Desktop, Codex CLI) to the node, or disconnect them
  agent    offer an agent from this machine to your network (add, remove, list)
  runtime  list supported agent runtimes and their local configuration, or check
           an agent's support of the agentiscale runtime contract (check)
  down     stop the node's background service; its identity stays
  status   show the node's service, network and mesh connection
  version  print the version
  reset    drop the node's identity so 'up' joins it again as a new node; its settings stay
  run      run the node in the foreground with the settings 'up' saved
  mcp      serve the node's MCP tools on stdio, for agent clients
  task     send a task to an agent and print its answer (send), wait for a task
           to end (wait) or cancel it (cancel), or control task execution on
           this node (status, stop, resume)
  skill    print the agentiscale skill, which teaches agents to call other agents
           of your network, or install it for Claude Code and Codex (install)

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
