package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1050: extends #1049 to context + symbols (batch). Same shape —
// unscoped GetSymbol fallback resolves IDs in any indexed project
// that happens to carry them. Mirror projects carry identical
// symbol IDs to their primary repo; pre-fix the agent got source
// bytes from a stale fork with no signal. context is particularly
// dangerous because its EdgesFrom walks the leaked project's
// graph, so callees + imports also come from the wrong tree.
//
// #1232 update (2026-05-16): default behaviour flipped to strict-
// error — silent-cross-project is now an explicit choice (cross_project=
// true). This test now pins the OPT-IN warning shape; strict-error
// coverage lives in context_cross_project_strict_test.go.

func TestHandleContext_CrossProject_OptInStillWarns(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-ctx-session"
	mirrorPID := "p-ctx-mirror"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	store.UpsertProject(db.Project{
		ID: mirrorPID, Path: t.TempDir(), Name: mirrorPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "shared.go::pkg.Common#Function", ProjectID: mirrorPID, FilePath: "shared.go",
			Name: "Common", QualifiedName: "pkg.Common", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id":            "shared.go::pkg.Common#Function",
		"lite":          true,
		"cross_project": true, // #1232 opt-in to legacy silent-fallback shape
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
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

// Control: context with session-scoped hit must not warn.
func TestHandleContext_NoProject_SessionScopedHit_NoWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-ctx-ok"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "a.go::pkg.Foo#Function", ProjectID: sessionPID, FilePath: "a.go",
			Name: "Foo", QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id":   "a.go::pkg.Foo#Function",
		"lite": true,
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
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

// symbols (batch) — at least one ID resolves cross-project.
func TestHandleSymbols_NoProject_CrossProjectResolution_Warns(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-syms-session"
	mirrorPID := "p-syms-mirror"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	store.UpsertProject(db.Project{
		ID: mirrorPID, Path: t.TempDir(), Name: mirrorPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	// One ID resolves to mirror, one is unknown.
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "a.go::pkg.Foo#Function", ProjectID: mirrorPID, FilePath: "a.go",
			Name: "Foo", QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSymbols(context.Background(), makeReq(map[string]any{
		"ids": []any{
			"a.go::pkg.Foo#Function",
			"b.go::pkg.Bar#Function", // not found
		},
	}))
	if err != nil {
		t.Fatalf("handleSymbols: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing — expected cross-project warning")
	}
	warnings, _ := meta["warnings"].([]any)
	saw := false
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "of 2 resolved symbol") &&
			strings.Contains(s, mirrorPID) &&
			strings.Contains(s, sessionPID) {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("expected cross-project warning naming mirror + session + count; got %v", warnings)
	}
}

// Control: symbols batch all-session-scoped — no warning.
func TestHandleSymbols_NoProject_AllSessionScoped_NoWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-syms-ok"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "a.go::pkg.Foo#Function", ProjectID: sessionPID, FilePath: "a.go",
			Name: "Foo", QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		{ID: "b.go::pkg.Bar#Function", ProjectID: sessionPID, FilePath: "b.go",
			Name: "Bar", QualifiedName: "pkg.Bar", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSymbols(context.Background(), makeReq(map[string]any{
		"ids": []any{
			"a.go::pkg.Foo#Function",
			"b.go::pkg.Bar#Function",
		},
	}))
	if err != nil {
		t.Fatalf("handleSymbols: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return
	}
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "resolved symbol") &&
			strings.Contains(s, "rather than the session project") {
			t.Errorf("all-session batch must not warn cross-project; got %q", s)
		}
	}
}

// Control: symbols batch with explicit project arg — caller asked
// for that scope, no leak possible.
func TestHandleSymbols_ExplicitProject_NoCrossProjectWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-syms-exp"
	otherPID := "p-syms-other"
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

	result, err := srv.handleSymbols(context.Background(), makeReq(map[string]any{
		"ids":     []any{"c.go::pkg.Baz#Function"},
		"project": otherPID,
	}))
	if err != nil {
		t.Fatalf("handleSymbols: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return
	}
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "rather than the session project") {
			t.Errorf("explicit project arg must not trip cross-project warning; got %q", s)
		}
	}
}
