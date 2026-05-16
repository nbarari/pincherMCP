package server

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func nowMinusDays(d int) int64 {
	return time.Now().Add(-time.Duration(d) * 24 * time.Hour).Unix()
}

// #1205: pre-fix `handleDoctor` looped `ListExtractionFailures(p.ID, top)`
// per project. On a 130-project install the N round-trips dominated
// latency (~60s on an 11GB DB). The fix collapses the loop into one
// cross-project SELECT + one COUNT for the honest truncation tally.

// Positive: failures from multiple projects round-trip with the right
// project_name join (the in-memory map lookup replaces the per-project
// SELECT's implicit name binding).
func TestHandleDoctor_FailuresAcrossMultipleProjects(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-alpha", "/tmp/p-alpha", "alpha")
	mustUpsertProject(t, store, "p-beta", "/tmp/p-beta", "beta")
	mustUpsertProject(t, store, "p-gamma", "/tmp/p-gamma", "gamma")

	// One failure per project — verifies the join hits every project.
	if err := store.RecordExtractionFailure("p-alpha", "a.go", "Go", "parse_error", "a-detail"); err != nil {
		t.Fatalf("seed alpha: %v", err)
	}
	if err := store.RecordExtractionFailure("p-beta", "b.go", "Go", "parse_error", "b-detail"); err != nil {
		t.Fatalf("seed beta: %v", err)
	}
	if err := store.RecordExtractionFailure("p-gamma", "c.go", "Go", "parse_error", "c-detail"); err != nil {
		t.Fatalf("seed gamma: %v", err)
	}

	result, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, result)
	failures, _ := body["extraction_failures"].([]any)
	if len(failures) != 3 {
		t.Fatalf("want 3 failures across 3 projects, got %d", len(failures))
	}
	gotProjects := map[string]bool{}
	for _, f := range failures {
		m, _ := f.(map[string]any)
		if p, _ := m["project"].(string); p != "" {
			gotProjects[p] = true
		}
	}
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !gotProjects[want] {
			t.Errorf("missing project_name %q in failures payload; got projects=%v", want, gotProjects)
		}
	}
}

// Positive: when more failures exist than `top`, doctor caps the list
// AND reports an honest extraction_failures_truncated count via the
// COUNT(*) second query (not the pre-fix per-project running sum).
func TestHandleDoctor_FailuresTruncationCountIsHonest(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-noisy", "/tmp/p-noisy", "noisy")

	const total = 75 // > default top=10
	for i := 0; i < total; i++ {
		fp := fmt.Sprintf("f%03d.go", i)
		if err := store.RecordExtractionFailure("p-noisy", fp, "Go", "parse_error", "detail"); err != nil {
			t.Fatalf("seed failure %d: %v", i, err)
		}
	}

	result, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{"top": 10}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, result)
	failures, _ := body["extraction_failures"].([]any)
	if len(failures) != 10 {
		t.Fatalf("want 10 failures (capped at top), got %d", len(failures))
	}
	// extraction_failures_truncated must be the exact COUNT-derived
	// remainder: 75 total - 10 returned = 65 hidden.
	tr, ok := body["extraction_failures_truncated"]
	if !ok {
		t.Fatal("extraction_failures_truncated missing — should be present when len(failures) >= top and more exist")
	}
	got, ok := tr.(float64)
	if !ok {
		t.Fatalf("extraction_failures_truncated must be numeric; got %T %v", tr, tr)
	}
	if int(got) != 65 {
		t.Errorf("extraction_failures_truncated = %d; want 65 (75 total - 10 returned)", int(got))
	}
}

// Control: cutoff filter applied in SQL, not Go. Failures older than
// the lookback_hours window must be excluded. Pre-fix the filter ran
// in Go after the per-project SELECT pulled everything; post-fix the
// `WHERE last_seen_at >= ?` clause does the work.
func TestHandleDoctor_FailuresCutoffFilterApplied(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-cutoff", "/tmp/p-cutoff", "cutoff")

	// Fresh row — falls inside any positive lookback window.
	if err := store.RecordExtractionFailure("p-cutoff", "fresh.go", "Go", "parse_error", "fresh"); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}

	// Stale row — record then back-date its last_seen_at well past
	// the default 168h lookback so the cutoff must filter it.
	if err := store.RecordExtractionFailure("p-cutoff", "stale.go", "Go", "parse_error", "stale"); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	// Back-date stale.go to 365 days ago.
	if _, err := store.DB().Exec(
		`UPDATE extraction_failures SET last_seen_at = ? WHERE file_path = 'stale.go'`,
		nowMinusDays(365),
	); err != nil {
		t.Fatalf("back-date stale: %v", err)
	}

	result, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{"lookback_hours": 168}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, result)
	failures, _ := body["extraction_failures"].([]any)
	if len(failures) != 1 {
		t.Fatalf("want 1 fresh failure (stale excluded by cutoff), got %d", len(failures))
	}
	m, _ := failures[0].(map[string]any)
	if file, _ := m["file"].(string); file != "fresh.go" {
		t.Errorf("expected fresh.go, got %q", file)
	}
}

// Cross-check: empty failures table → empty slice (#328 invariant —
// nil slice marshals to null and breaks dashboard consumers). Verifies
// the refactor didn't drop the explicit `[]failureRow{}` init.
func TestHandleDoctor_EmptyFailuresIsEmptySliceNotNil(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-clean", "/tmp/p-clean", "clean")
	// No RecordExtractionFailure calls — table is empty for this project.

	result, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, result)
	failures, ok := body["extraction_failures"].([]any)
	if !ok {
		t.Fatalf("extraction_failures must marshal as JSON array; got %T %v", body["extraction_failures"], body["extraction_failures"])
	}
	if len(failures) != 0 {
		t.Errorf("want empty failures slice, got %d entries", len(failures))
	}
	// And truncated must be absent — there's nothing to truncate.
	if _, present := body["extraction_failures_truncated"]; present {
		t.Errorf("extraction_failures_truncated should be omitted when failures is empty; got %v", body["extraction_failures_truncated"])
	}
}
