package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1055: extends the #1048 cross-project-sentinel fix from per-ID
// retrieval tools (symbol/symbols/context/neighborhood) to the
// aggregate tools (health/stats). Pre-fix passing project="*" to
// either tool produced a misleading "did not resolve — falling back"
// warning as if "*" were a typo — but the caller passed it
// deliberately (likely thinking the tool supports cross-project the
// way search/query do). Now "*" is treated silently as "fall back
// to session/global view," same shape as #1048.

func TestHandleHealth_StarSentinel_NoWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-health-star"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	result, err := srv.handleHealth(context.Background(), makeReq(map[string]any{
		"project": "*",
	}))
	if err != nil {
		t.Fatalf("handleHealth: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return
	}
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, `"*"`) && strings.Contains(s, "did not resolve") {
			t.Errorf("health: project=* must not warn 'did not resolve'; got %q", s)
		}
	}
}

func TestHandleStats_StarSentinel_NoWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-stats-star"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	result, err := srv.handleStats(context.Background(), makeReq(map[string]any{
		"project": "*",
	}))
	if err != nil {
		t.Fatalf("handleStats: %v", err)
	}
	// Stats renders human-readable text; warning is prepended as
	// `warning: project "*" did not resolve...` if it fires.
	out := textOf(t, result)
	if strings.Contains(out, `"*"`) && strings.Contains(out, "did not resolve") {
		t.Errorf("stats: project=* must not produce did-not-resolve warning; got %q", out)
	}
}

// Control: genuinely unknown project name still warns.
func TestHandleHealth_UnknownProject_StillWarns(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-health-ok"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	result, err := srv.handleHealth(context.Background(), makeReq(map[string]any{
		"project": "totally-bogus",
	}))
	if err != nil {
		t.Fatalf("handleHealth: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing — expected warning on unknown project")
	}
	warnings, _ := meta["warnings"].([]any)
	saw := false
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "totally-bogus") && strings.Contains(s, "did not resolve") {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("unknown project must warn; got %v", warnings)
	}
}

func TestHandleStats_UnknownProject_StillWarns(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-stats-ok"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	result, err := srv.handleStats(context.Background(), makeReq(map[string]any{
		"project": "totally-bogus",
	}))
	if err != nil {
		t.Fatalf("handleStats: %v", err)
	}
	// Stats renders text; the project-resolve warning is prepended.
	out := textOf(t, result)
	if !strings.Contains(out, "totally-bogus") || !strings.Contains(out, "did not resolve") {
		t.Errorf("unknown project must surface 'did not resolve' warning in text; got %q", out)
	}
}
