package db

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Targeted coverage tests for thin utility/admin functions that lacked
// direct exercise. Each test below was added to lift a specific function
// from 0–70% to 90–100% — see `go tool cover -func` output as the guide.
// Functional tests live in db_test.go; this file only fills coverage gaps
// that wouldn't naturally fall out of feature work.

// ─────────────────────────────────────────────────────────────────────────────
// percentileIdx: pure function, multiple branches
// ─────────────────────────────────────────────────────────────────────────────

func TestPercentileIdx_AllBranches(t *testing.T) {
	cases := []struct {
		n, p int
		want int
	}{
		{0, 50, 0},      // n <= 1 fast path
		{1, 50, 0},      // n == 1 fast path
		{10, 0, 0},      // p=0 → first element
		{10, 100, 9},    // p=100 → last element (clamped)
		{10, 50, 4},     // p=50 → middle (idx = 50*9/100 = 4)
		{10, 10, 0},     // p=10 → idx = 10*9/100 = 0 via int truncation
		{100, 50, 49},   // p=50 with larger n
		{4, 75, 2},      // p=75 → idx = 75*3/100 = 2
		{2, 90, 0},      // 90 * 1 / 100 = 0 (integer truncation)
	}
	for _, tc := range cases {
		got := percentileIdx(tc.n, tc.p)
		if got != tc.want {
			t.Errorf("percentileIdx(%d, %d) = %d, want %d", tc.n, tc.p, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AvgConfidenceByKind: untested helper used by health diagnostics
// ─────────────────────────────────────────────────────────────────────────────

func TestAvgConfidenceByKind_AggregatesPerKind(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertProject(testProject("avg-proj")); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := s.BulkUpsertSymbols([]Symbol{
		{ID: "a1", ProjectID: "avg-proj", FilePath: "a.go", Name: "F1",
			QualifiedName: "pkg.F1", Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		{ID: "a2", ProjectID: "avg-proj", FilePath: "a.go", Name: "F2",
			QualifiedName: "pkg.F2", Kind: "Function", Language: "Go", ExtractionConfidence: 0.8},
		{ID: "b1", ProjectID: "avg-proj", FilePath: "b.go", Name: "T1",
			QualifiedName: "pkg.T1", Kind: "Type", Language: "Go", ExtractionConfidence: 0.9},
	}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	got, err := s.AvgConfidenceByKind("avg-proj")
	if err != nil {
		t.Fatalf("AvgConfidenceByKind: %v", err)
	}
	// Function avg = (1.0 + 0.8) / 2 = 0.9; Type avg = 0.9
	if v, ok := got["Function"]; !ok || v < 0.89 || v > 0.91 {
		t.Errorf("Function avg = %v, want ~0.9", v)
	}
	if v, ok := got["Type"]; !ok || v < 0.89 || v > 0.91 {
		t.Errorf("Type avg = %v, want ~0.9", v)
	}
}

func TestAvgConfidenceByKind_EmptyProject(t *testing.T) {
	s := newTestStore(t)
	got, err := s.AvgConfidenceByKind("nonexistent")
	if err != nil {
		t.Fatalf("AvgConfidenceByKind: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty project should return empty map, got %v", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ClearExtractionFailures: thin DELETE, but never had direct test
// ─────────────────────────────────────────────────────────────────────────────

func TestClearExtractionFailures_RemovesProjectRows(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertProject(testProject("clr-proj")); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := s.UpsertProject(testProject("other-proj")); err != nil {
		t.Fatalf("UpsertProject other: %v", err)
	}
	// Insert failures for both projects via the public RecordExtractionFailure
	// path so the test exercises the same shape production callers use.
	if err := s.RecordExtractionFailure("clr-proj", "a.go", "Go", "parse_error", "boom"); err != nil {
		t.Fatalf("record (clr): %v", err)
	}
	if err := s.RecordExtractionFailure("other-proj", "x.go", "Go", "parse_error", "boom"); err != nil {
		t.Fatalf("record (other): %v", err)
	}

	if err := s.ClearExtractionFailures("clr-proj"); err != nil {
		t.Fatalf("ClearExtractionFailures: %v", err)
	}

	// clr-proj cleared; other-proj untouched.
	clr, err := s.ListExtractionFailures("clr-proj", 0)
	if err != nil {
		t.Fatalf("ListExtractionFailures (clr): %v", err)
	}
	if len(clr) != 0 {
		t.Errorf("clr-proj should have 0 failures after clear, got %d", len(clr))
	}
	other, err := s.ListExtractionFailures("other-proj", 0)
	if err != nil {
		t.Fatalf("ListExtractionFailures (other): %v", err)
	}
	if len(other) != 1 {
		t.Errorf("other-proj should still have 1 failure, got %d", len(other))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// RebuildFTS: drops + recreates legacy + per-corpus FTS5 indexes; the body
// has both a DROP loop and a backfill, neither of which has a dedicated test.
// ─────────────────────────────────────────────────────────────────────────────

func TestRebuildFTS_BackfillsAfterDrop(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertProject(testProject("fts-proj")); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	// Insert symbols so RebuildFTS has something to backfill.
	if err := s.BulkUpsertSymbols([]Symbol{
		{ID: "f1", ProjectID: "fts-proj", FilePath: "a.go", Name: "Apple",
			QualifiedName: "pkg.Apple", Kind: "Function", Language: "Go"},
		{ID: "f2", ProjectID: "fts-proj", FilePath: "b.go", Name: "Banana",
			QualifiedName: "pkg.Banana", Kind: "Function", Language: "Go"},
	}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	// Pre-rebuild: search should already work via insert triggers.
	preHits, err := s.SearchSymbols("fts-proj", "Apple", "", "", 10)
	if err != nil {
		t.Fatalf("SearchSymbols pre: %v", err)
	}
	if len(preHits) == 0 {
		t.Fatal("expected pre-rebuild search to find Apple")
	}

	rows, err := s.RebuildFTS()
	if err != nil {
		t.Fatalf("RebuildFTS: %v", err)
	}
	if rows != 2 {
		t.Errorf("RebuildFTS reported rows=%d, want 2 (one per symbol in legacy index)", rows)
	}

	// Post-rebuild: same search should still hit, proving the recreated
	// index was actually backfilled (not just dropped).
	postHits, err := s.SearchSymbols("fts-proj", "Banana", "", "", 10)
	if err != nil {
		t.Fatalf("SearchSymbols post: %v", err)
	}
	if len(postHits) == 0 {
		t.Fatal("expected post-rebuild search to find Banana — backfill missing?")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// OpenWithReaders: parameter clamping + custom-pool wiring
// ─────────────────────────────────────────────────────────────────────────────

func TestOpenWithReaders_DefaultZeroUsesDefault(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenWithReaders(dir, 0)
	if err != nil {
		t.Fatalf("OpenWithReaders: %v", err)
	}
	defer s.Close()
	if s.RO() == nil {
		t.Error("reader pool should be initialised with default size")
	}
}

func TestOpenWithReaders_CustomPoolSize(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenWithReaders(dir, 2)
	if err != nil {
		t.Fatalf("OpenWithReaders: %v", err)
	}
	defer s.Close()
	if s.RO() == nil {
		t.Error("reader pool should be initialised at custom size")
	}
}

func TestOpenWithReaders_ClampsHugeValue(t *testing.T) {
	dir := t.TempDir()
	// Above MaxReaderPoolSize (32) — should clamp without error.
	s, err := OpenWithReaders(dir, 9999)
	if err != nil {
		t.Fatalf("OpenWithReaders huge: %v", err)
	}
	defer s.Close()
	if s.RO() == nil {
		t.Error("reader pool should be initialised even with clamped huge value")
	}
}

// SetReaderPoolSize covers the runtime-retune path, including clamp
// branches that are ordinarily hidden behind callers passing tame values.
func TestSetReaderPoolSize_BoundsAndClamps(t *testing.T) {
	s := newTestStore(t)

	// Zero → fall back to default.
	if err := s.SetReaderPoolSize(0); err != nil {
		t.Fatalf("SetReaderPoolSize(0): %v", err)
	}
	// Below min → clamp.
	if err := s.SetReaderPoolSize(-5); err != nil {
		t.Fatalf("SetReaderPoolSize(-5): %v", err)
	}
	// Above max → clamp.
	if err := s.SetReaderPoolSize(9999); err != nil {
		t.Fatalf("SetReaderPoolSize(9999): %v", err)
	}
	// In-range → set.
	if err := s.SetReaderPoolSize(8); err != nil {
		t.Fatalf("SetReaderPoolSize(8): %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// RO fallback: when reader pool is nil, RO() falls back to the writer pool.
// ─────────────────────────────────────────────────────────────────────────────

func TestRO_FallsBackToWriterWhenPoolNil(t *testing.T) {
	s := newTestStore(t)
	// Force the fallback path by nilling the reader pool. Production code
	// never does this, but the fallback exists defensively for the case
	// where attachReaderPool failed during Open.
	saved := s.ro
	s.ro = nil
	defer func() { s.ro = saved }()

	got := s.RO()
	if got == nil {
		t.Fatal("RO fallback returned nil")
	}
	if got != s.db {
		t.Error("RO fallback should return writer pool when reader pool is nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Close: idempotence + post-close reads should fail rather than crash
// ─────────────────────────────────────────────────────────────────────────────

func TestClose_DoubleCloseIsSafe(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	// Second close — sql.DB.Close handles redundant calls. The contract
	// is "safe to call multiple times" (db.go:275).
	_ = s.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// Open: error path when the directory is unwritable
// ─────────────────────────────────────────────────────────────────────────────

func TestOpen_UnwritableDirReturnsError(t *testing.T) {
	// On Windows + Linux, /dev/null/sub or a path under a regular file
	// is unwritable. Use filepath.Join with a non-directory parent.
	bogusParent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := writeFile(bogusParent, "i am a file"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	bogusDir := filepath.Join(bogusParent, "child")
	_, err := Open(bogusDir)
	if err == nil {
		t.Error("expected Open to fail when parent is a file, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DataDir: covers the env-fallback branch on at least one OS
// ─────────────────────────────────────────────────────────────────────────────

func TestDataDir_Returns(t *testing.T) {
	got, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if got == "" {
		t.Error("DataDir returned empty string")
	}
	// Sanity: result should end with the pincherMCP segment.
	if filepath.Base(got) != "pincherMCP" {
		t.Errorf("DataDir result %q should end with pincherMCP segment", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// formatStaleness: covers all four branches
// ─────────────────────────────────────────────────────────────────────────────

// ─────────────────────────────────────────────────────────────────────────────
// DeleteEmptyProjects: covers the empty-DB path AND the actual-deletion loop
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteEmptyProjects_RemovesOnlyEmpty(t *testing.T) {
	s := newTestStore(t)
	// Two projects: one empty (no symbols/edges), one populated.
	full := testProject("full-proj")
	full.SymCount = 1
	if err := s.UpsertProject(full); err != nil {
		t.Fatalf("UpsertProject full: %v", err)
	}
	if err := s.UpsertProject(testProject("empty-proj")); err != nil {
		t.Fatalf("UpsertProject empty: %v", err)
	}
	// Give the full project an actual symbol so its sym_count gets updated
	// by UpdateProjectCounts (called by BulkUpsertSymbols indirectly via
	// the indexer in production; we update directly here).
	if err := s.UpdateProjectCounts("full-proj", 1, 0, 1); err != nil {
		t.Fatalf("UpdateProjectCounts: %v", err)
	}

	deleted, err := s.DeleteEmptyProjects()
	if err != nil {
		t.Fatalf("DeleteEmptyProjects: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (only empty-proj)", deleted)
	}

	// Full project survives.
	got, err := s.GetProject("full-proj")
	if err != nil {
		t.Fatalf("GetProject full: %v", err)
	}
	if got == nil {
		t.Error("full-proj should not have been deleted")
	}
	// Empty project gone.
	got2, err := s.GetProject("empty-proj")
	if err != nil {
		t.Fatalf("GetProject empty: %v", err)
	}
	if got2 != nil {
		t.Error("empty-proj should have been deleted")
	}
}

func TestDeleteEmptyProjects_NoOpOnFreshDB(t *testing.T) {
	s := newTestStore(t)
	deleted, err := s.DeleteEmptyProjects()
	if err != nil {
		t.Fatalf("DeleteEmptyProjects: %v", err)
	}
	if deleted != 0 {
		t.Errorf("fresh DB should delete 0, got %d", deleted)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GraphStats: covers the symbol-iteration AND the edge-iteration branches
// ─────────────────────────────────────────────────────────────────────────────

func TestGraphStats_PopulatedProject(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertProject(testProject("gs-proj")); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := s.BulkUpsertSymbols([]Symbol{
		{ID: "g1", ProjectID: "gs-proj", FilePath: "a.go", Name: "F",
			QualifiedName: "pkg.F", Kind: "Function", Language: "Go"},
		{ID: "g2", ProjectID: "gs-proj", FilePath: "a.go", Name: "T",
			QualifiedName: "pkg.T", Kind: "Type", Language: "Go"},
	}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	if err := s.BulkUpsertEdges([]Edge{
		{FromID: "g1", ToID: "g2", Kind: "USES", ProjectID: "gs-proj"},
	}); err != nil {
		t.Fatalf("BulkUpsertEdges: %v", err)
	}

	symCount, edgeCount, kindCounts, edgeKinds, err := s.GraphStats("gs-proj")
	if err != nil {
		t.Fatalf("GraphStats: %v", err)
	}
	if symCount != 2 {
		t.Errorf("symCount = %d, want 2", symCount)
	}
	if edgeCount != 1 {
		t.Errorf("edgeCount = %d, want 1", edgeCount)
	}
	if kindCounts["Function"] != 1 || kindCounts["Type"] != 1 {
		t.Errorf("kindCounts = %v, want {Function:1, Type:1}", kindCounts)
	}
	if edgeKinds["USES"] != 1 {
		t.Errorf("edgeKinds = %v, want {USES:1}", edgeKinds)
	}
}

func TestFormatStaleness_AllBranches(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{2 * 24 * time.Hour, "2d"},
	}
	for _, tc := range cases {
		got := formatStaleness(tc.d)
		if got != tc.want {
			t.Errorf("formatStaleness(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Local helper — write a file in a temp test, used by Open error test.
// ─────────────────────────────────────────────────────────────────────────────

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
