package db

import (
	"testing"
	"time"
)

// TestRecordBenchRun_RoundTrip exercises the full persistence path:
// insert one run plus per-tool results, list back via ListBenchRuns
// (project-scoped + global), fetch per-run results via GetBenchResults.
// Pins the v29 schema's column ordering and the transaction wrapping.
func TestRecordBenchRun_RoundTrip(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// FK requires the project row to exist first.
	if err := store.UpsertProject(Project{
		ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	run := BenchRun{
		RunID:         "run-1",
		ProjectID:     "p1",
		StartedAt:     time.Unix(1000, 0),
		NSamples:      10,
		TraceDepth:    2,
		BinaryVersion: "0.68.0-test",
	}
	results := []BenchResult{
		{RunID: "run-1", ToolName: "search", Calls: 10, P50LatencyMs: 1.5, P95LatencyMs: 3.0, MeanLatencyMs: 1.8, MeanTokensActual: 100, MeanTokensBaseline: 1000, SavingsPct: 90.0},
		{RunID: "run-1", ToolName: "context", Calls: 10, P50LatencyMs: 0.5, P95LatencyMs: 1.2, MeanLatencyMs: 0.7, MeanTokensActual: 50, MeanTokensBaseline: 800, SavingsPct: 93.75},
		{RunID: "run-1", ToolName: "trace", Calls: 10, P50LatencyMs: 4.0, P95LatencyMs: 10.0, MeanLatencyMs: 5.0, MeanTokensActual: 500, MeanTokensBaseline: 5000, SavingsPct: 90.0},
	}
	if err := store.RecordBenchRun(run, results); err != nil {
		t.Fatalf("RecordBenchRun: %v", err)
	}

	// Project-scoped listing must surface the one row.
	runs, err := store.ListBenchRuns("p1", 10)
	if err != nil {
		t.Fatalf("ListBenchRuns(p1): %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListBenchRuns(p1) = %d rows, want 1", len(runs))
	}
	if runs[0].RunID != "run-1" {
		t.Errorf("run_id = %q, want run-1", runs[0].RunID)
	}
	if runs[0].NSamples != 10 {
		t.Errorf("n_samples = %d, want 10", runs[0].NSamples)
	}
	if runs[0].BinaryVersion != "0.68.0-test" {
		t.Errorf("binary_version = %q, want '0.68.0-test'", runs[0].BinaryVersion)
	}

	// Global listing (empty projectID) must also surface the row.
	allRuns, err := store.ListBenchRuns("", 10)
	if err != nil {
		t.Fatalf("ListBenchRuns(\"\"): %v", err)
	}
	if len(allRuns) != 1 {
		t.Errorf("ListBenchRuns(\"\") = %d rows, want 1", len(allRuns))
	}

	// Per-tool results, ordered by tool_name ASC (context/search/trace).
	got, err := store.GetBenchResults("run-1")
	if err != nil {
		t.Fatalf("GetBenchResults: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("GetBenchResults = %d rows, want 3", len(got))
	}
	wantOrder := []string{"context", "search", "trace"}
	for i, w := range wantOrder {
		if got[i].ToolName != w {
			t.Errorf("got[%d].tool_name = %q, want %q", i, got[i].ToolName, w)
		}
	}
	// Pin the savings_pct round-trip — the bench claim is the headline
	// number, and a column-ordering regression would silently flip it.
	for _, r := range got {
		if r.ToolName == "context" && r.SavingsPct != 93.75 {
			t.Errorf("context savings_pct = %v, want 93.75 (column-ordering regression?)", r.SavingsPct)
		}
	}
}

// TestRecordBenchRun_FKRejectsBadProject pins the cascade-delete
// foreign key. A run referencing a missing project must error rather
// than silently land an orphan row.
func TestRecordBenchRun_FKRejectsBadProject(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	run := BenchRun{
		RunID: "x", ProjectID: "no-such-project",
		StartedAt: time.Now(), NSamples: 1, TraceDepth: 1,
	}
	err = store.RecordBenchRun(run, nil)
	if err == nil {
		t.Fatal("RecordBenchRun with missing project FK returned nil error, want FK violation")
	}
}

// TestListBenchRuns_EmptyOnFreshDB pins the empty-slice contract:
// callers must get []BenchRun{} (zero-length, non-nil) so JSON
// callers don't have to null-check. Mirrors the JSON response
// invariant in CLAUDE.md.
func TestListBenchRuns_EmptyOnFreshDB(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	runs, err := store.ListBenchRuns("", 10)
	if err != nil {
		t.Fatalf("ListBenchRuns: %v", err)
	}
	if runs == nil {
		t.Fatal("ListBenchRuns returned nil slice; want zero-length []BenchRun{}")
	}
	if len(runs) != 0 {
		t.Errorf("ListBenchRuns on fresh DB returned %d rows, want 0", len(runs))
	}
}

// TestListBenchRuns_NewestFirst pins the ORDER BY started_at DESC
// contract — the dashboard panel surfaces newest at the top.
func TestListBenchRuns_NewestFirst(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if err := store.UpsertProject(Project{ID: "p", Path: "/p", Name: "p", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := store.RecordBenchRun(BenchRun{RunID: "old", ProjectID: "p", StartedAt: time.Unix(1, 0), NSamples: 1, TraceDepth: 1}, nil); err != nil {
		t.Fatalf("RecordBenchRun old: %v", err)
	}
	if err := store.RecordBenchRun(BenchRun{RunID: "new", ProjectID: "p", StartedAt: time.Unix(1000, 0), NSamples: 1, TraceDepth: 1}, nil); err != nil {
		t.Fatalf("RecordBenchRun new: %v", err)
	}

	runs, err := store.ListBenchRuns("p", 10)
	if err != nil {
		t.Fatalf("ListBenchRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d rows, want 2", len(runs))
	}
	if runs[0].RunID != "new" {
		t.Errorf("ORDER BY broken: first row = %q, want 'new' (newest started_at)", runs[0].RunID)
	}
}
