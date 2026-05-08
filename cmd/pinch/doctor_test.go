package main

import (
	"strings"
	"testing"
	"time"

	"github.com/pincherMCP/pincher/internal/db"
)

// TestDoctorReport_EmptyDatabase pins the healthy-empty-state shape:
// fresh DB, no projects, no failures, no slow queries → report renders
// cleanly with the "No diagnostic issues to report" footer.
func TestDoctorReport_EmptyDatabase(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	r, err := buildDoctorReport(store, dir, 168, 10)
	if err != nil {
		t.Fatalf("buildDoctorReport: %v", err)
	}

	if r.SchemaVersion < 1 {
		t.Errorf("schema_version = %d, want >= 1 on a fresh DB", r.SchemaVersion)
	}
	if len(r.Projects) != 0 {
		t.Errorf("expected 0 projects, got %d", len(r.Projects))
	}
	if len(r.ExtractionFailures) != 0 {
		t.Errorf("expected 0 failures, got %d", len(r.ExtractionFailures))
	}
	if len(r.SlowQueries) != 0 {
		t.Errorf("expected 0 slow queries, got %d", len(r.SlowQueries))
	}

	md := formatDoctorMarkdown(r)
	if !strings.Contains(md, "No diagnostic issues to report") {
		t.Errorf("empty-state Markdown missing healthy footer:\n%s", md)
	}
}

// TestDoctorReport_WithFailuresAndSlowQueries — populated state. Each
// section MUST appear in the rendered Markdown with the right counts.
func TestDoctorReport_WithFailuresAndSlowQueries(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if err := store.UpsertProject(db.Project{
		ID: "p1", Path: "/p", Name: "demo", IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	// Two extraction failures.
	for _, c := range []struct{ file, lang, reason, details string }{
		{"compose.yaml", "YAML", "byte_range_negative", "end_byte=10 <= start_byte=10"},
		{"main.go", "Go", "extractor_panicked", "runtime error: index out of range"},
	} {
		if err := store.RecordExtractionFailure("p1", c.file, c.lang, c.reason, c.details); err != nil {
			t.Fatalf("RecordExtractionFailure: %v", err)
		}
	}

	// Two slow queries.
	for _, c := range []struct {
		tool, projectID, args string
		duration              int64
	}{
		{"search", "p1", `{"query":"open"}`, 220},
		{"trace", "p1", `{"name":"main"}`, 1500},
	} {
		if err := store.RecordSlowQuery(c.tool, c.projectID, c.duration, c.args); err != nil {
			t.Fatalf("RecordSlowQuery: %v", err)
		}
	}

	r, err := buildDoctorReport(store, dir, 168, 10)
	if err != nil {
		t.Fatalf("buildDoctorReport: %v", err)
	}

	if len(r.Projects) != 1 || r.Projects[0].Name != "demo" {
		t.Errorf("project list = %+v, want one project named 'demo'", r.Projects)
	}
	if len(r.ExtractionFailures) != 2 {
		t.Errorf("expected 2 failures, got %d", len(r.ExtractionFailures))
	}
	if len(r.SlowQueries) != 2 {
		t.Errorf("expected 2 slow queries, got %d", len(r.SlowQueries))
	}

	md := formatDoctorMarkdown(r)
	for _, want := range []string{
		"demo",
		"compose.yaml", "byte_range_negative",
		"main.go", "extractor_panicked",
		"search", "1500",
		"Extraction failures (last 168 hours):  2",
		"Slow queries (last 168 hours):         2",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown missing %q\nfull report:\n%s", want, md)
		}
	}
	// And the "no issues" footer MUST NOT appear when there ARE issues.
	if strings.Contains(md, "No diagnostic issues to report") {
		t.Errorf("populated-state Markdown should not contain healthy footer:\n%s", md)
	}
}

// TestDoctorReport_LookbackFilters — rows older than the lookback window
// MUST be excluded. Without this, `pincher doctor` would show stale
// failures from months-old re-indexes.
func TestDoctorReport_LookbackFilters(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.UpsertProject(db.Project{
		ID: "p1", Path: "/p", Name: "demo", IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	// Insert a failure with a fake old last_seen_at via raw SQL.
	if _, err := store.DB().Exec(
		`INSERT INTO extraction_failures (project_id, file_path, language, reason, details, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p1", "old.yaml", "YAML", "parse_error", "", 0, 0,
	); err != nil {
		t.Fatalf("seed old failure: %v", err)
	}

	// Recent failure
	if err := store.RecordExtractionFailure("p1", "fresh.go", "Go", "extractor_panicked", "fresh"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Lookback = 1 hour — old row excluded, fresh row included.
	r, err := buildDoctorReport(store, dir, 1, 10)
	if err != nil {
		t.Fatalf("buildDoctorReport: %v", err)
	}
	if len(r.ExtractionFailures) != 1 {
		t.Errorf("expected 1 fresh failure, got %d", len(r.ExtractionFailures))
	}
	for _, f := range r.ExtractionFailures {
		if f.File == "old.yaml" {
			t.Errorf("old.yaml (last_seen_at=0) should be filtered by 1-hour lookback")
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{2048, "2.0 KB"},
		{1024 * 1024, "1.0 MB"},
		{int64(1024) * 1024 * 1024 * 5, "5.0 GB"},
	}
	for _, c := range cases {
		got := humanBytes(c.n)
		if got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
