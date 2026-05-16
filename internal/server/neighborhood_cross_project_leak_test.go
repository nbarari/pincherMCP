package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1051: extends #1049 / #1050 to neighborhood. Same shape — unscoped
// GetSymbol fallback resolves the seed id in any indexed project
// that carries it. Mirror projects (sniffer mirrors, MCP_Combine
// staging, .pincher-supported snapshots) carry identical seed IDs to
// their primary repo. Pre-fix neighborhood returned 224 neighbors
// from the wrong tree with no signal. An agent planning an in-file
// refactor would plan against the wrong file.
//
// #1232 update (2026-05-16): default behaviour flipped to strict-error.
// This test now pins the OPT-IN warning shape; strict-error coverage
// lives in neighborhood_cross_project_strict_test.go.

func TestHandleNeighborhood_CrossProject_OptInStillWarns(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-nbh-session"
	mirrorPID := "p-nbh-mirror"
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
		{ID: "shared.go::pkg.Sibling#Function", ProjectID: mirrorPID, FilePath: "shared.go",
			Name: "Sibling", QualifiedName: "pkg.Sibling", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":            "shared.go::pkg.Common#Function",
		"cross_project": true, // #1232 opt-in to legacy silent-fallback shape
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
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

// Control: session-scoped hit doesn't trip the warning.
func TestHandleNeighborhood_NoProject_SessionScopedHit_NoWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-nbh-ok"
	store.UpsertProject(db.Project{
		ID: sessionPID, Path: t.TempDir(), Name: sessionPID, IndexedAt: time.Now(),
	})
	srv.sessionID = sessionPID

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "a.go::pkg.Foo#Function", ProjectID: sessionPID, FilePath: "a.go",
			Name: "Foo", QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "a.go::pkg.Foo#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
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

// Control: explicit project arg means caller asked for that scope.
func TestHandleNeighborhood_ExplicitProject_NoCrossProjectWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	sessionPID := "p-nbh-exp"
	otherPID := "p-nbh-other"
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

	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":      "b.go::pkg.Bar#Function",
		"project": otherPID,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
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
