package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1027: handleSymbols (batch) silently fell back to unscoped batch
// lookup when the caller's `project` arg didn't resolve. Same
// silent-fallback shape as #1023 (health) / #1024 (stats) /
// #1025 (neighborhood) / #1026 (symbol).

func TestHandleSymbols_UnknownProject_WarnsAndFallsBack(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "syms-sess"
	store.UpsertProject(db.Project{
		ID: "syms-sess", Path: "/tmp/syms-sess", Name: "syms-sess",
		IndexedAt: time.Now(),
	})
	if err := store.BulkUpsertSymbols([]db.Symbol{{
		ID:                   "test.go::pkg.Foo#Function",
		ProjectID:            "syms-sess",
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

	res, err := srv.handleSymbols(context.Background(), makeReq(map[string]any{
		"ids":     []any{"test.go::pkg.Foo#Function"},
		"project": "totally-bogus-project",
	}))
	if err != nil {
		t.Fatalf("handleSymbols: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success (fallback); got error: %s", textOf(t, res))
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
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
