package index

import (
	"context"
	"os"
	"testing"

	"github.com/pincherMCP/pincher/internal/db"
)

// stale_symbol_cleanup_test pins the LATENT #15 fix: when a file is re-parsed
// and a previously-emitted symbol is no longer present in the new source, the
// indexer must delete the orphaned row so search/symbol/trace don't return
// stale data. The fix is a per-file DeleteSymbolsForFile call in the indexer
// goroutine, before re-extraction.

const staleFixtureBefore = `package svc

// KeepMe stays through the edit.
func KeepMe() string {
	return "keep"
}

// RemoveMe gets deleted in the edited version.
func RemoveMe() int {
	return 42
}

// AlsoKeep stays.
func AlsoKeep() bool {
	return true
}
`

const staleFixtureAfter = `package svc

// KeepMe stays through the edit.
func KeepMe() string {
	return "keep"
}

// AlsoKeep stays.
func AlsoKeep() bool {
	return true
}
`

func TestIndex_DeletesOrphanedSymbolsOnEdit(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "svc/svc.go", staleFixtureBefore)
	pid := db.ProjectIDFromPath(dir)

	// Initial index — all three functions present.
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("first index: %v", err)
	}
	for _, want := range []string{"KeepMe", "RemoveMe", "AlsoKeep"} {
		syms, err := store.GetSymbolsByName(pid, want, 5)
		if err != nil || len(syms) == 0 {
			t.Fatalf("expected %s indexed before edit; err=%v len=%d", want, err, len(syms))
		}
	}

	// Edit: remove RemoveMe.
	if err := os.WriteFile(path, []byte(staleFixtureAfter), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}

	// Re-index without force; hash differs → file is re-parsed.
	res2, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("re-index: %v", err)
	}
	if res2.Files != 1 {
		t.Errorf("expected 1 changed file on re-index, got %d", res2.Files)
	}

	// RemoveMe must be gone.
	syms, err := store.GetSymbolsByName(pid, "RemoveMe", 5)
	if err != nil {
		t.Fatalf("GetSymbolsByName(RemoveMe): %v", err)
	}
	if len(syms) != 0 {
		qns := make([]string, len(syms))
		for i, s := range syms {
			qns[i] = s.QualifiedName
		}
		t.Errorf("expected RemoveMe to be deleted after edit, got %d matching: %v", len(syms), qns)
	}

	// KeepMe and AlsoKeep must still resolve.
	for _, want := range []string{"KeepMe", "AlsoKeep"} {
		syms, err := store.GetSymbolsByName(pid, want, 5)
		if err != nil || len(syms) == 0 {
			t.Errorf("expected %s to still exist after edit; err=%v len=%d", want, err, len(syms))
		}
	}
}

func TestIndex_DeletesOrphanedSymbolsOnEmptyFile(t *testing.T) {
	// Edge case: file used to have symbols, now has none (e.g., user deleted
	// every function but kept the package declaration). Symbols should still
	// be cleared even though the new parse yields nothing extractable other
	// than the Module symbol.
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "svc/svc.go", staleFixtureBefore)
	pid := db.ProjectIDFromPath(dir)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("first index: %v", err)
	}

	if err := os.WriteFile(path, []byte("package svc\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("re-index: %v", err)
	}

	for _, removed := range []string{"KeepMe", "RemoveMe", "AlsoKeep"} {
		syms, err := store.GetSymbolsByName(pid, removed, 5)
		if err != nil {
			t.Fatalf("GetSymbolsByName(%s): %v", removed, err)
		}
		if len(syms) != 0 {
			t.Errorf("expected %s to be deleted after empty-file edit, got %d", removed, len(syms))
		}
	}
}

func TestIndex_NoDeleteWhenFileUnchanged(t *testing.T) {
	// Hash-skip path: when content is unchanged, the per-file goroutine
	// shouldn't run at all, so no DELETE fires. Verify by counting symbols
	// before and after a no-op re-index.
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/svc.go", staleFixtureBefore)
	pid := db.ProjectIDFromPath(dir)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("first index: %v", err)
	}
	beforeCount := countAllSymbols(t, store, pid)

	res2, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("re-index: %v", err)
	}
	if res2.Skipped == 0 {
		t.Error("expected hash-skip on no-op re-index, got Skipped=0")
	}
	if res2.Files != 0 {
		t.Errorf("expected 0 changed files on no-op re-index, got %d", res2.Files)
	}
	afterCount := countAllSymbols(t, store, pid)
	if afterCount != beforeCount {
		t.Errorf("symbol count drifted on no-op re-index: before=%d after=%d", beforeCount, afterCount)
	}
}

func TestIndex_ForceReindexResultsAreClean(t *testing.T) {
	// force=true re-parses every file. The per-file DELETE then INSERT path
	// should produce identical symbol counts to the initial index — no
	// duplicates from the upsert-on-ID semantics meeting fresh DELETEs.
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/svc.go", staleFixtureBefore)
	writeFile(t, dir, "other/other.go", "package other\nfunc Do() {}\n")
	pid := db.ProjectIDFromPath(dir)

	if _, err := idx.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("first index: %v", err)
	}
	beforeIDs := allSymbolIDs(t, store, pid)

	if _, err := idx.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("force re-index: %v", err)
	}
	afterIDs := allSymbolIDs(t, store, pid)

	if len(beforeIDs) != len(afterIDs) {
		t.Errorf("force re-index changed symbol count: before=%d after=%d", len(beforeIDs), len(afterIDs))
	}
	for id := range beforeIDs {
		if _, ok := afterIDs[id]; !ok {
			t.Errorf("symbol ID %q dropped after force re-index", id)
		}
	}
}

// TestIndex_StaleCleanupCascadesToCrossFileEdges pins the cross-feature
// contract between PR #31 (stale-symbol cleanup) and PR #27 (cross-file
// CALLS resolution): when a target function with inbound CALLS edges from
// a different file is removed, both the symbol AND the inbound edges are
// cleared.
//
// The DB-level `DeleteSymbolsForFile` already cascades to edges (covered by
// db_test.go's TestDeleteSymbolsForFile). What this test pins is the full
// indexer→delete→edge-cascade path in the presence of cross-file edges
// that were resolved in the deferred `resolveCalls` pass — a path no
// individual PR exercised end-to-end.
func TestIndex_StaleCleanupCascadesToCrossFileEdges(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	// Setup: caller.go imports & calls Helper from helpers.go.
	// resolveCalls runs in the deferred pass so this edge only resolves at
	// the project level after wg.Wait().
	writeFile(t, dir, "helpers.go", `package svc

func Helper() string {
	return "hi"
}
`)
	writeFile(t, dir, "caller.go", `package svc

func Run() {
	_ = Helper()
}
`)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("first index: %v", err)
	}
	pid := db.ProjectIDFromPath(dir)

	// Sanity: the cross-file CALLS edge from Run → Helper exists.
	helperSyms, err := store.GetSymbolsByName(pid, "Helper", 5)
	if err != nil || len(helperSyms) == 0 {
		t.Fatalf("expected Helper indexed; err=%v len=%d", err, len(helperSyms))
	}
	helperID := helperSyms[0].ID

	var inbound int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM edges WHERE project_id = ? AND to_id = ? AND kind = 'CALLS'`,
		pid, helperID,
	).Scan(&inbound); err != nil {
		t.Fatalf("count inbound edges: %v", err)
	}
	if inbound == 0 {
		t.Fatalf("expected at least one CALLS edge into Helper before deletion (resolveCalls regression?)")
	}

	// Delete Helper from helpers.go (replace with just `package svc`).
	helpersPath := writeFile(t, dir, "helpers.go", "package svc\n")
	_ = helpersPath

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("re-index: %v", err)
	}

	// Negative: Helper symbol is gone.
	helperSyms2, err := store.GetSymbolsByName(pid, "Helper", 5)
	if err != nil {
		t.Fatalf("GetSymbolsByName(Helper) after delete: %v", err)
	}
	if len(helperSyms2) != 0 {
		t.Errorf("expected Helper to be deleted after edit, got %d", len(helperSyms2))
	}

	// Negative: no orphan inbound CALLS edges remain to the deleted symbol's ID.
	// (The edge-cascade in DeleteSymbolsForFile covers `from_id OR to_id` in
	// the deleted file. Edges to a deleted helper from a different file are
	// caught only because helpers.go's deletion swept the to_id endpoint.)
	var orphan int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM edges WHERE project_id = ? AND to_id = ?`,
		pid, helperID,
	).Scan(&orphan); err != nil {
		t.Fatalf("count orphan edges: %v", err)
	}
	if orphan != 0 {
		t.Errorf("expected 0 orphan edges into deleted Helper (id=%s), got %d", helperID, orphan)
	}

	// Positive: Run is still present and well-formed (the OTHER file's index
	// untouched).
	runSyms, err := store.GetSymbolsByName(pid, "Run", 5)
	if err != nil || len(runSyms) == 0 {
		t.Errorf("expected Run to survive deletion of Helper; err=%v len=%d", err, len(runSyms))
	}
}

func countAllSymbols(t *testing.T, store *db.Store, projectID string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM symbols WHERE project_id = ?`, projectID,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func allSymbolIDs(t *testing.T, store *db.Store, projectID string) map[string]struct{} {
	t.Helper()
	rows, err := store.DB().Query(
		`SELECT id FROM symbols WHERE project_id = ?`, projectID,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[id] = struct{}{}
	}
	return out
}

