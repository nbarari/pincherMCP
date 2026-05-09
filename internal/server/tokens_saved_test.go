package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// metaTokensSaved extracts the integer tokens_saved from a tool
// result's _meta envelope. Returns -1 when the field is missing so
// tests can distinguish "not set" from "set to zero".
func metaTokensSaved(t *testing.T, m map[string]any) int {
	t.Helper()
	meta, ok := m["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("response missing _meta envelope: %v", m)
	}
	v, ok := meta["tokens_saved"]
	if !ok {
		return -1
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("tokens_saved not a number: %v", v)
	}
	return int(f)
}

// TestArchitecture_TokensSavedIsZero pins #219: the architecture tool
// returns a metadata-only response (counts, histograms, hotspot names),
// so the savings baseline should be 0 — there is no file-read
// alternative an agent would have used instead. The prior formula
// `savedVsFullRead(symCount, …)` claimed `symCount × avgFileSize/4`
// per call, which over-claimed by 4-6 orders of magnitude on real
// corpora and dominated the cumulative session counter.
func TestArchitecture_TokensSavedIsZero(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now(), FileCount: 5, SymCount: 1000})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "a.go", Name: "Foo", QualifiedName: "pkg.Foo",
			Kind: "Function", Language: "Go", StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5},
	})

	result, err := srv.handleArchitecture(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleArchitecture: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", decode(t, result))
	}
	got := metaTokensSaved(t, decode(t, result))
	if got != 0 {
		t.Errorf("architecture tokens_saved=%d, want 0 (#219 — metadata-only tool, no file-read baseline)", got)
	}
}

// TestSymbolsBatch_UsesRealFileSizes pins #220: the symbols batch tool
// must use real file sizes (via savedVsFileSizes) rather than the
// avgFileSize=20000 constant. We seed two physical files with known
// sizes and assert the savings reflect those, not 2 × 20000 / 4.
func TestSymbolsBatch_UsesRealFileSizes(t *testing.T) {
	srv, store, _ := newTestServer(t)
	root := t.TempDir()
	srv.sessionRoot = root

	// Tiny config-ish files: 200 bytes each. Under the old constant a
	// 2-ID batch would have claimed 2 × 20000/4 = 10000 tokens saved
	// minus the response payload. With real sizes it claims 2 × 200/4
	// = 100 tokens saved minus the response payload (likely 0 after
	// max-with-zero clamp).
	for _, name := range []string{"a.yml", "b.yml"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(strings.Repeat("k: v\n", 40)), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	store.UpsertProject(db.Project{ID: "p1", Path: root, Name: "p1", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "a.yml", Name: "Foo", QualifiedName: "a.Foo",
			Kind: "Setting", Language: "YAML", StartByte: 0, EndByte: 5, StartLine: 1, EndLine: 1},
		{ID: "s2", ProjectID: "p1", FilePath: "b.yml", Name: "Bar", QualifiedName: "b.Bar",
			Kind: "Setting", Language: "YAML", StartByte: 0, EndByte: 5, StartLine: 1, EndLine: 1},
	})

	result, err := srv.handleSymbols(context.Background(), makeReq(map[string]any{
		"ids": []any{"s1", "s2"},
	}))
	if err != nil {
		t.Fatalf("handleSymbols: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", decode(t, result))
	}
	got := metaTokensSaved(t, decode(t, result))
	// Old behaviour: 2 × 20000 / 4 = 10000-ish (minus payload).
	// New behaviour: 2 × 200 / 4 = 100 (minus payload, max-clamped).
	// Using a generous upper bound: anything >1000 means the constant
	// is still being applied.
	if got > 1000 {
		t.Errorf("symbols batch tokens_saved=%d, want ≤1000 (#220 — should use real os.Stat sizes for two 200-byte files, not the 20000-byte constant)", got)
	}
}

// TestSymbolsBatch_DedupsByFilePath pins the dedup half of #220: a
// batch hitting N IDs from M unique files (M < N) should attribute
// the sum of M file sizes, not N × per-file-estimate. Six IDs
// against two unique files should claim ~2× a single file's worth of
// savings, not ~6×.
func TestSymbolsBatch_DedupsByFilePath(t *testing.T) {
	srv, store, _ := newTestServer(t)
	root := t.TempDir()
	srv.sessionRoot = root

	// Two physical files, ~4000 bytes each.
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(strings.Repeat("// comment line padding to size\n", 125)), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	store.UpsertProject(db.Project{ID: "p1", Path: root, Name: "p1", IndexedAt: time.Now()})
	// Six symbols spread across the two files (3 + 3).
	syms := []db.Symbol{}
	for i := 0; i < 3; i++ {
		syms = append(syms, db.Symbol{
			ID: "a" + string(rune('0'+i)), ProjectID: "p1", FilePath: "a.go",
			Name: "fa" + string(rune('0'+i)), QualifiedName: "a.fa" + string(rune('0'+i)),
			Kind: "Function", Language: "Go",
			StartByte: i * 100, EndByte: i*100 + 50, StartLine: 1, EndLine: 1,
		})
		syms = append(syms, db.Symbol{
			ID: "b" + string(rune('0'+i)), ProjectID: "p1", FilePath: "b.go",
			Name: "fb" + string(rune('0'+i)), QualifiedName: "b.fb" + string(rune('0'+i)),
			Kind: "Function", Language: "Go",
			StartByte: i * 100, EndByte: i*100 + 50, StartLine: 1, EndLine: 1,
		})
	}
	store.BulkUpsertSymbols(syms)

	ids := []any{"a0", "a1", "a2", "b0", "b1", "b2"}
	result, err := srv.handleSymbols(context.Background(), makeReq(map[string]any{"ids": ids}))
	if err != nil {
		t.Fatalf("handleSymbols: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", decode(t, result))
	}
	got := metaTokensSaved(t, decode(t, result))
	// Per-file size ≈ 4000 bytes / 4 chars-per-token ≈ 1000 tokens.
	// Two unique files dedup'd: ≈2000 token baseline, minus payload.
	// If we DID NOT dedup: ≈6000 token baseline.
	// Generous bound: anything > 3500 means dedup didn't happen.
	if got > 3500 {
		t.Errorf("symbols batch tokens_saved=%d for 6 IDs / 2 unique files, want ≤3500 (dedup should attribute 2 file sizes, not 6)", got)
	}
}

// TestSymbolsBatch_DocumentSymbolsSkipped covers the Document-kind
// branch of the savings path: a Document symbol stores its body in
// Docstring (no on-disk file), so it's excluded from the file-size
// baseline rather than os.Stat-ing a path that doesn't exist.
func TestSymbolsBatch_DocumentSymbolsSkipped(t *testing.T) {
	srv, store, _ := newTestServer(t)
	root := t.TempDir()
	srv.sessionRoot = root

	store.UpsertProject(db.Project{ID: "p1", Path: root, Name: "p1", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "doc1", ProjectID: "p1", FilePath: "https://example.com/api", Name: "API",
			QualifiedName: "api", Kind: "Document", Language: "html",
			Docstring: "fetched body content", StartByte: 0, EndByte: 0},
	})

	result, err := srv.handleSymbols(context.Background(), makeReq(map[string]any{
		"ids": []any{"doc1"},
	}))
	if err != nil {
		t.Fatalf("handleSymbols: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", decode(t, result))
	}
	// Just assert it doesn't crash and returns ≤ a small number — Document
	// symbols have no on-disk file to stat, so they shouldn't contribute
	// to the file-size baseline.
	got := metaTokensSaved(t, decode(t, result))
	if got > 100 {
		t.Errorf("Document-only batch tokens_saved=%d, want small (no file-read baseline applies)", got)
	}
}
