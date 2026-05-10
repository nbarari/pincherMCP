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
