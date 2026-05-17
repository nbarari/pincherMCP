package server

import (
	"context"
	"testing"
	"time"
)

// #1382: doctor's extraction_failures rows carry an `is_stale` boolean
// when last_seen_at predates the project's indexed_at — meaning the row
// was recorded in a prior pass but the most-recent re-extraction did NOT
// re-record it. The row will clear on the project's next index pass via
// PruneExtractionFailuresForFile (#1319). Pre-fix the operator couldn't
// distinguish "failing now" from "awaiting re-index" — high alarm-fatigue
// in the doctor display.

// TestHandleDoctor_FailureStaleTag_StaleRowMarked seeds a failure with
// last_seen_at older than the project's indexed_at, then asserts the
// doctor payload carries is_stale=true on that row.
func TestHandleDoctor_FailureStaleTag_StaleRowMarked(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-stale", "/tmp/p-stale", "stale-proj")

	// Bump indexed_at to "now" so it's well after the failure we're
	// about to seed. mustUpsertProject already sets time.Now(); back-
	// date the failure instead.
	if err := store.RecordExtractionFailure("p-stale", "stale.go", "Go", "parse_error", "old"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Back-date failure last_seen_at to 1h before now — still inside
	// the default 168h lookback so it surfaces, but before project's
	// indexed_at (which is mustUpsertProject's time.Now()).
	pastUnix := time.Now().Add(-1 * time.Hour).Unix()
	if _, err := store.DB().Exec(
		`UPDATE extraction_failures SET last_seen_at = ? WHERE file_path = 'stale.go'`,
		pastUnix,
	); err != nil {
		t.Fatalf("back-date: %v", err)
	}

	result, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, result)
	failures, _ := body["extraction_failures"].([]any)
	if len(failures) != 1 {
		t.Fatalf("want 1 failure, got %d", len(failures))
	}
	m, _ := failures[0].(map[string]any)
	isStale, ok := m["is_stale"].(bool)
	if !ok {
		t.Fatalf("is_stale must be a JSON boolean; got %T %v", m["is_stale"], m["is_stale"])
	}
	if !isStale {
		t.Errorf("expected is_stale=true on row whose last_seen_at predates project's indexed_at; got false")
	}
}

// TestHandleDoctor_FailureStaleTag_FreshRowOmitsTag — when the failure
// was recorded AFTER the project's indexed_at (the indexer ran, saw the
// failure, recorded it), is_stale is omitted from JSON (omitempty on
// false). The row reflects a real current failure.
func TestHandleDoctor_FailureStaleTag_FreshRowOmitsTag(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-fresh", "/tmp/p-fresh", "fresh-proj")

	// Back-date project's indexed_at to 2h before now, then seed a
	// failure at "now" — the failure post-dates the index pass.
	pastUnix := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := store.DB().Exec(
		`UPDATE projects SET indexed_at = ? WHERE id = 'p-fresh'`,
		pastUnix,
	); err != nil {
		t.Fatalf("back-date project: %v", err)
	}
	if err := store.RecordExtractionFailure("p-fresh", "fresh.go", "Go", "parse_error", "new"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	result, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, result)
	failures, _ := body["extraction_failures"].([]any)
	if len(failures) != 1 {
		t.Fatalf("want 1 failure, got %d", len(failures))
	}
	m, _ := failures[0].(map[string]any)
	// omitempty on the Go side means false-or-absent. Both are
	// acceptable; JSON-unaware consumers see "not stale" either way.
	if v, present := m["is_stale"]; present {
		if b, _ := v.(bool); b {
			t.Errorf("fresh failure should NOT be tagged stale; got is_stale=true")
		}
	}
}
