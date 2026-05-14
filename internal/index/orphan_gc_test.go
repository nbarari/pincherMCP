package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// #326: When a file is deleted from disk between index runs, the walker
// no longer yields it, so the per-file goroutine that calls
// DeleteSymbolsForFile never fires. Without a tail-pass GC, symbols and
// the file_hash row persist forever — paperclip in the dogfood DB had
// 4820 orphan symbols and 0 files because of exactly this gap.

func TestIndex_DeletedFileOnDisk_GCsSymbolsAndHash(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	idx := New(store)

	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	if err := os.WriteFile(a, []byte("package main\nfunc Alpha(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("package main\nfunc Beta(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// First index: both files indexed.
	r1, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("first Index: %v", err)
	}
	if r1.Symbols == 0 {
		t.Fatalf("first run produced 0 symbols, expected both Alpha+Beta extracted")
	}
	if r1.Deleted != 0 {
		t.Errorf("first run Deleted = %d, want 0 (no prior state)", r1.Deleted)
	}

	files1, err := store.ListFilesForProject(r1.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files1) != 2 {
		t.Fatalf("after first index, ListFilesForProject = %d, want 2; got %v", len(files1), files1)
	}

	// Delete b.go on disk.
	if err := os.Remove(b); err != nil {
		t.Fatalf("rm b.go: %v", err)
	}

	// Second index: walker sees only a.go. b.go's symbols should be GC'd.
	r2, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("second Index: %v", err)
	}
	if r2.Deleted != 1 {
		t.Errorf("second run Deleted = %d, want 1 (b.go was removed from disk)", r2.Deleted)
	}

	// b.go should be gone from files table; a.go should remain.
	files2, err := store.ListFilesForProject(r1.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files2) != 1 {
		t.Errorf("after GC, ListFilesForProject = %d, want 1; got %v", len(files2), files2)
	}
	if len(files2) == 1 && files2[0] != "a.go" {
		t.Errorf("survivor = %q, want a.go", files2[0])
	}

	// Symbols for b.go must be gone; symbols for a.go must still be there.
	bsyms, err := store.GetSymbolsForFile(r1.ProjectID, "b.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(bsyms) != 0 {
		t.Errorf("b.go symbols = %d, want 0 (orphans should have been GC'd)", len(bsyms))
	}
	asyms, err := store.GetSymbolsForFile(r1.ProjectID, "a.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(asyms) == 0 {
		t.Errorf("a.go symbols = 0, want >0 (intact file should not be touched)")
	}
}

// #756: orphan symbols whose `files` row was never written — a crash
// between flushBatch (writes symbols) and SetFileHash (writes the
// files row) — were invisible to the #326 GC, which iterated only the
// `files` table. The GC now unions the `files` table with the distinct
// file paths in `symbols`, so a file_path with symbols but no files
// row is still reconsidered and pruned when it's gone from disk.
func TestIndex_OrphanSymbolsWithoutFilesRow_GCd(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	idx := New(store)

	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.go")
	gone := filepath.Join(dir, "gone.go")
	if err := os.WriteFile(keep, []byte("package main\nfunc Keep(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gone, []byte("package main\nfunc Gone(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r1, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("first Index: %v", err)
	}

	// Simulate the crash-between-flushBatch-and-SetFileHash state:
	// gone.go has symbols but its `files` row is removed. Pre-#756 this
	// made ListFilesForProject blind to it forever.
	if err := store.DeleteFileHash(r1.ProjectID, "gone.go"); err != nil {
		t.Fatalf("DeleteFileHash: %v", err)
	}
	files, err := store.ListFilesForProject(r1.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "gone.go" {
			t.Fatal("precondition failed: gone.go still has a files row")
		}
	}
	// ...but its symbols are still present — that's the orphan state.
	if syms, _ := store.GetSymbolsForFile(r1.ProjectID, "gone.go"); len(syms) == 0 {
		t.Fatal("precondition failed: gone.go should still have orphan symbols")
	}

	// Now remove it from disk too and re-index.
	if err := os.Remove(gone); err != nil {
		t.Fatalf("rm gone.go: %v", err)
	}
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("second Index: %v", err)
	}

	// The orphan symbols must be GC'd via the symbols-table scan.
	gsyms, err := store.GetSymbolsForFile(r1.ProjectID, "gone.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(gsyms) != 0 {
		t.Errorf("gone.go orphan symbols = %d, want 0 (GC must union symbols-table paths, not just the files table)", len(gsyms))
	}
	// keep.go must be untouched.
	if ksyms, _ := store.GetSymbolsForFile(r1.ProjectID, "keep.go"); len(ksyms) == 0 {
		t.Error("keep.go symbols = 0, want >0 (intact file must not be GC'd)")
	}
}

// No-op when nothing was deleted: an idempotent re-index should not
// cause Deleted > 0 even when every file is hash-skipped.
func TestIndex_NoDeletedFiles_DeletedZero(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	idx := New(store)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"),
		[]byte("package main\nfunc Alpha(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("first Index: %v", err)
	}
	r2, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("second Index: %v", err)
	}
	if r2.Deleted != 0 {
		t.Errorf("re-index of unchanged tree had Deleted = %d, want 0", r2.Deleted)
	}
}
