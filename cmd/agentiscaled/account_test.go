package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// account delete asks for the typed confirmation before it signs in, then
// deletes the signed-in account with the sign-in's token and says until
// when it can be restored; account restore undoes it.
func TestAccountDelete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	idp := newFakeIdP(t, "")
	var deletes, restores atomic.Int32
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ACCESS" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/me":
			deletes.Add(1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"scheduled","network_ids":["11111111-2222-4333-8444-555555555555"],` +
				`"requested_at":"2030-01-15T12:00:00Z","purge_after":"2030-02-14T12:00:00Z"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/me/restore":
			restores.Add(1)
			_, _ = w.Write([]byte(`{"user":{"id":"u"},"network":{"id":"11111111-2222-4333-8444-555555555555","name":"Alice's network"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer control.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"delete", "-data-dir", t.TempDir(), "-control", control.URL,
		"-auth-issuer", idp.config().issuer, "-auth-client-id", "CLIENT", "-auth-audience", "https://control.test"}

	var out bytes.Buffer
	if err := runAccount(ctx, args, strings.NewReader("no\n"), &out); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("unconfirmed: %v", err)
	}
	idp.mu.Lock()
	signIns := len(idp.forms)
	idp.mu.Unlock()
	if deletes.Load() != 0 || signIns != 0 {
		t.Fatalf("unconfirmed delete signed in (%d) or deleted (%d)", signIns, deletes.Load())
	}

	out.Reset()
	if err := runAccount(ctx, args, strings.NewReader("Delete\n"), &out); err != nil {
		t.Fatalf("delete: %v\n%s", err, out.String())
	}
	if deletes.Load() != 1 || !strings.Contains(out.String(), "deleted for good on 14 February 2030") ||
		!strings.Contains(out.String(), "agentiscaled account restore") {
		t.Fatalf("deletes %d, output:\n%s", deletes.Load(), out.String())
	}

	out.Reset()
	restore := append([]string{"restore"}, args[1:]...)
	if err := runAccount(ctx, restore, strings.NewReader(""), &out); err != nil {
		t.Fatalf("restore: %v\n%s", err, out.String())
	}
	if restores.Load() != 1 || !strings.Contains(out.String(), "Restored your account and Alice's network") ||
		!strings.Contains(out.String(), "agentiscaled reset") {
		t.Fatalf("restores %d, output:\n%s", restores.Load(), out.String())
	}
}

func TestRestoreAccountAnswers(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusConflict:            "not scheduled for deletion",
		http.StatusNotFound:            "only for 30 days",
		http.StatusInternalServerError: "internal error",
	} {
		control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"internal error"}`))
		}))
		_, err := restoreAccount(context.Background(), control.URL, "T")
		control.Close()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("status %d: %v, want %q", status, err, want)
		}
	}
}

func TestDeleteAccountAnswers(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusNotFound:            "no account to delete",
		http.StatusInternalServerError: "internal error",
		// An older control service's answer, with no date to restore by.
		http.StatusAccepted: "202 Accepted",
	} {
		control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"internal error"}`))
		}))
		_, err := deleteAccount(context.Background(), control.URL, "T")
		control.Close()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("status %d: %v, want %q", status, err, want)
		}
	}
	if err := runAccount(context.Background(), nil, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Error("no subcommand accepted")
	}
}
