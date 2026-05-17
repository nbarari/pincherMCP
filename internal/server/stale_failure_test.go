package server

import (
	"testing"
	"time"
)

// Mirrors `cmd/pinch/stale_failure_test.go` per the bounded-duplication
// convention. The two helpers must stay in lockstep; same behavior
// asserted twice.

func TestServerIsStaleFailure_LastSeenBeforeIndexedAt(t *testing.T) {
	indexedAt := time.Date(2026, 5, 17, 13, 0, 0, 0, time.UTC)
	failureAt := time.Date(2026, 5, 17, 9, 49, 0, 0, time.UTC)
	if !isStaleFailure(failureAt, indexedAt) {
		t.Errorf("failure at %s should be stale relative to indexed_at %s", failureAt, indexedAt)
	}
}

func TestServerIsStaleFailure_LastSeenAfterIndexedAt(t *testing.T) {
	indexedAt := time.Date(2026, 5, 17, 9, 0, 0, 0, time.UTC)
	failureAt := time.Date(2026, 5, 17, 13, 0, 0, 0, time.UTC)
	if isStaleFailure(failureAt, indexedAt) {
		t.Errorf("failure at %s should NOT be stale relative to indexed_at %s", failureAt, indexedAt)
	}
}

func TestServerIsStaleFailure_IndexedAtZero(t *testing.T) {
	failureAt := time.Date(2026, 5, 17, 9, 49, 0, 0, time.UTC)
	if isStaleFailure(failureAt, time.Time{}) {
		t.Errorf("failure with zero indexed_at should NOT be tagged stale")
	}
}

func TestServerIsStaleFailure_ExactlyEqual(t *testing.T) {
	t1 := time.Date(2026, 5, 17, 13, 0, 0, 0, time.UTC)
	if isStaleFailure(t1, t1) {
		t.Errorf("failure last_seen_at exactly equal to indexed_at should NOT be tagged stale")
	}
}
