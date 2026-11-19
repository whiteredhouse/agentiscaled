package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// The prompts of runtime check's turns.
const (
	checkDoneTask     = "Reply with the single word ready. Do not use any tools."
	checkQuestionTask = "Before anything else, ask the sender which color they like, as your one question. Do not use any tools."
	checkAnswer       = "Blue. Reply with the color the sender likes, in one word. Do not use any tools."
	checkLongTask     = "Count from 1 to 100000, one number per line. Do not stop until you are done."
)

// checkCancelAfter is how long the cancelled turn runs before it is
// cancelled.
var checkCancelAfter = 3 * time.Second

func runRuntimeCheck(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("runtime check", flag.ContinueOnError)
	command := fs.String("command", "", "Command line of the agent, which speaks ACP on stdio (required)")
	dir := fs.String("dir", "", "Directory the agent works in (default: a new empty one)")
	dataDir := fs.String("data-dir", "", "Directory for the agent's own configuration with -owner-config=false (default: a new empty one)")
	var policy agentPolicy
	fs.BoolVar(&policy.DirInstructions, "dir-instructions", true, "=false checks the agent with the directory's instruction files turned off")
	fs.BoolVar(&policy.DirSkills, "dir-skills", true, "=false checks the agent with the directory's skills turned off")
	fs.BoolVar(&policy.OwnerConfig, "owner-config", true, "=false checks the agent with a configuration of its own")
	timeout := fs.Duration("timeout", 5*time.Minute, "How long each turn may take")
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 || strings.TrimSpace(*command) == "" {
		return errors.New(`usage: agentiscaled runtime check -command "my-agent acp" [-dir DIR] [-owner-config=false] [-dir-instructions=false] [-dir-skills=false]`)
	}
	temp := func(p *string, pattern string) error {
		if *p != "" {
			return nil
		}
		d, err := os.MkdirTemp("", pattern)
		if err != nil {
			return err
		}
		*p = d
		return nil
	}
	if err := temp(dir, "agentiscale-check-dir-"); err != nil {
		return err
	}
	if err := temp(dataDir, "agentiscale-check-data-"); err != nil {
		return err
	}

	failed := 0
	report := func(result, step, detail string) {
		if result == "FAIL" {
			failed++
		}
		if detail != "" {
			step += ": " + detail
		}
		fmt.Fprintf(out, "%-4s  %s\n", result, step)
	}

	program, programArgs, err := splitACPCommand(*command)
	if err != nil {
		return err
	}
	rt, err := newACPRuntime(program, programArgs, *dataDir, policy)
	if err != nil {
		report("FAIL", "initialize", err.Error())
		return errors.New("the agent does not keep the runtime contract")
	}
	r := rt.(*acpRuntime)
	c := r.info.conventions()
	report("PASS", "initialize", fmt.Sprintf("%s speaks ACP version %d", r.info.name(r.command), r.info.ProtocolVersion))
	if r.info.Meta.Agentiscale == nil {
		report("WARN", "conventions", "no _meta.agentiscale: the agent declares no policies, and its turns end in the JSON answer the prompt asks for")
	} else {
		report("PASS", "conventions", fmt.Sprintf("contract %d, policies [%s], status %v", c.Contract, strings.Join(c.Policies, " "), c.Status))
	}
	if r.info.AgentCapabilities.MCPCapabilities.HTTP {
		report("PASS", "network", "takes HTTP MCP servers, so it can call the network's other agents")
	} else {
		report("WARN", "network", "no mcpCapabilities.http: the agent works without the network's other agents")
	}

	// Each turn has a deadline, so an agent that never answers fails its
	// step rather than hanging the check.
	turn := func(ctx context.Context, text, session string) (turnResult, int, error) {
		steps := 0
		tctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		res, err := r.Run(tctx, *dir, taskRequest{Sender: "runtime-check", Text: text, Session: session, Progress: func(string) { steps++ }})
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = fmt.Errorf("no answer within %s (-timeout)", *timeout)
		}
		return res, steps, err
	}
	res, steps, err := turn(ctx, checkDoneTask, "")
	switch {
	case err != nil:
		report("FAIL", "turn", err.Error())
	case res.Question:
		report("FAIL", "turn", "the agent asked a question instead of answering: "+progressLine(res.Text))
	case res.Text == "":
		report("FAIL", "turn", "the agent's result is empty")
	default:
		report("PASS", "turn", fmt.Sprintf("result %q, %d steps", progressLine(res.Text), steps))
	}

	if !r.info.AgentCapabilities.LoadSession {
		report("SKIP", "question", "no loadSession: the agent cannot ask questions")
	} else if res, _, err := turn(ctx, checkQuestionTask, ""); err != nil {
		report("FAIL", "question", err.Error())
	} else if !res.Question {
		report("FAIL", "question", "the turn ended in a result, not needs_input: "+progressLine(res.Text))
	} else {
		report("PASS", "question", progressLine(res.Text))
		if ans, _, err := turn(ctx, checkAnswer, res.Session); err != nil {
			report("FAIL", "answer", err.Error())
		} else if ans.Question {
			report("FAIL", "answer", "the agent asked again: "+progressLine(ans.Text))
		} else {
			report("PASS", "answer", fmt.Sprintf("session/load resumed the session; result %q", progressLine(ans.Text)))
		}
	}

	var stop string
	r.stopped = func(reason string) { stop = reason }
	cctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(checkCancelAfter, cancel)
	start := time.Now()
	_, _, err = turn(cctx, checkLongTask, "")
	timer.Stop()
	cancel()
	took := time.Since(start) - checkCancelAfter
	switch {
	case !errors.Is(err, context.Canceled):
		report("WARN", "cancel", "the turn ended before it was cancelled; cancellation was not checked")
	case stop == "cancelled":
		report("PASS", "cancel", fmt.Sprintf("ended with stopReason cancelled in %s", took.Round(10*time.Millisecond)))
	case stop != "":
		report("FAIL", "cancel", fmt.Sprintf("ended with stopReason %q, not cancelled", stop))
	default:
		report("FAIL", "cancel", fmt.Sprintf("did not end the turn within %s of session/cancel; the node stopped its processes", acpCancelGrace))
	}

	if failed > 0 {
		return fmt.Errorf("%d checks failed", failed)
	}
	return nil
}
