package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #310: Index() with a missing path should fail fast with a clear
// "does not exist" error instead of silently returning a zero-result
// summary that looks like "no indexable code in this repo".

func TestIndex_MissingPath_FailsWithClearError(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	idx := New(store)

	missing := filepath.Join(t.TempDir(), "definitely-not-here-310")
	_, err := idx.Index(context.Background(), missing, false)
	if err == nil {
		t.Fatal("expected error for missing path; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "does not exist") {
		t.Errorf("error %q must explicitly say the path does not exist", msg)
	}
	if !strings.Contains(msg, "definitely-not-here-310") {
		t.Errorf("error %q should include the offending path so the user can spot the typo", msg)
	}
}

// Index() against a real file (not a directory) should also fail
// clearly. Catches the case where someone passes a path to a single
// file rather than the project root.
func TestIndex_PathIsFile_FailsClearly(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	idx := New(store)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(filePath, []byte("package main\nfunc main(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := idx.Index(context.Background(), filePath, false)
	if err == nil {
		t.Fatal("expected error for file (not directory) path; got nil")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error %q must say the path isn't a directory", err.Error())
	}
}

// Existing directory still indexes successfully — the new validation
// must not regress the happy path.
func TestIndex_ExistingDir_StillIndexes(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	idx := New(store)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\nfunc main(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("expected success on real dir; got %v", err)
	}
	if result == nil {
		t.Fatal("nil result on success")
	}
}

// newTestStore is a small helper to keep this test file self-
// sufficient without coupling to any particular existing fixture.
func newTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	return store
}
