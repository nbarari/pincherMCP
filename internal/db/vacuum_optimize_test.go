package db

import (
	"testing"
)

// #1219: pincher vacuum now runs the full 4-step reclaim flow:
//   1. VACUUM (load-bearing — reclaims freed pages, #732)
//   2. PRAGMA wal_checkpoint(TRUNCATE) (load-bearing — shrinks on-disk file, #732)
//   3. PRAGMA optimize (advisory — re-analyze stats so subsequent query plans are fresh post-rewrite)
//   4. INSERT INTO symbols_{code,config,docs}_fts(...) VALUES('optimize') (advisory — compact FTS5 segments)
//
// Steps 3-4 are advisory: failures populate VacuumResult.OptimizeError /
// FTSOptimizeError so the CLI can surface them, but the run still
// reports vacuumed=true if (1) and (2) landed.

// Positive: healthy DB vacuums clean, both new error fields stay empty.
// Pre-#1219 only steps 1-2 ran; this test pins that all 4 succeed on a
// real DB with all three FTS5 vtabs populated.
func TestVacuum_OptimizeSteps_HealthyDBHasNoErrors(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	// Seed at least one symbol so the FTS5 vtabs are non-empty —
	// 'optimize' on a zero-segment vtab is a no-op that wouldn't
	// exercise the merge path. One symbol per corpus would be ideal;
	// a single Go Function lands in symbols_code_fts.
	if err := s.UpsertProject(testProject("vac-opt")); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	id := makeSymID(t, "vac-opt", "x.go", "Fn", "Function")
	if err := s.BulkUpsertSymbols([]Symbol{testSymbol(id, "Fn", "Function", "vac-opt", "x.go")}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	res, err := s.Vacuum()
	if err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	if res.OptimizeError != "" {
		t.Errorf("PRAGMA optimize errored on a healthy DB: %q — step 3 should always succeed when steps 1-2 did", res.OptimizeError)
	}
	if res.FTSOptimizeError != "" {
		t.Errorf("FTS5 'optimize' errored on a healthy DB: %q — step 4 should always succeed when the vtabs exist (they're created by migrations)", res.FTSOptimizeError)
	}
}

// Cross-check: VacuumResult exposes the two new advisory fields with
// the documented string type. Renames would silently break the CLI's
// JSON receipt without this gate (same shape as the #1149
// TestVacuumResult_BusyFieldShape gate above).
func TestVacuumResult_OptimizeFieldShapes(t *testing.T) {
	var r VacuumResult
	r.OptimizeError = "test-message"
	r.FTSOptimizeError = "test-message-fts"
	if r.OptimizeError != "test-message" {
		t.Errorf("VacuumResult.OptimizeError didn't round-trip — field rename?")
	}
	if r.FTSOptimizeError != "test-message-fts" {
		t.Errorf("VacuumResult.FTSOptimizeError didn't round-trip — field rename?")
	}
}

// Control: the load-bearing VACUUM + checkpoint must still reclaim
// space when steps 3-4 have nothing meaningful to do (empty DB). Same
// shape as TestVacuum_HappyPath_ReclaimsAndReportsNoBusy but anchored
// on the post-#1219 4-step flow rather than the 2-step ancestor —
// regression check that adding optimize calls didn't gate the
// reclaim on optimize succeeding.
func TestVacuum_AdvisorySteps_DontGateReclaim(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	if err := s.UpsertProject(testProject("vac-gate")); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	syms := make([]Symbol, 0, 200)
	for i := 0; i < 200; i++ {
		id := makeSymID(t, "vac-gate", "x.go", "sym"+itoa(i), "Function")
		syms = append(syms, testSymbol(id, "sym"+itoa(i), "Function", "vac-gate", "x.go"))
	}
	if err := s.BulkUpsertSymbols(syms); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	if err := s.CheckpointTruncate(); err != nil {
		t.Fatalf("CheckpointTruncate: %v", err)
	}
	grown := dbFileSizeForTest(t, s.Path)
	if err := s.DeleteProject("vac-gate"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	res, err := s.Vacuum()
	if err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	shrunk := dbFileSizeForTest(t, s.Path)
	if shrunk >= grown {
		t.Errorf("post-#1219 Vacuum did not reclaim space (grown=%d, shrunk=%d) — advisory steps 3-4 must not gate the load-bearing reclaim", grown, shrunk)
	}
	// OptimizeError / FTSOptimizeError are advisory — present for
	// the CLI to surface. They must not have prevented the reclaim
	// above. If they're populated AND reclaim didn't happen, the
	// test above already failed; if reclaim happened, the run is
	// healthy regardless.
	_ = res
}
