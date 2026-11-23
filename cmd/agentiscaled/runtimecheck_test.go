package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRuntimeCheckObservesAnswersAndCancellation(t *testing.T) {
	// Each fixture is a fresh test process, including under the race detector.
	t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	for _, tc := range []struct {
		mode, want string
		pass       bool
	}{
		{"slow-start", "PASS  cancel", true},
		{"quick-result", "WARN  cancel: the turn completed before it was cancelled", true},
		{"prompt-error", "FAIL  cancel: fake-acp failed: session/prompt answered with error -32603", false},
		{"timeout", "FAIL  cancel: no answer within 2s (-timeout)", false},
		{"interrupted", "PASS  answer:", false},
		{"lost-history", "FAIL  answer: the result did not include", false},
		{"empty-answer", "FAIL  answer: the result did not include", false},
		{"exit-after-question", "FAIL  answer: the process that asked did not take the answer", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			old := checkCancelAfter
			checkCancelAfter = 100 * time.Millisecond
			t.Cleanup(func() { checkCancelAfter = old })
			if tc.mode == "quick-result" || tc.mode == "prompt-error" || tc.mode == "timeout" {
				checkCancelAfter = 5 * time.Second
			}

			dir := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var out bytes.Buffer
			report := writerFunc(func(p []byte) (int, error) {
				if tc.mode == "interrupted" && bytes.Contains(p, []byte("PASS  answer:")) {
					cancel()
				}
				if tc.mode == "exit-after-question" && bytes.Contains(p, []byte("PASS  question")) {
					// Let the normal answer path notice that the process exited and
					// successfully load the session before the checker evaluates it.
					pid, err := strconv.Atoi(readRecord(t, filepath.Join(dir, "asking.pid")))
					if err != nil {
						t.Fatal(err)
					}
					waitFor(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
				}
				return out.Write(p)
			})
			command := fakeACPCommand(t, "-status", "-load", "-check-mode", tc.mode)
			args := []string{"-command", command, "-dir", dir, "-data-dir", t.TempDir()}
			if tc.mode == "timeout" {
				args = append(args, "-timeout=2s")
			}
			err := runRuntimeCheck(ctx, args, report)
			if tc.mode == "interrupted" && !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted check returned %v", err)
			}
			if (err == nil) != tc.pass || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("runtime check: %v\n%s", err, &out)
			}
			if tc.mode == "exit-after-question" && !strings.Contains(out.String(), "PASS  load:") {
				t.Fatalf("the same agent should pass the separate session/load check:\n%s", &out)
			}
		})
	}
}
