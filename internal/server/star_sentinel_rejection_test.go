package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1056: per-project tools (architecture / schema / trace / dead_code /
// changes / etc.) flow through resolveProjectID. Pre-fix passing
// project="*" landed in the bare "project '*' not found" error with
// next_steps pointing at `list` — but `list` doesn't show "*", so an
// agent who'd assumed `*` worked everywhere (the way it does on
// search + query) got pushed deeper into confusion. The clear-
// rejection error names "*" as the cross-project sentinel + names
// the two tools that actually accept it, so the caller can either
// pin scope or switch tools.

func TestResolveProjectID_StarSentinel_ClearRejection(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	_, err := srv.resolveProjectID("*")
	if err == nil {
		t.Fatal("expected rejection for project='*'")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cross-project sentinel") {
		t.Errorf("expected error naming the sentinel; got %q", msg)
	}
	if !strings.Contains(msg, "search") || !strings.Contains(msg, "query") {
		t.Errorf("expected error pointing at search + query as the cross-project tools; got %q", msg)
	}
	if strings.Contains(msg, "not found") {
		t.Errorf("'not found' framing was the pre-fix bug — should be replaced with the sentinel explanation; got %q", msg)
	}
}

// Architecture flows through mustProject → resolveProjectID; it
// inherits the new rejection text.
func TestHandleArchitecture_StarSentinel_RejectsWithClearText(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-arch-star"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	result, err := srv.handleArchitecture(context.Background(), makeReq(map[string]any{
		"project": "*",
	}))
	if err != nil {
		t.Fatalf("handleArchitecture: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result")
	}
	text := textOf(t, result)
	if !strings.Contains(text, "cross-project sentinel") {
		t.Errorf("expected error to identify * as a sentinel; got %q", text)
	}
}

// Control: a real-but-unknown project still gets the standard
// not-found error pointing at `list`.
func TestResolveProjectID_RealTypo_StillSaysNotFound(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	_, err := srv.resolveProjectID("totally-bogus-project")
	if err == nil {
		t.Fatal("expected error for unknown project")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not found") {
		t.Errorf("typo'd name must still hit the 'not found' message; got %q", msg)
	}
	if strings.Contains(msg, "sentinel") {
		t.Errorf("typo'd name must not be confused with the sentinel; got %q", msg)
	}
}
