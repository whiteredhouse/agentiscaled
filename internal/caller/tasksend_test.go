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

func TestAnswerCommand(t *testing.T) {
	o := taskSendOptions{dataDir: nodestate.DefaultDataDir()}
	if got, want := answerCommand(o, Target{Service: "helper"}, "a-1"), `agentiscaled task send -task a-1 helper -- "<your answer>"`; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	o.dataDir = "/tmp/my node"
	got := answerCommand(o, Target{PeerID: "12D3Koo", Service: "helper"}, "a-1")
	if want := `agentiscaled task send -data-dir '/tmp/my node' -task a-1 -peer 12D3Koo helper -- "<your answer>"`; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
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
	err := RunTaskSend(context.Background(), []string{"-data-dir", t.TempDir(), "-task", "$(x)", "helper", "21"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not a task ID") {
		t.Fatalf("-task $(x): %v", err)
	}
}

// The whole command, run by bash, reaches task send's parser as the task
// ID, the agent and the answer.
func TestAnswerCommandRoundTrips(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	cmd := answerCommand(taskSendOptions{dataDir: "/tmp/it's here"}, Target{Service: "helper"}, "a-1")
	cmd = strings.Replace(cmd, `"<your answer>"`, `-42`, 1)
	out, err := exec.Command(bash, "-c", `agentiscaled() { printf '%s\0' "$@"; }; `+cmd).Output()
	if err != nil {
		t.Fatalf("bash ran %s: %v", cmd, err)
	}
	args := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	task, dir := fs.String("task", "", ""), fs.String("data-dir", "", "")
	pos, err := cli.ParseInterleaved(fs, args[2:])
	if err != nil || *task != "a-1" || *dir != "/tmp/it's here" || !slices.Equal(pos, []string{"helper", "-42"}) {
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
