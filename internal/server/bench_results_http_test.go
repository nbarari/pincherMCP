package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1263 v0.68 follow-up: HTTP integration tests for /v1/bench-results.
// Pins the endpoint that the dashboard's Bench History panel fetches.
// Validates: empty-state contract, populated round-trip, project
// filter, GET-only refusal.

func TestBenchResultsHTTP_EmptyContract(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest("GET", "/v1/bench-results", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /v1/bench-results returned %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	runs, ok := resp["runs"].([]any)
	if !ok {
		t.Fatalf("response missing runs array; got %v", resp)
	}
	if len(runs) != 0 {
		t.Errorf("expected 0 runs on fresh DB; got %d", len(runs))
	}
	// JSON invariant: empty array, not null — dashboard relies on
	// .length to differentiate empty vs error.
	if resp["runs"] == nil {
		t.Errorf("runs is nil; want []  (matches the #330 invariant)")
	}
}

func TestBenchResultsHTTP_PopulatedRoundTrip(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)

	if err := store.UpsertProject(db.Project{
		ID: "proj-a", Path: "/tmp/a", Name: "a", IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := store.RecordBenchRun(
		db.BenchRun{
			RunID: "r1", ProjectID: "proj-a",
			StartedAt: time.Unix(1000, 0),
			NSamples:  5, TraceDepth: 2, BinaryVersion: "0.68.0",
		},
		[]db.BenchResult{
			{RunID: "r1", ToolName: "search", Calls: 5, P50LatencyMs: 1.0, MeanTokensActual: 100, MeanTokensBaseline: 1000, SavingsPct: 90.0},
		},
	); err != nil {
		t.Fatalf("RecordBenchRun: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/bench-results", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET returned %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	runs := resp["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run; got %d", len(runs))
	}
	run := runs[0].(map[string]any)
	if run["run_id"] != "r1" {
		t.Errorf("run_id = %v, want r1", run["run_id"])
	}
	results, ok := run["results"].([]any)
	if !ok {
		t.Fatalf("run missing results array")
	}
	if len(results) != 1 {
		t.Errorf("expected 1 per-tool result; got %d", len(results))
	}
}

func TestBenchResultsHTTP_ProjectFilter(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)

	for _, id := range []string{"proj-a", "proj-b"} {
		if err := store.UpsertProject(db.Project{ID: id, Path: "/tmp/" + id, Name: id, IndexedAt: time.Now()}); err != nil {
			t.Fatalf("UpsertProject %s: %v", id, err)
		}
		if err := store.RecordBenchRun(
			db.BenchRun{RunID: "r-" + id, ProjectID: id, StartedAt: time.Now(), NSamples: 1, TraceDepth: 1},
			nil,
		); err != nil {
			t.Fatalf("RecordBenchRun %s: %v", id, err)
		}
	}

	// Filter by project — only proj-a's run must surface.
	req := httptest.NewRequest("GET", "/v1/bench-results?project=proj-a", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	runs := resp["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("project=proj-a filter returned %d rows, want 1", len(runs))
	}
	if got := runs[0].(map[string]any)["run_id"]; got != "r-proj-a" {
		t.Errorf("got run_id %v, want r-proj-a", got)
	}
}

func TestBenchResultsHTTP_PostRejected(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/v1/bench-results", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 404 && rr.Code != 405 {
		t.Errorf("expected 404/405 on POST; got %d", rr.Code)
	}
}
