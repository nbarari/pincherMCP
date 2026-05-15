package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1024: handleStats's InputSchema declared a `project` parameter
// ("Project to include in index size breakdown. Defaults to session
// project."), but the handler never read args["project"]. The arg
// was silently ignored — passing `project=foo` returned the
// session project's stats with no signal of the override failure.
// Contract drift inside one tool.

func TestHandleStats_UnknownProject_WarnsAndFallsBack(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "stats-session-p"
	store.UpsertProject(db.Project{
		ID: "stats-session-p", Path: "/tmp/stats-session-p", Name: "stats-session-p",
		IndexedAt: time.Now(), FileCount: 10, SymCount: 100, EdgeCount: 50,
	})

	res, err := srv.handleStats(context.Background(), makeReq(map[string]any{
		"project": "totally-bogus-stats-project",
	}))
	if err != nil {
		t.Fatalf("handleStats: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success (falls back); got error: %s", textOf(t, res))
	}
	text := textOf(t, res)
	if !strings.Contains(text, "warning:") || !strings.Contains(text, "totally-bogus-stats-project") {
		t.Errorf("expected inlined resolve warning naming the failed lookup; got %q", text)
	}
	// Session project's stats should still appear (fallback behavior).
	if !strings.Contains(text, "stats-session-p") {
		t.Errorf("expected session project name in fallback output; got %q", text)
	}
}

func TestHandleStats_ResolvableProject_NoWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "stats-default"
	store.UpsertProject(db.Project{
		ID: "stats-default", Path: "/tmp/stats-default", Name: "stats-default",
		IndexedAt: time.Now(),
	})
	store.UpsertProject(db.Project{
		ID: "stats-other", Path: "/tmp/stats-other", Name: "stats-other",
		IndexedAt: time.Now(), FileCount: 5, SymCount: 50, EdgeCount: 25,
	})

	res, err := srv.handleStats(context.Background(), makeReq(map[string]any{
		"project": "stats-other",
	}))
	if err != nil {
		t.Fatalf("handleStats: %v", err)
	}
	text := textOf(t, res)
	if strings.Contains(text, "did not resolve") {
		t.Errorf("resolved project must not produce warning; got %q", text)
	}
	// The PROJECT section should show stats-other, not the session default.
	if !strings.Contains(text, "stats-other") {
		t.Errorf("expected stats-other in PROJECT section; got %q", text)
	}
	if strings.Contains(text, "stats-default") {
		t.Errorf("expected stats-default (session) NOT shown when project= override resolved; got %q", text)
	}
}
