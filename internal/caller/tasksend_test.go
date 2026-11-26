package caller

import (
	"context"
	"errors"
	"flag"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/whiteredhouse/agentiscaled/internal/cli"
	"github.com/whiteredhouse/agentiscaled/internal/nodestate"
)

func TestTerminalSafe(t *testing.T) {
	for in, want := range map[string]string{
		"plain\ttext\nnext":              "plain\ttext\nnext",
		"\x1b[2Jclear\x1b[31mred\x1b[0m": "clearred",
		"\x1b]0;title\x07after":          "after",
		"\x1b]8;;http://x\x1b\\link":     "link",
		"bell\x07\rcr\x00nul":            "bellcrnul",
	} {
		if got := terminalSafe(in); got != want {
			t.Errorf("terminalSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

// testPeer is a peer ID a task reference can carry.
const testPeer = "12D3KooWJZwrF8GWoShiLjjAbSN1oRJMTBS8rbELAk2GuU5XcJ6F"

func TestAnswerCommand(t *testing.T) {
	o := taskSendOptions{dataDir: nodestate.DefaultDataDir()}
	ref := TaskRef{Target{PeerID: testPeer, Service: "helper"}, "a-1"}
	if got, want := answerCommand(o, ref), `agentiscaled task send -task `+testPeer+`/helper/a-1 -- "<your answer>"`; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	o.dataDir = "/tmp/my node"
	if got, want := answerCommand(o, ref), `agentiscaled task send -data-dir '/tmp/my node' -task `+testPeer+`/helper/a-1 -- "<your answer>"`; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if got, want := waitCommand(o, ref), `agentiscaled task wait -data-dir '/tmp/my node' `+testPeer+`/helper/a-1`; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// A task's reference names its node, its agent and its ID, each in the
// form the commands take, and reads back as it was written.
func TestTaskRefs(t *testing.T) {
	ref := TaskRef{Target{PeerID: testPeer, Service: "helper.v2"}, "3f2b6c1e-9d4a-4c1b-8f0e-2a7d5b9c1e33"}
	if got, err := ParseTaskRef(ref.String()); err != nil || got != ref {
		t.Fatalf("%s read back as %+v, %v", ref, got, err)
	}
	for _, bad := range []string{"", "a-1", "helper/a-1", testPeer + "/helper", "not-a-peer/helper/a-1", testPeer + "/bad name/a-1", testPeer + "/helper/$(x)", testPeer + "/helper/a-1/x", testPeer + "//a-1"} {
		if _, err := ParseTaskRef(bad); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
}

// Only plain task IDs, such as the UUIDs agentiscaled agents use, are taken.
func TestTaskIDs(t *testing.T) {
	for _, id := range []string{"3f2b6c1e-9d4a-4c1b-8f0e-2a7d5b9c1e33", "a-1", "t.2:x_y"} {
		if !taskID.MatchString(id) {
			t.Errorf("%q refused", id)
		}
	}
	for _, id := range []string{"", "--", "-json", "a b", "$(x)", "t\x1b[2J", "a\x00b", "a\n", "it's", strings.Repeat("a", 129)} {
		if taskID.MatchString(id) {
			t.Errorf("%q taken", id)
		}
	}
	err := RunTaskSend(context.Background(), []string{"-data-dir", t.TempDir(), "-task", testPeer + "/helper/$(x)", "21"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not a task ID") {
		t.Fatalf("-task $(x): %v", err)
	}
	// The reference names the agent: an agent beside it is a mistake.
	err = RunTaskSend(context.Background(), []string{"-data-dir", t.TempDir(), "-task", testPeer + "/helper/a-1", "helper", "21"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "takes only the answer") {
		t.Fatalf("-task with an agent: %v", err)
	}
}

// The whole command, run by bash, reaches task send's parser as the task
// ID, the agent and the answer.
func TestAnswerCommandRoundTrips(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	ref := TaskRef{Target{PeerID: testPeer, Service: "helper"}, "a-1"}
	cmd := answerCommand(taskSendOptions{dataDir: "/tmp/it's here"}, ref)
	cmd = strings.Replace(cmd, `"<your answer>"`, `-42`, 1)
	out, err := exec.Command(bash, "-c", `agentiscaled() { printf '%s\0' "$@"; }; `+cmd).Output()
	if err != nil {
		t.Fatalf("bash ran %s: %v", cmd, err)
	}
	args := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	task, dir := fs.String("task", "", ""), fs.String("data-dir", "", "")
	pos, err := cli.ParseInterleaved(fs, args[2:])
	if err != nil || *task != ref.String() || *dir != "/tmp/it's here" || !slices.Equal(pos, []string{"-42"}) {
		t.Fatalf("task %q, dir %q, args %q, %v", *task, *dir, pos, err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestReportTheAnswer(t *testing.T) {
	task := Task{TaskID: "t", State: "completed", Terminal: "completed", Result: "\x1b[31mred\x1b[0m done\n"}
	var b strings.Builder
	if err := report(&b, taskSendOptions{}, task, nil); err != nil || b.String() != "\x1b[31mred\x1b[0m done\n" {
		t.Fatalf("to a pipe: %q %v", b.String(), err)
	}
	b.Reset()
	if err := report(&b, taskSendOptions{terminal: true}, task, nil); err != nil || b.String() != "red done\n" {
		t.Fatalf("to a terminal: %q %v", b.String(), err)
	}
	for _, o := range []taskSendOptions{{}, {json: true}} {
		if err := report(failingWriter{}, o, task, nil); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("json=%t: write error lost: %v", o.json, err)
		}
	}
	failed := errors.New("task t ended failed")
	if err := report(failingWriter{}, taskSendOptions{json: true}, task, failed); err != failed {
		t.Errorf("the task's own error should win: %v", err)
	}
}
