package db

import (
	"sort"
	"testing"
)

// #1629 v0.87: store-layer primitives for the incremental resolve
// pass. Two new methods land together: LoadPendingEdgesByKindAndFiles
// scopes the pending-edges load to a subset of files; the matching
// delete (DeleteResolvePassEdgesByKindForSourceFiles) wipes only the
// resolve_pass edges originating from those files. The pair lets the
// resolver skip work on unchanged files during watcher ticks where
// only a handful of files were re-extracted.

func TestLoadPendingEdgesByKindAndFiles_FiltersToScope_1629(t *testing.T) {
	store := newTestStore(t)
	const projectID = "p-load-scope"
	if err := store.UpsertProject(Project{
		ID: projectID, Path: t.TempDir(), Name: "load-scope-test",
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	// Seed pending edges across three files, two kinds. Note:
	// ReplacePendingEdgesForFile is per-FILE (all kinds at once),
	// so we must group by file before inserting, not by kind.
	rows := []PendingEdge{
		{ProjectID: projectID, FromFile: "a.go", Kind: "IMPORTS", FromQN: "p.a", ToName: "fmt", Confidence: 1.0},
		{ProjectID: projectID, FromFile: "a.go", Kind: "CALLS", FromQN: "p.a.F", ToName: "Foo", Confidence: 1.0},
		{ProjectID: projectID, FromFile: "b.go", Kind: "IMPORTS", FromQN: "p.b", ToName: "fmt", Confidence: 1.0},
		{ProjectID: projectID, FromFile: "c.go", Kind: "IMPORTS", FromQN: "p.c", ToName: "context", Confidence: 1.0},
	}
	byFile := map[string][]PendingEdge{}
	for _, e := range rows {
		byFile[e.FromFile] = append(byFile[e.FromFile], e)
	}
	for file, edges := range byFile {
		if err := store.ReplacePendingEdgesForFile(projectID, file, edges); err != nil {
			t.Fatalf("ReplacePendingEdgesForFile %s: %v", file, err)
		}
	}

	// Scope to just a.go — expect IMPORTS rows only from a.go.
	got, err := store.LoadPendingEdgesByKindAndFiles(projectID, "IMPORTS", []string{"a.go"})
	if err != nil {
		t.Fatalf("LoadPendingEdgesByKindAndFiles: %v", err)
	}
	if len(got) != 1 || got[0].FromFile != "a.go" || got[0].ToName != "fmt" {
		t.Errorf("expected one a.go IMPORTS edge to fmt; got %+v", got)
	}

	// Scope to two files — expect both their IMPORTS.
	got2, err := store.LoadPendingEdgesByKindAndFiles(projectID, "IMPORTS", []string{"a.go", "c.go"})
	if err != nil {
		t.Fatalf("LoadPendingEdgesByKindAndFiles two-file: %v", err)
	}
	if len(got2) != 2 {
		t.Fatalf("two-file scope: got %d rows, want 2: %+v", len(got2), got2)
	}
	sort.Slice(got2, func(i, j int) bool { return got2[i].FromFile < got2[j].FromFile })
	if got2[0].FromFile != "a.go" || got2[1].FromFile != "c.go" {
		t.Errorf("two-file scope returned wrong files: %+v", got2)
	}
}

func TestLoadPendingEdgesByKindAndFiles_EmptyFiles_ReturnsNil_1629(t *testing.T) {
	store := newTestStore(t)
	got, err := store.LoadPendingEdgesByKindAndFiles("any", "IMPORTS", nil)
	if err != nil {
		t.Fatalf("nil files: %v", err)
	}
	if got != nil {
		t.Errorf("empty files should return nil; got %v", got)
	}
}

func TestDeleteResolvePassEdgesByKindForSourceFiles_ScopesToFiles_1629(t *testing.T) {
	store := newTestStore(t)
	const projectID = "p-delete-scope"
	if err := store.UpsertProject(Project{
		ID: projectID, Path: t.TempDir(), Name: "delete-scope-test",
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	// Seed symbols: 2 in a.go, 2 in b.go.
	syms := []Symbol{
		{ID: projectID + "::a.go::p.A1#Function", ProjectID: projectID, FilePath: "a.go", Name: "A1", QualifiedName: "p.A1", Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		{ID: projectID + "::a.go::p.A2#Function", ProjectID: projectID, FilePath: "a.go", Name: "A2", QualifiedName: "p.A2", Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		{ID: projectID + "::b.go::p.B1#Function", ProjectID: projectID, FilePath: "b.go", Name: "B1", QualifiedName: "p.B1", Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		{ID: projectID + "::b.go::p.B2#Function", ProjectID: projectID, FilePath: "b.go", Name: "B2", QualifiedName: "p.B2", Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
	}
	if err := store.BulkUpsertSymbols(syms); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	// Seed resolve_pass edges: A1->A2 (a.go), B1->B2 (b.go), one cross
	// from A1->B1 (a.go). All as resolve_pass source.
	edges := []Edge{
		{ProjectID: projectID, FromID: syms[0].ID, ToID: syms[1].ID, Kind: "CALLS", Source: "resolve_pass", Confidence: 1.0},
		{ProjectID: projectID, FromID: syms[2].ID, ToID: syms[3].ID, Kind: "CALLS", Source: "resolve_pass", Confidence: 1.0},
		{ProjectID: projectID, FromID: syms[0].ID, ToID: syms[2].ID, Kind: "CALLS", Source: "resolve_pass", Confidence: 1.0},
		// Also one per_file edge — must NOT be deleted (different source).
		{ProjectID: projectID, FromID: syms[0].ID, ToID: syms[3].ID, Kind: "CALLS", Source: "per_file", Confidence: 1.0},
	}
	if err := store.BulkUpsertEdges(edges); err != nil {
		t.Fatalf("BulkUpsertEdges: %v", err)
	}

	// Verify pre-delete counts via a direct COUNT query — EdgesFrom
	// doesn't expose source so we go through the raw db handle.
	countBy := func(t *testing.T, source string) int {
		t.Helper()
		var n int
		if err := store.db.QueryRow(
			`SELECT COUNT(*) FROM edges WHERE project_id=? AND source=?`,
			projectID, source,
		).Scan(&n); err != nil {
			t.Fatalf("count query: %v", err)
		}
		return n
	}
	if got := countBy(t, "resolve_pass"); got != 3 {
		t.Fatalf("expected 3 resolve_pass edges pre-delete; got %d", got)
	}
	if got := countBy(t, "per_file"); got != 1 {
		t.Fatalf("expected 1 per_file edge pre-delete; got %d", got)
	}

	// Delete a.go's resolve_pass CALLS edges. Should remove 2 (A1->A2,
	// A1->B1) but leave b.go's (B1->B2) and the per_file edge.
	if err := store.DeleteResolvePassEdgesByKindForSourceFiles(projectID, "CALLS", []string{"a.go"}); err != nil {
		t.Fatalf("DeleteResolvePassEdgesByKindForSourceFiles: %v", err)
	}

	if got := countBy(t, "resolve_pass"); got != 1 {
		t.Errorf("post-delete: expected 1 resolve_pass edge (b.go's B1->B2); got %d", got)
	}
	if got := countBy(t, "per_file"); got != 1 {
		t.Errorf("post-delete: per_file edge must be preserved; got %d", got)
	}
}

func TestDeleteResolvePassEdgesByKindForSourceFiles_EmptyFiles_NoOp_1629(t *testing.T) {
	store := newTestStore(t)
	if err := store.DeleteResolvePassEdgesByKindForSourceFiles("p", "CALLS", nil); err != nil {
		t.Errorf("empty files should be no-op; got: %v", err)
	}
	if err := store.DeleteResolvePassEdgesByKindForSourceFiles("p", "CALLS", []string{}); err != nil {
		t.Errorf("empty slice should be no-op; got: %v", err)
	}
}
