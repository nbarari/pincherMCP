package main

import (
	"testing"
	"time"
)

// TestIsStaleFailure_LastSeenBeforeIndexedAt pins the canonical case:
// a failure recorded BEFORE the most-recent re-index is awaiting that
// indexer pass to either re-record or implicitly clear it via #1319.
// The doctor display tags the row so the operator can ignore it
// pending re-extraction.
func TestIsStaleFailure_LastSeenBeforeIndexedAt(t *testing.T) {
	indexedAt := time.Date(2026, 5, 17, 13, 0, 0, 0, time.UTC)
	failureAt := time.Date(2026, 5, 17, 9, 49, 0, 0, time.UTC) // 4h before re-index
	if !isStaleFailure(failureAt, indexedAt) {
		t.Errorf("failure at %s should be stale relative to indexed_at %s", failureAt, indexedAt)
	}
}

// TestIsStaleFailure_LastSeenAfterIndexedAt — failure recorded during
// or after the most-recent re-index is current; the row reflects a
// real failure the indexer hasn't yet superseded.
func TestIsStaleFailure_LastSeenAfterIndexedAt(t *testing.T) {
	indexedAt := time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC)
	failureAt := time.Date(2026, 5, 17, 13, 0, 0, 0, time.UTC) // 4h after re-index
	if isStaleFailure(failureAt, indexedAt) {
		t.Errorf("failure at %s should NOT be stale relative to indexed_at %s", failureAt, indexedAt)
	}
}

// TestIsStaleFailure_IndexedAtZero — when indexed_at is the zero time
// (project record is missing the column for any reason), don't
// false-positive: assume the row is current and leave it un-tagged.
func TestIsStaleFailure_IndexedAtZero(t *testing.T) {
	failureAt := time.Date(2026, 5, 17, 9, 49, 0, 0, time.UTC)
	if isStaleFailure(failureAt, time.Time{}) {
		t.Errorf("failure with zero indexed_at should NOT be tagged stale")
	}
}

// TestIsStaleFailure_ExactlyEqual — failure last_seen_at exactly
// matches indexed_at. The re-extraction observed the failure right at
// indexing time, so it's current, not stale. (Before-relation is
// strict; equal counts as current.)
func TestIsStaleFailure_ExactlyEqual(t *testing.T) {
	t1 := time.Date(2026, 5, 17, 13, 0, 0, 0, time.UTC)
	if isStaleFailure(t1, t1) {
		t.Errorf("failure last_seen_at exactly equal to indexed_at should NOT be tagged stale")
	}
}
