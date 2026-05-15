package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1042: handleSchema silently returned bare counts for ghost-extraction
// projects (symbols extracted, edges==0). Agents seeing
// `{node_kinds:{Function: 327, ...}, edges: 0}` could mistake it for
// a config/docs-only project or miss the resolver failure entirely.
// Companion to #1040 (architecture ghost-extraction diagnosis).

func TestHandleSchema_GhostExtraction_DiagnosisNamesIt(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-schema-ghost"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
		FileCount: 10, SymCount: 100, EdgeCount: 0,
	})
	srv.sessionID = pid

	// Seed 100 callable Go symbols (above the ghost-diagnosis threshold)
	// — kindCounts will report Function = 100, edgeCount == 0.
	syms := []db.Symbol{}
	for i := 0; i < 100; i++ {
		syms = append(syms, db.Symbol{
			ID:                   pid + "::pkg.Func" + string(rune('A'+i)) + "#Function",
			ProjectID:            pid,
			FilePath:             "a.go",
			Name:                 "Func" + string(rune('A'+i)),
			QualifiedName:        "pkg.Func" + string(rune('A'+i)),
			Kind:                 "Function",
			Language:             "Go",
			ExtractionConfidence: 1.0,
		})
	}
	mustUpsertSymbols(t, store, syms)

	result, err := srv.handleSchema(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleSchema: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing — expected ghost-extraction diagnosis")
	}
	diagnosis, _ := meta["diagnosis"].(string)
	if !strings.Contains(diagnosis, "ghost-extraction") {
		t.Errorf("expected ghost-extraction diagnosis; got %q", diagnosis)
	}
	if !strings.Contains(diagnosis, "ZERO edges") {
		t.Errorf("diagnosis must name the ZERO edges signal; got %q", diagnosis)
	}
}

// Control: a healthy project (edges > 0) doesn't trip the diagnosis.
func TestHandleSchema_HealthyProject_NoDiagnosis(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-schema-healthy"
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

	result, err := srv.handleSchema(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleSchema: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return // no diagnosis is fine
	}
	diagnosis, _ := meta["diagnosis"].(string)
	if strings.Contains(diagnosis, "ghost-extraction") {
		t.Errorf("must not claim ghost-extraction for a healthy project with edges; got %q", diagnosis)
	}
}

// A project with edges==0 but only docs/config kinds (no callable
// kinds) should NOT trip the ghost-extraction diagnosis — those
// projects legitimately have zero edges.
func TestHandleSchema_DocsOnlyZeroEdges_NoGhostDiagnosis(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-schema-docs"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
	})
	srv.sessionID = pid

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::a.yaml::root.setting#Setting", ProjectID: pid, FilePath: "a.yaml",
			Name: "setting", QualifiedName: "root.setting", Kind: "Setting", Language: "YAML",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSchema(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleSchema: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return
	}
	diagnosis, _ := meta["diagnosis"].(string)
	if strings.Contains(diagnosis, "ghost-extraction") {
		t.Errorf("must not claim ghost-extraction for docs-only project; got %q", diagnosis)
	}
}
