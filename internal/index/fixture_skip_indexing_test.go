package index

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #996: pre-fix, external projects with large JSON test fixtures
// (warp-fork: 1.45M symbols / 247 edges) inflated the symbols table
// because the indexer extracted every JSON object as a symbol from
// files inside testdata/ / __fixtures__/ etc. isFixturePath existed
// (#750) but was only used by resolution to avoid binding edges into
// fixtures — it was never wired into the per-file gate at index
// time, so the bytes still got read, hashed, and extracted.
//
// The fix wires isFixturePath into the per-file loop in Index().
// Files inside fixture paths are now counted as blocked + skipped
// before any read/extract work. Pinned-corpus snapshots are
// unaffected because they are indexed AS THEIR OWN project root,
// where relPath does not contain "testdata/" (the heuristic checks
// the relative path, not the absolute one).

func TestIndex_FixturePath_NotIndexed(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	// Real project file — must be indexed.
	writeFile(t, dir, "app/run.go", "package app\n\nfunc Run() {}\n")
	// Fixture file — must be skipped.
	writeFile(t, dir, "testdata/golden.json", `{"functions": [{"name": "ghost", "kind": "Function"}]}`)
	writeFile(t, dir, "__fixtures__/sample.go", "package sample\n\nfunc Ghost() {}\n")
	writeFile(t, dir, "fixtures/big.json", `{"a": 1, "b": 2, "c": 3}`)

	res, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if res.Symbols == 0 {
		t.Fatalf("expected at least the real symbol (app.Run); got 0")
	}

	// Real symbol present.
	realID := db.MakeSymbolID("app/run.go", "app.Run", "Function")
	got, err := store.GetSymbol(realID)
	if err != nil || got == nil {
		t.Errorf("expected app.Run to be indexed; got err=%v sym=%v", err, got)
	}

	// Every indexed file_path must NOT contain a fixture directory
	// marker. ListSymbolFilePaths is the lightweight probe.
	paths, err := store.ListSymbolFilePaths(res.Project)
	if err != nil {
		t.Fatalf("ListSymbolFilePaths: %v", err)
	}
	for _, fp := range paths {
		low := strings.ToLower(fp)
		for _, marker := range []string{"testdata/", "__fixtures__/", "fixtures/", "test-fixtures/"} {
			if strings.Contains(low, marker) {
				t.Errorf("file from fixture path leaked into index: %q", fp)
			}
		}
	}

	// `blocked` should account for the skipped fixture files.
	if res.Blocked < 3 {
		t.Errorf("expected blocked ≥ 3 (the 3 fixture files); got %d", res.Blocked)
	}
}

// Control: pinned-corpus indexing (corpus dir as project root) still
// works. relPath does not contain "testdata/" because the corpus IS
// the root, so isFixturePath returns false and the corpus content
// gets indexed.
func TestIndex_CorpusAsRoot_NotSkippedAsFixture(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	// Mirror the shape of testdata/corpus/go-project/* — root-level
	// Go file with NO "testdata/" prefix in its relative path.
	writeFile(t, dir, "internal/auth/auth.go", "package auth\n\nfunc Open() {}\n")
	writeFile(t, dir, "internal/auth/auth_test.go", "package auth\n")

	res, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if res.Symbols == 0 {
		t.Fatal("corpus-as-root indexing must still extract symbols when relPath has no fixture marker")
	}
	openID := db.MakeSymbolID("internal/auth/auth.go", "auth.Open", "Function")
	got, err := store.GetSymbol(openID)
	if err != nil || got == nil {
		t.Errorf("expected auth.Open to be indexed (corpus-as-root path); got err=%v sym=%v", err, got)
	}
}
