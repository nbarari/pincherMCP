package db

import (
	"path/filepath"
	"testing"
	"time"
)

// #1421: extraction_failures rows must carry the binary version that
// recorded them so doctor consumers can distinguish "fixed-since-this-
// binary" rows from "still recurring on the running binary" without
// cross-referencing CHANGELOG by hand. Schema v33.
func TestRecordExtractionFailureWithBinary_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.UpsertProject(Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	if err := s.RecordExtractionFailureWithBinary("p1", "broken.go", "Go", "parse_error", "syntax err line 42", "0.71.0-7-ga9794e7"); err != nil {
		t.Fatalf("RecordExtractionFailureWithBinary: %v", err)
	}

	rows, err := s.ListExtractionFailures("p1", 0)
	if err != nil {
		t.Fatalf("ListExtractionFailures: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if got, want := rows[0].BinaryVersionAtFailure, "0.71.0-7-ga9794e7"; got != want {
		t.Errorf("BinaryVersionAtFailure = %q; want %q", got, want)
	}
}

// Backwards-compat: legacy callers using RecordExtractionFailure (no
// binary version) must still work — empty string stays the documented
// "unknown" sentinel so pre-v33 rows / dev builds don't get a misleading
// stamp.
func TestRecordExtractionFailure_LegacyWrapper_EmptyBinaryVersion(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.UpsertProject(Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := s.RecordExtractionFailure("p1", "broken.go", "Go", "parse_error", "syntax err"); err != nil {
		t.Fatalf("RecordExtractionFailure: %v", err)
	}

	rows, err := s.ListExtractionFailures("p1", 0)
	if err != nil {
		t.Fatalf("ListExtractionFailures: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if got := rows[0].BinaryVersionAtFailure; got != "" {
		t.Errorf("BinaryVersionAtFailure = %q; want empty string (legacy wrapper)", got)
	}
}

// Cross-project read path (the one doctor actually uses) must surface
// the binary version too.
func TestListRecentExtractionFailuresAcrossProjects_CarriesBinaryVersion(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.UpsertProject(Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertProject p1: %v", err)
	}
	if err := s.UpsertProject(Project{ID: "p2", Path: "/tmp/p2", Name: "p2", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertProject p2: %v", err)
	}
	if err := s.RecordExtractionFailureWithBinary("p1", "a.go", "Go", "parse_error", "x", "0.71.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordExtractionFailureWithBinary("p2", "b.go", "Go", "parse_error", "y", "0.72.0"); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListRecentExtractionFailuresAcrossProjects(0, 100)
	if err != nil {
		t.Fatalf("ListRecentExtractionFailuresAcrossProjects: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	versions := map[string]string{}
	for _, r := range rows {
		versions[r.ProjectID] = r.BinaryVersionAtFailure
	}
	if versions["p1"] != "0.71.0" {
		t.Errorf("p1 binary version = %q; want 0.71.0", versions["p1"])
	}
	if versions["p2"] != "0.72.0" {
		t.Errorf("p2 binary version = %q; want 0.72.0", versions["p2"])
	}
}

// Schema-parity guard: the v33 migration must actually have added the
// column. Reads the column directly via PRAGMA so the test fails
// loudly if a future migration accidentally drops or renames the field.
func TestExtractionFailures_BinaryVersionColumn_Present_v33(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	rows, err := s.ro.Query(`SELECT name FROM pragma_table_info('extraction_failures') WHERE name = 'binary_version_at_failure'`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("extraction_failures.binary_version_at_failure column missing — v33 migration did not run")
	}
}
