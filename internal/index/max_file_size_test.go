package index

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pincherMCP/pincher/internal/db"
)

// Per-file size cap (#111) — bounds memory during indexing so a single
// pathological input (large auto-generated JSON, captured GraphQL dump,
// OpenAPI spec) cannot stall the indexer or exhaust process memory.

// TestIndex_MaxFileSize_RejectsOversized verifies the happy gate: a file
// over the configured cap is recorded as `file_too_large` and produces no
// symbols, while a file under the cap indexes normally.
func TestIndex_MaxFileSize_RejectsOversized(t *testing.T) {
	idx, store := newTestIndexer(t)
	idx.SetMaxFileSize(64 * 1024) // 64 KB cap for fast test

	dir := t.TempDir()

	// Under-cap Go file: should index.
	writeFile(t, dir, "small.go", "package demo\nfunc Small() {}\n")

	// Over-cap JSON file: should be refused without read.
	big := strings.Repeat(`{"k":"`+strings.Repeat("x", 200)+`"},`, 1024) // ~210 KB
	writeFile(t, dir, "big.json", "["+big+`{"end":1}]`)

	res, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	pid := db.ProjectIDFromPath(dir)

	// The over-cap file must show up as a file_too_large failure.
	failures, err := store.ListExtractionFailures(pid, 0)
	if err != nil {
		t.Fatalf("ListExtractionFailures: %v", err)
	}
	var saw bool
	for _, f := range failures {
		if f.Reason == "file_too_large" && filepath.Base(f.FilePath) == "big.json" {
			saw = true
			if !strings.Contains(f.Details, "size=") || !strings.Contains(f.Details, "cap=") {
				t.Errorf("file_too_large details missing size/cap: %q", f.Details)
			}
			break
		}
	}
	if !saw {
		t.Fatalf("expected file_too_large failure for big.json; got %d failures: %+v", len(failures), failures)
	}

	// Blocked counter should reflect the refused file.
	if res.Blocked < 1 {
		t.Errorf("Blocked=%d, want >= 1 (big.json)", res.Blocked)
	}

	// The under-cap Go file must still produce symbols (cap doesn't break
	// healthy indexing).
	if res.Symbols == 0 {
		t.Errorf("Symbols=0, expected at least one from small.go")
	}
}

// TestIndex_MaxFileSize_ZeroDisables verifies the escape hatch: setting the
// cap to 0 (or negative) disables the size check entirely. This matters for
// users with legitimate large generated files who want to opt out.
func TestIndex_MaxFileSize_ZeroDisables(t *testing.T) {
	idx, store := newTestIndexer(t)
	idx.SetMaxFileSize(0) // disable

	dir := t.TempDir()
	// 200 KB JSON — would be rejected at the 64 KB cap above, but should
	// pass through here.
	big := strings.Repeat(`{"k":"`+strings.Repeat("x", 200)+`"},`, 1024)
	writeFile(t, dir, "big.json", "["+big+`{"end":1}]`)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	pid := db.ProjectIDFromPath(dir)
	failures, _ := store.ListExtractionFailures(pid, 0)
	for _, f := range failures {
		if f.Reason == "file_too_large" {
			t.Errorf("cap=0 should disable the check, but recorded file_too_large for %s", f.FilePath)
		}
	}
}

// TestIndex_MaxFileSize_DefaultUnchanged pins the default value so a future
// PR that touches `DefaultMaxFileSize` triggers a deliberate review. 4 MB
// is the documented baseline; bumping it past 16 MB without intent would
// re-introduce the #111 hang risk.
func TestIndex_MaxFileSize_DefaultUnchanged(t *testing.T) {
	if got, want := DefaultMaxFileSize, int64(4*1024*1024); got != want {
		t.Errorf("DefaultMaxFileSize = %d, want %d (4 MB) — bumping this re-opens #111 if not intentional", got, want)
	}
	idx, _ := newTestIndexer(t)
	if got := idx.MaxFileSize(); got != DefaultMaxFileSize {
		t.Errorf("New() should default to DefaultMaxFileSize, got %d", got)
	}
}
