package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestACPFinalMessage(t *testing.T) {
	const (
		first = `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Checking."}}}`
		tool  = `{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"t","kind":"read"}}`
		part1 = `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"{\"status\":\"needs_input\","}}}`
		part2 = `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"\"text\":\"Which branch?\"}"}}}`
	)
	named := func(params, id string) string {
		return strings.Replace(params, `"content":`, `"messageId":"`+id+`","content":`, 1)
	}
	for _, tc := range []struct {
		name    string
		updates []string
	}{
		{"consecutive named messages", []string{named(first, "one"), named(part1, "two"), named(part2, "two")}},
		{"same message across tool", []string{named(first, "one"), named(part1, "two"), tool, named(part2, "two")}},
		{"interleaved named messages", []string{named(part1, "answer"), named(first, "aside"), named(part2, "answer")}},
		{"empty ID is distinct from missing ID", []string{named(part1, ""), first, named(part2, "")}},
		{"unnamed message following named commentary", []string{named(first, "one"), part1, part2}},
		{"named message following unnamed commentary", []string{first, named(part1, "two"), named(part2, "two")}},
		{"unnamed messages separated by tool", []string{first, tool, part1, part2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan acpResponse, 1)
			s := &acpSession{pending: map[int64]chan acpResponse{1: ch}, done: make(chan struct{})}
			s.begin("s")
			s.promptID = 1
			var wire strings.Builder
			notify := func(params string) {
				fmt.Fprintf(&wire, "{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":%s}\n", params)
			}
			for _, params := range tc.updates {
				notify(params)
			}
			wire.WriteString("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"stopReason\":\"end_turn\"}}\n")
			notify(first) // This notification is outside the completed turn.
			s.read(strings.NewReader(wire.String()))
			if s.messages.named != nil {
				t.Fatal("a completed prompt retained its named-message history")
			}
			var result struct{ StopReason string }
			if err := s.await(context.Background(), "session/prompt", ch, &result); err != nil || result.StopReason != "end_turn" {
				t.Fatalf("final response followed by EOF: %+v, %v", result, err)
			}
			text, err := s.result(nil)
			if err != nil {
				t.Fatal(err)
			}
			answer, err := parseTurn(text, "s")
			if err != nil || !answer.Question || answer.Text != "Which branch?" {
				t.Fatalf("final message parsed as %+v, %v", answer, err)
			}
		})
	}
}

func TestACPNegotiatesResponseText(t *testing.T) {
	for _, negotiated := range []bool{false, true} {
		t.Run(fmt.Sprint(negotiated), func(t *testing.T) {
			script := fmt.Sprintf(`#!/bin/sh
IFS= read -r initialize || exit 1
case "$initialize" in *'"_meta":{'*'"agentiscale":{'*'"resultText":true'*) ;; *) exit 1 ;; esac
echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"_meta":{"agentiscale":{"status":true,"resultText":%t}}}}'
IFS= read -r request || exit 0
echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s"}}'
IFS= read -r request || exit 1
echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"streamed"}}}}'
echo '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn","_meta":{"agentiscale":{"status":"done","text":"explicit"}}}}'
`, negotiated)
			program := filepath.Join(t.TempDir(), "agent")
			if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			r, err := newACPRuntime(t.Context(), program, nil, t.TempDir(), ownerPolicy)
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.Run(context.Background(), t.TempDir(), taskRequest{Text: "result"})
			want := "streamed"
			if negotiated {
				want = "explicit"
			}
			if err != nil || got.Text != want {
				t.Fatalf("got %+v, %v; want %q", got, err, want)
			}
		})
	}
}

func TestACPResponseTextLimits(t *testing.T) {
	s := &acpSession{}
	s.messages.append(nil, "streamed")
	for _, tc := range []struct{ wire, want string }{
		{"", "streamed"},
		{"null", "streamed"},
		{`""`, ""},
	} {
		got, err := s.result(json.RawMessage(tc.wire))
		if err != nil || got != tc.want {
			t.Errorf("text %q: got %q, %v; want %q", tc.wire, got, err, tc.want)
		}
	}
	// Trimming whitespace must not allow an oversized response.
	if _, err := s.result(json.RawMessage(`"` + strings.Repeat(" ", acpMaxResult+1) + `"`)); err == nil {
		t.Error("accepted oversized response text")
	}
	s.messages.append(nil, strings.Repeat("a", acpMaxResult))
	if got, err := s.result(json.RawMessage(`"ok"`)); err != nil || got != "ok" {
		t.Fatalf("negotiated final text after oversized commentary: %q, %v", got, err)
	}
	for i := range acpMaxMessageIDs + 1 {
		id := fmt.Sprint(i)
		s.messages.append(&id, "")
	}
	if _, err := s.result(nil); err == nil {
		t.Fatal("incomplete streamed history accepted as a final message")
	}
	if got, err := s.result(json.RawMessage(`"final"`)); err != nil || got != "final" {
		t.Fatalf("explicit result after streamed history exceeded the ID limit: %q, %v", got, err)
	}
	s.begin("next")
	part := strings.Repeat("x", acpMaxResult)
	for i := range acpMaxNamedBytes / acpMaxResult {
		id := fmt.Sprint(i)
		s.messages.append(&id, part)
	}
	if _, err := s.result(nil); err == nil || s.messages.named != nil {
		t.Fatal("retained streamed history after text and IDs exceeded the byte limit")
	}
}
