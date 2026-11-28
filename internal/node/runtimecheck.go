package node

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/whiteredhouse/agentiscaled/internal/checkprompt"
)

// The prompts of runtime check's turns.
const (
	checkDoneTask     = checkprompt.Done
	checkQuestionTask = checkprompt.Question
	checkAnswer       = checkprompt.Answer
	checkLongTask     = checkprompt.Long
)

// checkCancelAfter is how long the cancelled turn runs before it is
// cancelled.
var checkCancelAfter = 3 * time.Second

func runRuntimeCheck(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("runtime check", flag.ContinueOnError)
	command := fs.String("command", "", "Command line of the agent, which speaks ACP on stdio (required)")
	name := fs.String("name", "", "Select the named agent's private configuration when checking with -owner-config=false and its -data-dir")
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
		return errors.New(`usage: agentiscaled runtime check -command "my-agent acp" [-dir DIR] [-name NAME] [-owner-config=false] [-dir-instructions=false] [-dir-skills=false]`)
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
	// Only an explicit directory contributes to an unnamed check's profile.
	checkDir := *dir
	generatedPrivateData := *dataDir == "" && !policy.OwnerConfig
	if err := temp(dir, "agentiscale-check-dir-"); err != nil {
		return err
	}
	if err := temp(dataDir, "agentiscale-check-data-"); err != nil {
		return err
	}
	// ACP takes only an absolute working directory.
	for _, p := range []*string{dir, dataDir} {
		abs, err := filepath.Abs(*p)
		if err != nil {
			return err
		}
		*p = abs
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
	if checkDir != "" {
		checkDir = *dir // absolute now
	}
	a := agentConfig{Name: *name, Command: program, Args: programArgs, Dir: *dir}
	a.setPolicy(policy)
	rt, err := newACPRuntime(ctx, a, *dataDir, checkDir)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		report("FAIL", "initialize", err.Error())
		return errors.New("the agent does not keep the runtime contract")
	}
	r := rt.(*acpRuntime)
	defer r.releaseAll()
	c := r.info.conventions()
	report("PASS", "initialize", fmt.Sprintf("%s speaks ACP version %d", r.info.name(r.command), r.info.ProtocolVersion))
	if r.info.Meta.Agentiscale == nil {
		report("WARN", "conventions", "no _meta.agentiscale: the agent declares no policies, and its turns end in the JSON answer the prompt asks for")
	} else {
		report("PASS", "conventions", fmt.Sprintf("contract %d, policies [%s], status %v", c.Contract, strings.Join(c.Policies, " "), c.Status))
	}
	if r.info.AgentCapabilities.MCPCapabilities.HTTP {
		report("INFO", "network", "advertises HTTP MCP servers, so turns get the network's other agents; this check does not connect one")
	} else {
		report("INFO", "network", "no mcpCapabilities.http: turns get the network's other agents as a stdio server (agentiscaled network-bridge); this check does not connect one")
	}

	// Each turn has a deadline, so an agent that never answers fails its
	// step rather than hanging the check.
	turn := func(ctx context.Context, task, text, session string) (turnResult, int, error) {
		var steps atomic.Int32
		tctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		res, err := r.Run(tctx, *dir, taskRequest{Sender: "runtime-check", Text: text, Session: session, Task: task, Progress: func(string) { steps.Add(1) }})
		var signIn *acpSignInRequired
		if errors.As(err, &signIn) {
			message := signIn.ownerMessage()
			if generatedPrivateData {
				message += "\nAfter sign-in, retry this check with -data-dir " + shellQuote(*dataDir)
			}
			err = errors.New(message)
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = fmt.Errorf("no answer within %s (-timeout)", *timeout)
		}
		return res, int(steps.Load()), err
	}
	res, steps, err := turn(ctx, "runtime-check-result", checkDoneTask, "")
	switch {
	case err != nil:
		report("FAIL", "turn", err.Error())
	case res.Question:
		report("FAIL", "turn", "the agent asked a question instead of answering: "+progressLine(res.Text))
	case !checkHasWords(res.Text, "ready"):
		report("FAIL", "turn", "the result did not contain the requested word ready: "+progressLine(res.Text))
	default:
		report("PASS", "turn", fmt.Sprintf("result %q, %d steps", progressLine(res.Text), steps))
	}

	// The question and answer supply different words so the result shows
	// that the agent carried the first prompt into the answering turn.
	checkResume := func(askStep, answerStep string, reload bool) {
		task := "runtime-check-" + answerStep
		var process *acpSession
		var loaded bool
		r.prompted = func(s *acpSession, fromLoad bool) { process, loaded = s, fromLoad }
		question, _, err := turn(ctx, task, checkQuestionTask, "")
		if err != nil {
			report("FAIL", askStep, err.Error())
			return
		}
		if !question.Question {
			report("FAIL", askStep, "the turn ended in a result, not needs_input: "+progressLine(question.Text))
			return
		}
		askingProcess := process
		if reload {
			r.release(task)
		} else {
			report("PASS", askStep, "the turn ended in needs_input")
		}
		process = nil
		answer, _, err := turn(ctx, task, checkAnswer, question.Session)
		switch {
		case err != nil:
			report("FAIL", answerStep, err.Error())
		case reload && (!loaded || process == askingProcess):
			report("FAIL", answerStep, "the answer did not use session/load in a new process")
		case !reload && (loaded || process != askingProcess):
			report("FAIL", answerStep, "the process that asked did not take the answer")
		case answer.Question:
			report("FAIL", answerStep, "the agent asked again: "+progressLine(answer.Text))
		case !checkHasWords(answer.Text, "lantern", "blue"):
			report("FAIL", answerStep, "the result did not include the word from the original task and the color from the answer: "+progressLine(answer.Text))
		default:
			how := "answered in the process that asked"
			if reload {
				how = "session/load resumed the session"
			}
			report("PASS", answerStep, fmt.Sprintf("%s; result %q", how, progressLine(answer.Text)))
		}
	}
	checkResume("question", "answer", false)
	if r.info.AgentCapabilities.LoadSession {
		checkResume("load", "load", true)
	} else {
		report("SKIP", "load", "no loadSession: an answer that comes after the agent's process ended fails the task")
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var timer *time.Timer
	var cancelAt, stoppedAt time.Time
	var stop string
	r.prompted = func(*acpSession, bool) {
		cancelAt = time.Now().Add(checkCancelAfter)
		timer = time.AfterFunc(checkCancelAfter, cancel)
	}
	r.stopped = func(reason string) {
		stop, stoppedAt = reason, time.Now()
		if timer != nil {
			timer.Stop()
		}
	}
	res, _, err = turn(cctx, "runtime-check-cancel", checkLongTask, "")
	if timer != nil {
		timer.Stop()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch {
	case err == nil && !res.Question && res.Text != "":
		report("WARN", "cancel", "the turn completed before it was cancelled; cancellation was not checked")
	case err == nil:
		report("FAIL", "cancel", "the long turn ended without a result before it was cancelled")
	case !errors.Is(err, context.Canceled) || cctx.Err() == nil:
		report("FAIL", "cancel", err.Error())
	case stop == "cancelled":
		report("PASS", "cancel", fmt.Sprintf("ended with stopReason cancelled in %s", stoppedAt.Sub(cancelAt).Round(10*time.Millisecond)))
	case stop != "":
		report("FAIL", "cancel", fmt.Sprintf("ended with stopReason %q, not cancelled", stop))
	default:
		report("FAIL", "cancel", "the agent did not acknowledge session/cancel with stopReason cancelled")
	}
	if failed > 0 {
		return fmt.Errorf("%d checks failed", failed)
	}
	return nil
}

// checkHasWords accepts the requested words in any order or surrounding prose.
func checkHasWords(text string, want ...string) bool {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for _, word := range want {
		if !slices.Contains(words, word) {
			return false
		}
	}
	return true
}
