package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1037: when handleSymbol returns "not found" AND the caller's project
// arg also failed to resolve, the pre-fix error only mentioned the
// symbol miss. An agent who'd typo'd both args (e.g. wrong project +
// bogus id) only learned about the id mistake, then hit the same
// project-resolve failure on their next call.

func TestHandleSymbol_BogusProjectAndBogusID_BothFailuresSurfaced(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-symbol-both"
	store.UpsertProject(db.Project{
		ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now(),
	})
	srv.sessionID = pid

	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id":      "does/not/exist.go::pkg.NoSuchThing#Function",
		"project": "totally-bogus-project",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result; got success: %s", textOf(t, result))
	}
	body := decode(t, result)
	errMsg, _ := body["error"].(string)

	// Both failure modes must appear in the message.
	if !strings.Contains(errMsg, "totally-bogus-project") {
		t.Errorf("error must surface the project-resolve failure; got %q", errMsg)
	}
	if !strings.Contains(errMsg, "did not resolve") {
		t.Errorf("error must use the project-resolve phrasing; got %q", errMsg)
	}
	if !strings.Contains(errMsg, "not found") {
		t.Errorf("error must still report the symbol-not-found failure; got %q", errMsg)
	}
}

// Control: not-found WITHOUT a project arg uses the original message
// shape (no project-resolve preamble).
func TestHandleSymbol_BogusIDNoProject_OriginalMessage(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-symbol-noprj"
	store.UpsertProject(db.Project{
		ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now(),
	})
	srv.sessionID = pid

	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id": "does/not/exist.go::pkg.NoSuchThing#Function",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	body := decode(t, result)
	errMsg, _ := body["error"].(string)
	if strings.Contains(errMsg, "did not resolve") {
		t.Errorf("must not surface project-resolve text when no project was passed; got %q", errMsg)
	}
	if !strings.Contains(errMsg, "not found") {
		t.Errorf("must still report the not-found failure; got %q", errMsg)
	}
}
