package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestDeliveryLogRecoversFinalRecord(t *testing.T) {
	first, _ := json.Marshal(deliveryRecord{testSender, "before-crash", "task-before"})
	tail, _ := json.Marshal(deliveryRecord{testSender, "complete-tail", "task-tail"})
	for _, tc := range []struct {
		name                string
		tail                []byte
		keepTail, wantError bool
	}{
		{"partial final record", []byte(`{"sender":`), false, false},
		{"complete final record without newline", tail, true, false},
		{"normal complete record", append(append([]byte{}, tail...), '\n'), true, false},
		{"corrupt complete record", []byte(`{"sender":` + string(tail) + "\n"), false, true},
		{"missing identity fields", []byte("{}\n"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "deliveries.jsonl")
			original := append(append(append([]byte{}, first...), '\n'), tc.tail...)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			deliveries, err := loadDeliveries(path)
			if tc.wantError {
				if err == nil {
					t.Fatal("untrustworthy completed history was accepted")
				}
				after, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(after, original) {
					t.Fatal("corrupt completed history was modified")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if id, ok := deliveries.first(testSender, "before-crash"); !ok || id != "task-before" {
				t.Fatal("valid history prefix was lost")
			}
			if id, ok := deliveries.first(testSender, "complete-tail"); ok != tc.keepTail || ok && id != "task-tail" {
				t.Fatalf("tail recovery: %s %v", id, ok)
			}
			if err := deliveries.record(testSender, "after-recovery", "task-after"); err != nil {
				t.Fatal(err)
			}
			restarted, err := loadDeliveries(path)
			if err != nil {
				t.Fatal(err)
			}
			if id, ok := restarted.first(testSender, "after-recovery"); !ok || id != "task-after" {
				t.Fatal("new delivery lost on the next restart")
			}
			if id, ok := restarted.first(testSender, "before-crash"); !ok || id != "task-before" {
				t.Fatal("old delivery lost after restart")
			}
			if id, ok := restarted.first(testSender, "complete-tail"); ok != tc.keepTail || ok && id != "task-tail" {
				t.Fatal("completed tail did not survive restart")
			}
		})
	}
}

func TestARequestAfterTornLogIsNotRunTwice(t *testing.T) {
	dir := t.TempDir()
	initial := newAgentHarnessIn(t, testSender, dir)
	if err := os.WriteFile(initial.svc.delivered.path, []byte(`{"sender":`), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered := newAgentHarnessIn(t, testSender, dir)
	first := recovered.send(t, "after-torn-tail", "one action", nil)
	<-recovered.fix.prompts
	recovered.fix.release <- "one result"
	recovered.await(t, first.ID, a2a.TaskStateCompleted)
	waitIdle(t, recovered.svc)
	restarted := newAgentHarnessIn(t, testSender, dir)
	retry := restarted.send(t, "after-torn-tail", "one action", nil)
	restarted.fix.release <- "incorrect duplicate result"
	done := restarted.await(t, retry.ID, a2a.TaskStateRejected)
	if done.Metadata[metaDuplicateOf] != string(first.ID) {
		t.Fatalf("lost original delivery: %v", done.Metadata)
	}
}
