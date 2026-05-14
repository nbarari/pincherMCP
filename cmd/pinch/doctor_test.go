package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
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

func TestTruncMid_Cases(t *testing.T) {
	cases := []struct {
		s    string
		max  int
		want string
	}{
		// Below the threshold — pass through unchanged.
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		// Tiny max — fall through to s[:max] without truncation marker.
		{"abcdefghij", 3, "abc"},
		{"abcdefghij", 7, "abcdefg"},
		// Long string — half-and-half with ellipsis in the middle.
		// max=10, half=4 → s[:4] + "…" + s[len-4:].
		{"abcdefghijklmnopqrst", 10, "abcd…qrst"},
		// Edge: max == len(s) → no truncation.
		{"abcdef", 6, "abcdef"},
	}
	for _, c := range cases {
		if got := truncMid(c.s, c.max); got != c.want {
			t.Errorf("truncMid(%q, %d) = %q, want %q", c.s, c.max, got, c.want)
		}
	}
}

// TestDoctorReport_BinaryVersionPopulated pins that buildDoctorReport
// captures the binary version into the JSON-shaped report and that the
// Markdown formatter surfaces it. Useful when users paste doctor output
// for support — version is right there alongside schema_version.
func TestDoctorReport_BinaryVersionPopulated(t *testing.T) {
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
	if r.BinaryVersion == "" {
		t.Errorf("BinaryVersion should be populated; got empty string")
	}
	// #732: Advisories must be a non-nil slice (JSON invariant) and
	// empty on a healthy tiny test DB.
	if r.Advisories == nil {
		t.Errorf("Advisories should be a non-nil slice; got nil")
	}
	if len(r.Advisories) != 0 {
		t.Errorf("healthy tiny test DB should produce no advisories; got %v", r.Advisories)
	}
	md := formatDoctorMarkdown(r)
	if !strings.Contains(md, "Binary:") {
		t.Errorf("Markdown should include 'Binary:' line, got:\n%s", md)
	}
	if !strings.Contains(md, "v"+r.BinaryVersion) {
		t.Errorf("Markdown should include the version after 'v', got:\n%s", md)
	}
}

// TestFormatDoctorMarkdown_BlankBinaryVersionSuppressed pins the
// graceful-empty branch — a directly-built binary with no -ldflags
// override would set BinaryVersion="" via the test fixture below;
// formatter must skip the line rather than print "Binary:           v".
func TestFormatDoctorMarkdown_BlankBinaryVersionSuppressed(t *testing.T) {
	r := &DoctorReport{BinaryVersion: ""}
	md := formatDoctorMarkdown(r)
	if strings.Contains(md, "Binary:") {
		t.Errorf("blank BinaryVersion should suppress the line, got:\n%s", md)
	}
}

// #732: largeDBAdvisory (CLI copy) must stay behaviourally identical to
// the internal/server copy — a multi-GB DB gets a concrete advisory, a
// small one gets nothing, and the heaviest project is named.
func TestLargeDBAdvisory_CLI(t *testing.T) {
	if got := largeDBAdvisory(500<<20, nil); got != "" {
		t.Errorf("500 MB DB should produce no advisory; got %q", got)
	}
	if got := largeDBAdvisory(1<<30, nil); got != "" {
		t.Errorf("exactly 1 GiB should not trip the advisory; got %q", got)
	}
	projects := []DoctorProjectSummary{
		{Name: "warp_rc", Symbols: 1453923, Files: 4363},
		{Name: "pincher-repo", Symbols: 5169, Files: 394},
	}
	got := largeDBAdvisory(5<<30, projects)
	if got == "" {
		t.Fatal("5 GB DB should produce an advisory")
	}
	for _, want := range []string{"5.0 GB", "warp_rc", "1453923", "prune_dead", "VACUUM"} {
		if !strings.Contains(got, want) {
			t.Errorf("advisory missing %q\n  got: %s", want, got)
		}
	}
	// formatDoctorMarkdown must surface a populated advisory.
	md := formatDoctorMarkdown(&DoctorReport{DBSizeBytes: 5 << 30, Advisories: []string{got}})
	if !strings.Contains(md, "Advisories:") || !strings.Contains(md, "warp_rc") {
		t.Errorf("markdown should render the advisory block; got:\n%s", md)
	}
}

// TestSummarizeByReason_DescCountAlphaTie pins the rollup ordering: the
// most common reason wins, ties break alphabetically so output is stable
// across runs (map iteration order would otherwise jitter).
func TestSummarizeByReason_DescCountAlphaTie(t *testing.T) {
	rows := []DoctorFailureRow{
		{Reason: "qualified_name_collision"},
		{Reason: "file_too_large"},
		{Reason: "file_too_large"},
		{Reason: "file_too_large"},
		{Reason: "byte_range_negative"},
		{Reason: "byte_range_negative"},
		{Reason: "qualified_name_collision"},
	}
	got := summarizeByReason(rows)
	want := "3 file_too_large, 2 byte_range_negative, 2 qualified_name_collision"
	if got != want {
		t.Errorf("summarizeByReason mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

func TestSummarizeByReason_Empty(t *testing.T) {
	if got := summarizeByReason(nil); got != "(none)" {
		t.Errorf("summarizeByReason(nil) = %q, want (none)", got)
	}
}

// TestFormatDoctorMarkdown_RollupShownOver5Failures pins the visible
// behaviour: the rollup line appears once the per-file list crosses
// the visual-scan threshold, and is suppressed below it (where the
// inline list is already legible).
func TestFormatDoctorMarkdown_RollupShownOver5Failures(t *testing.T) {
	rows6 := []DoctorFailureRow{
		{Project: "p", Language: "go", File: "a.go", Reason: "byte_range_negative"},
		{Project: "p", Language: "go", File: "b.go", Reason: "byte_range_negative"},
		{Project: "p", Language: "go", File: "c.go", Reason: "qualified_name_collision"},
		{Project: "p", Language: "go", File: "d.go", Reason: "qualified_name_collision"},
		{Project: "p", Language: "go", File: "e.go", Reason: "qualified_name_collision"},
		{Project: "p", Language: "go", File: "f.go", Reason: "file_too_large"},
	}
	out := formatDoctorMarkdown(&DoctorReport{ExtractionFailures: rows6, LookbackHours: 24})
	if !strings.Contains(out, "by reason:") {
		t.Errorf("expected rollup line for 6 failures, got:\n%s", out)
	}
	if !strings.Contains(out, "3 qualified_name_collision") {
		t.Errorf("expected '3 qualified_name_collision' in rollup, got:\n%s", out)
	}

	// 5 failures: rollup suppressed (the inline list is short enough to scan).
	out5 := formatDoctorMarkdown(&DoctorReport{ExtractionFailures: rows6[:5], LookbackHours: 24})
	if strings.Contains(out5, "by reason:") {
		t.Errorf("rollup should be suppressed at 5 failures, got:\n%s", out5)
	}
}

func TestTruncEnd_Cases(t *testing.T) {
	cases := []struct {
		s    string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		// Long string — keep first max-1 chars and tack on ellipsis.
		{"abcdefghijklmnopqrst", 10, "abcdefghi…"},
		{"abcdef", 6, "abcdef"},
	}
	for _, c := range cases {
		if got := truncEnd(c.s, c.max); got != c.want {
			t.Errorf("truncEnd(%q, %d) = %q, want %q", c.s, c.max, got, c.want)
		}
	}
}

// TestDoctorCLI_Binary_Markdown exercises the runDoctorCLI dispatch
// wrapper end-to-end — build the binary, run it against a fresh
// data dir, assert the human-readable Markdown output renders the
// expected sections. Together with TestDoctorCLI_Binary_JSON below
// these two tests cover the CLI flag-parsing + output-format paths
// that buildDoctorReport / formatDoctorMarkdown unit tests can't
// reach.
//
// With GOCOVERDIR set in the parent process (CI Coverage job), the
// binary is built with -cover and runDoctorCLI's coverage is folded
// into the merged coverprofile via Go's automatic test runner merge.
func TestDoctorCLI_Binary_Markdown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()

	cmd := exec.Command(bin, "doctor", "--data-dir", dataDir)
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pincher doctor: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"pincherMCP doctor",
		"Binary:",
		"Schema:",
		"Database size:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing field %q in doctor output:\n%s", want, got)
		}
	}
}

func TestDoctorCLI_Binary_JSON(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()

	cmd := exec.Command(bin, "doctor", "--data-dir", dataDir, "--json")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pincher doctor --json: %v\n%s", err, out)
	}

	// Just assert it's valid JSON with at least one top-level key — a
	// stricter shape gate would tie this test to internal report fields
	// that change as the diagnostic surface evolves.
	var report map[string]any
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("doctor --json output is not valid JSON: %v\n%s", err, out)
	}
	if len(report) == 0 {
		t.Fatalf("doctor --json output is empty object\n%s", out)
	}
	for _, key := range []string{"schema_version"} {
		if _, ok := report[key]; !ok {
			t.Errorf("doctor --json missing top-level key %q\nfull report:\n%s", key, out)
		}
	}
}

// TestDoctorCLI_Binary_TopFlag covers the --top argument plumbing.
// The default is 10; we set --top 3 to exercise the non-default path
// through the dispatch wrapper.
func TestDoctorCLI_Binary_TopFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()
	cmd := exec.Command(bin, "doctor", "--data-dir", dataDir, "--top", "3", "--json")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pincher doctor --top 3: %v\n%s", err, out)
	}
	var report DoctorReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
}

func TestDoctorCLI_Binary_LookbackFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()

	cmd := exec.Command(bin, "doctor", "--data-dir", dataDir, "--lookback", "24")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pincher doctor --lookback 24: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "pincherMCP doctor") {
		t.Errorf("expected doctor banner in --lookback output:\n%s", got)
	}
	// Lookback window should be reflected somewhere in the output. Default
	// is "last 168 hours"; with --lookback 24 we expect "last 24 hours".
	if !strings.Contains(got, "last 24 hours") {
		t.Errorf("expected '24 hours' lookback window in output:\n%s", got)
	}
}
