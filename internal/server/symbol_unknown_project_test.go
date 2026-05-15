package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1026: handleSymbol used to silently fall back to unscoped symbol
// lookup when the caller's `project` arg didn't resolve. A typo'd
// project name + a valid id returned the symbol from whatever
// project had it — no signal the scope override failed. Same
// silent-fallback shape as #1023 (health), #1024 (stats), #1025
// (neighborhood). Now: clamp warning naming the failed lookup.

func TestHandleSymbol_UnknownProject_WarnsAndFallsBack(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "symbol-sess"
	store.UpsertProject(db.Project{
		ID: "symbol-sess", Path: "/tmp/symbol-sess", Name: "symbol-sess",
		IndexedAt: time.Now(),
	})
	// Seed a real symbol.
	if err := store.BulkUpsertSymbols([]db.Symbol{{
		ID:                   "test.go::pkg.Foo#Function",
		ProjectID:            "symbol-sess",
		FilePath:             "test.go",
		Name:                 "Foo",
		QualifiedName:        "pkg.Foo",
		Kind:                 "Function",
		Language:             "Go",
		Signature:            "func Foo()",
		ExtractionConfidence: 1.0,
	}}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	res, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id":      "test.go::pkg.Foo#Function",
		"project": "totally-bogus-project",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success (fallback); got error: %s", textOf(t, res))
	}

	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	warnings, _ := meta["warnings"].([]any)
	foundWarn := false
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "totally-bogus-project") && strings.Contains(s, "did not resolve") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("expected project-resolution warning naming the failed lookup; got warnings=%v", warnings)
	}
}
