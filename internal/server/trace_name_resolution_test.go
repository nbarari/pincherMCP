package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1032: handleTrace's `name=` path called GetSymbolsByName with
// LIMIT 5 and no SQL ORDER BY. For names where the project has >5
// matching rows AND the Module/Setting rows happen to land first in
// SQL row order (Go projects with `package main` declare a Module
// per file), all 5 returned rows were Module-kind. sortTraceCandidates
// then had nothing to pick — Module beats nothing, the Function never
// entered the candidate pool. The trace resolved to a Module which
// has no CALLS edges and looked like "this symbol is a leaf."
//
// Fix: fetch up to 50 candidates so the Go-side sort can prefer
// Function/Method over Module regardless of SQL row order.

func TestHandleTrace_NameWithManyModuleHomonyms_PrefersFunction(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-trace-name"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid

	// Seed 8 Module symbols all named "main" (simulating a Go package
	// where every file declares `package main` and the indexer extracts
	// one Module per file). These are inserted FIRST so they appear in
	// the first 5 SQL rows when LIMIT is small.
	syms := []db.Symbol{}
	for i := 0; i < 8; i++ {
		syms = append(syms, db.Symbol{
			ID:                   pid + "::cmd/dummy/file" + string(rune('A'+i)) + ".go::main#Module",
			ProjectID:            pid,
			FilePath:             "cmd/dummy/file" + string(rune('A'+i)) + ".go",
			Name:                 "main",
			QualifiedName:        "main",
			Kind:                 "Module",
			Language:             "Go",
			ExtractionConfidence: 1.0,
		})
	}
	// The actual main Function — inserted LAST so SQL row order would
	// place it 9th if there were no Function-preferring fetch logic.
	syms = append(syms, db.Symbol{
		ID:                   pid + "::cmd/dummy/main.go::main.main#Function",
		ProjectID:            pid,
		FilePath:             "cmd/dummy/main.go",
		Name:                 "main",
		QualifiedName:        "main.main",
		Kind:                 "Function",
		Language:             "Go",
		ExtractionConfidence: 1.0,
	})
	mustUpsertSymbols(t, store, syms)

	result, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":      "main",
		"direction": "inbound",
		"depth":     1,
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, result)

	// Pre-#1032 the resolved_to was a Module ID. Post-fix it must be
	// the Function ID — sortTraceCandidates prefers Function-kind and
	// the fetch cap (50) ensures the Function row is in the candidate
	// pool even when 8 Module rows come back first.
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	amb, _ := meta["ambiguous_match"].(map[string]any)
	if amb == nil {
		t.Fatal("expected ambiguous_match in _meta (9 symbols share the name)")
	}
	resolvedTo, _ := amb["resolved_to"].(string)
	wantSuffix := "main.go::main.main#Function"
	if resolvedTo == "" || !strings.Contains(resolvedTo, wantSuffix) {
		t.Errorf("trace must resolve to the Function over the Modules; resolved_to=%q want suffix %q",
			resolvedTo, wantSuffix)
	}
}

// Companion: alternatives list stays capped at 5 even though the fetch
// pulls more candidates internally. Pinned so the alternatives shape
// doesn't drift larger and bloat _meta on pathological names.
func TestHandleTrace_NameWithManyMatches_AlternativesCappedAt5(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-trace-alts"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid

	// 10 functions named "doStuff" across distinct files.
	syms := []db.Symbol{}
	for i := 0; i < 10; i++ {
		syms = append(syms, db.Symbol{
			ID:                   pid + "::pkg/file" + string(rune('A'+i)) + ".go::pkg.doStuff#Function",
			ProjectID:            pid,
			FilePath:             "pkg/file" + string(rune('A'+i)) + ".go",
			Name:                 "doStuff",
			QualifiedName:        "pkg.doStuff",
			Kind:                 "Function",
			Language:             "Go",
			ExtractionConfidence: 1.0,
		})
	}
	mustUpsertSymbols(t, store, syms)

	result, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":      "doStuff",
		"direction": "inbound",
		"depth":     1,
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	amb, _ := meta["ambiguous_match"].(map[string]any)
	if amb == nil {
		t.Fatal("expected ambiguous_match for 10-way collision")
	}
	alts, _ := amb["alternatives"].([]any)
	if len(alts) > 5 {
		t.Errorf("alternatives must stay capped at 5; got %d", len(alts))
	}
	if len(alts) == 0 {
		t.Errorf("alternatives should be non-empty for an ambiguous name; got 0")
	}
}

