package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1043: handleQuery silently returned 0 rows on ghost-extraction
// projects without telling the caller the project itself was the
// problem. Every edge-traversal query against a project with 0 edges
// will look the same — pre-fix the empty result was indistinguishable
// from a true empty match. Companion to #1040 (architecture) /
// #1042 (schema).

func TestHandleQuery_GhostExtractionProject_EmptyRowsDiagnoseGhost(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-query-ghost"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
		FileCount: 10, SymCount: 150, EdgeCount: 0,
	})
	srv.sessionID = pid

	// Seed 150 callable Go symbols (above the threshold) but NO edges.
	syms := []db.Symbol{}
	for i := 0; i < 150; i++ {
		syms = append(syms, db.Symbol{
			ID:                   pid + "::pkg.F" + string(rune('A'+i%26)) + string(rune('A'+i/26)) + "#Function",
			ProjectID:            pid,
			FilePath:             "a.go",
			Name:                 "F" + string(rune('A'+i%26)) + string(rune('A'+i/26)),
			QualifiedName:        "pkg.F" + string(rune('A'+i%26)) + string(rune('A'+i/26)),
			Kind:                 "Function",
			Language:             "Go",
			ExtractionConfidence: 1.0,
		})
	}
	mustUpsertSymbols(t, store, syms)

	// Edge-traversal query that will return 0 rows since edges==0.
	result, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql": `MATCH (a)-[:CALLS]->(b) RETURN a.name, b.name LIMIT 3`,
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, result)

	rows, _ := body["rows"].([]any)
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows for a ghost project's edge query; got %d", len(rows))
	}

	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing — expected ghost-extraction diagnosis")
	}
	diagnosis, _ := meta["diagnosis"].(string)
	if !strings.Contains(diagnosis, "ghost-extraction") {
		t.Errorf("expected ghost-extraction diagnosis; got %q", diagnosis)
	}
}

// Control: a healthy project with edges should return its result
// without the ghost diagnosis even when the query happens to match nothing.
func TestHandleQuery_HealthyProjectEmptyResult_NoGhostDiagnosis(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-query-healthy"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
	})
	srv.sessionID = pid

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::pkg.A#Function", ProjectID: pid, FilePath: "a.go",
			Name: "A", QualifiedName: "pkg.A", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		{ID: pid + "::pkg.B#Function", ProjectID: pid, FilePath: "a.go",
			Name: "B", QualifiedName: "pkg.B", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})
	if err := store.BulkUpsertEdges([]db.Edge{
		{ProjectID: pid, FromID: pid + "::pkg.A#Function", ToID: pid + "::pkg.B#Function", Kind: "CALLS", Confidence: 1},
	}); err != nil {
		t.Fatal(err)
	}

	// Query that matches nothing (no node named "DefinitelyNotHere").
	result, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql": `MATCH (n) WHERE n.name = "DefinitelyNotHere" RETURN n.name`,
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return
	}
	diagnosis, _ := meta["diagnosis"].(string)
	if strings.Contains(diagnosis, "ghost-extraction") {
		t.Errorf("must not claim ghost-extraction for healthy project; got %q", diagnosis)
	}
}
