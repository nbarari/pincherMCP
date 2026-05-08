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

