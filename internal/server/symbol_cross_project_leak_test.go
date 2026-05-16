package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1049: handleSymbol's unscoped fallback (when projectArg=="") finds
// the ID in any indexed project that happens to carry it. Mirror
// projects (sniffer mirrors, MCP_Combine staging, .pincher-supported
// snapshots) routinely have identical symbol IDs to their primary
// repo. Pre-fix the response carried source bytes from a different
// project with NO signal the lookup crossed project boundaries —
// agents using the result to edit code were editing the wrong tree.
// Self-discovered probing pincher-repo: `symbol id=cmd/pinch/main.go::
// main.main#Function` (no project) returned source from the sniffer
// mirror, not pincher-repo.
//
// #1232 update (2026-05-16): the default behaviour flipped to strict-
// error — silent-cross-project is now an explicit choice (cross_project=
// true). This test now pins the OPT-IN warning shape; the strict-error
// path is covered by TestHandleSymbol_CrossProject_StrictErrorByDefault.

func TestHandleSymbol_CrossProject_OptInStillWarns(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-session"
	mirrorPID := "p-mirror"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	store.UpsertProject(db.Project{
		ID: mirrorPID, Path: t.TempDir(), Name: mirrorPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	// Identical symbol ID lives in mirror project only; session has
	// nothing at this id. Unscoped GetSymbol will find the mirror row.
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "shared.go::pkg.Common#Function", ProjectID: mirrorPID, FilePath: "shared.go",
			Name: "Common", QualifiedName: "pkg.Common", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id":            "shared.go::pkg.Common#Function",
		"cross_project": true, // #1232 opt-in to legacy silent-fallback shape
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing — expected cross-project warning on opt-in path")
	}
	warnings, _ := meta["warnings"].([]any)
	saw := false
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "resolved from project") &&
			strings.Contains(s, mirrorPID) &&
			strings.Contains(s, sessionPID) {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("expected cross-project warning naming mirror + session; got %v", warnings)
	}
}

// Control: symbol lives in the session project itself — no warning.
func TestHandleSymbol_NoProject_SessionScopedHit_NoWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-session-ok"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "a.go::pkg.Foo#Function", ProjectID: sessionPID, FilePath: "a.go",
			Name: "Foo", QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id": "a.go::pkg.Foo#Function",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return
	}
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "resolved from project") {
			t.Errorf("session-scoped hit must not warn cross-project; got %q", s)
		}
	}
}

// Control: explicit project arg means the caller asked for that
// scope deliberately — no cross-project warning regardless of
// session.
func TestHandleSymbol_ExplicitProject_NoCrossProjectWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-session-exp"
	otherPID := "p-other"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	store.UpsertProject(db.Project{
		ID: otherPID, Path: t.TempDir(), Name: otherPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "b.go::pkg.Bar#Function", ProjectID: otherPID, FilePath: "b.go",
			Name: "Bar", QualifiedName: "pkg.Bar", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id":      "b.go::pkg.Bar#Function",
		"project": otherPID,
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return
	}
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "resolved from project") {
			t.Errorf("explicit project arg must not trip cross-project warning; got %q", s)
		}
	}
}

// Control: project="*" sentinel — caller asked for any project — no warning.
func TestHandleSymbol_StarProject_NoCrossProjectWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-session-star"
	otherPID := "p-other-star"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	store.UpsertProject(db.Project{
		ID: otherPID, Path: t.TempDir(), Name: otherPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "c.go::pkg.Baz#Function", ProjectID: otherPID, FilePath: "c.go",
			Name: "Baz", QualifiedName: "pkg.Baz", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id":      "c.go::pkg.Baz#Function",
		"project": "*",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return
	}
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "resolved from project") {
			t.Errorf("project=* sentinel must not trip cross-project warning; got %q", s)
		}
	}
}
