package db

import (
	"testing"
)

// #1663 v0.87: dogfood-found threshold tuning for the FTS5
// fragmentation advisory (#1612). On a 27-project install
// immediately after `rebuild_fts`, the config corpus settled at
// 10.4x — above the original 10x threshold. The advisory fired
// every time doctor was inspected, training the user to ignore it.
// Threshold raised to 25x to clear the observed post-rebuild floor
// while still catching real degradation.
//
// These tests pin the threshold's boundary behavior so a future
// "let's tighten this back down" change can't silently re-introduce
// the false-positive class.

func TestFTS5FragmentationThreshold_BelowFloor_NoRebuildFlag_1663(t *testing.T) {
	store := newTestStore(t)
	rows, err := store.FTS5Fragmentation()
	if err != nil {
		t.Fatalf("FTS5Fragmentation: %v", err)
	}
	// Empty DB: every corpus has ratio=0, must not flag NeedsRebuild.
	for _, r := range rows {
		if r.NeedsRebuild {
			t.Errorf("empty DB %s corpus flagged NeedsRebuild — threshold (%v) wrongly applies to zero-ratio rows",
				r.Corpus, FTS5FragmentationThreshold)
		}
	}
}

// Pin the threshold value directly so a re-tightening change must
// be explicit and ride a new issue number, not a silent constant flip.
func TestFTS5FragmentationThreshold_ValueClearsPostRebuildFloor_1663(t *testing.T) {
	// Observed post-rebuild config corpus ratio on a real install
	// with 27 indexed projects: 10.4x. The threshold MUST sit above
	// that to avoid false-positiving on every doctor invocation.
	const observedPostRebuildFloor = 10.4
	if FTS5FragmentationThreshold <= observedPostRebuildFloor {
		t.Fatalf("FTS5FragmentationThreshold (%v) <= observed post-rebuild floor (%vx) — "+
			"advisory will false-positive on every doctor inspection (regression of #1663)",
			FTS5FragmentationThreshold, observedPostRebuildFloor)
	}
	// Real degradation case from the pre-rebuild dogfood machine: 62.7x.
	// The threshold MUST stay below that so we still catch genuine
	// fragmentation.
	const observedFraggedCase = 62.7
	if FTS5FragmentationThreshold >= observedFraggedCase {
		t.Fatalf("FTS5FragmentationThreshold (%v) >= observed fragged case (%vx) — "+
			"advisory will miss the real degradation it was built to surface (regression of #1612)",
			FTS5FragmentationThreshold, observedFraggedCase)
	}
}
