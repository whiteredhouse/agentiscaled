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
// deletes the signed-in account with the sign-in's token.
func TestAccountDelete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	idp := newFakeIdP(t, "")
	var deletes atomic.Int32
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/me" || r.Header.Get("Authorization") != "Bearer ACCESS" {
			http.NotFound(w, r)
			return
		}
		deletes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"deleting","network_ids":["11111111-2222-4333-8444-555555555555"]}`))
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
	if deletes.Load() != 1 || !strings.Contains(out.String(), "Deleted network 11111111-2222-4333-8444-555555555555") {
		t.Fatalf("deletes %d, output:\n%s", deletes.Load(), out.String())
	}
}

func TestDeleteAccountAnswers(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusNotFound:            "no account to delete",
		http.StatusInternalServerError: "internal error",
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
