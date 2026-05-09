package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pincherMCP/pincher/internal/db"
)

// TestStatsCLI_BuildReport_Empty exercises the report builder against a
// brand-new DB (no sessions, no projects). All-time totals must be zero
// and the projects slice must be empty (not nil — JSON-friendly).
func TestStatsCLI_BuildReport_Empty(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	report, err := buildStatsReport(store, dir)
	if err != nil {
		t.Fatalf("buildStatsReport: %v", err)
	}

	if report.AllTime.Calls != 0 {
		t.Errorf("calls = %d, want 0 on empty DB", report.AllTime.Calls)
	}
	if report.AllTime.CostAvoided != 0 {
		t.Errorf("cost_avoided = %v, want 0 on empty DB", report.AllTime.CostAvoided)
	}
	if report.Projects == nil {
		t.Error("projects slice is nil; want non-nil empty slice for stable JSON shape")
	}
	if len(report.Projects) != 0 {
		t.Errorf("projects = %d on empty DB, want 0", len(report.Projects))
	}
	if report.DataDir != dir {
		t.Errorf("data_dir = %q, want %q", report.DataDir, dir)
	}
}

// TestStatsCLI_BuildReport_WithSessions covers the all-time aggregation:
// inserting two session rows must produce summed totals in the report.
// The query path goes through GetAllTimeSavings which uses SUM() with
// COALESCE — this test pins that aggregation rather than the column
// values themselves.
func TestStatsCLI_BuildReport_WithSessions(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if err := store.RecordSession("s1", time.Unix(1, 0), 10, 100, 1000, 0.5); err != nil {
		t.Fatalf("RecordSession s1: %v", err)
	}
	if err := store.RecordSession("s2", time.Unix(3, 0), 5, 50, 500, 0.25); err != nil {
		t.Fatalf("RecordSession s2: %v", err)
	}

	report, err := buildStatsReport(store, dir)
	if err != nil {
		t.Fatalf("buildStatsReport: %v", err)
	}

	if got, want := report.AllTime.Calls, int64(15); got != want {
		t.Errorf("calls = %d, want %d", got, want)
	}
	if got, want := report.AllTime.TokensSaved, int64(1500); got != want {
		t.Errorf("tokens_saved = %d, want %d", got, want)
	}
	if got, want := report.AllTime.CostAvoided, 0.75; got != want {
		t.Errorf("cost_avoided = %v, want %v", got, want)
	}
}

// TestStatsCLI_JSONShape_IsValidJSON pins the JSON output's structural
// shape: the top-level object must contain `all_time`, `projects`,
// `data_dir`, and `db_size_kb`. Without this, dashboard / shell
// integrations would silently break on any future field rename.
func TestStatsCLI_JSONShape_IsValidJSON(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	report, err := buildStatsReport(store, dir)
	if err != nil {
		t.Fatalf("buildStatsReport: %v", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, encoded)
	}
	for _, key := range []string{"all_time", "projects", "data_dir", "db_size_kb"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("JSON output missing top-level key %q:\n%s", key, encoded)
		}
	}
	at, ok := parsed["all_time"].(map[string]any)
	if !ok {
		t.Fatalf("all_time is not an object:\n%s", encoded)
	}
	for _, key := range []string{"calls", "tokens_used", "tokens_saved", "cost_avoided"} {
		if _, ok := at[key]; !ok {
			t.Errorf("all_time missing key %q:\n%s", key, encoded)
		}
	}
}

// TestStatsCLI_TextOutput_ContainsExpectedSections renders the
// human-readable form and asserts the key section headers + a
// formatted cost line are present. Catches refactors that drop
// labels or change the dollar formatting.
func TestStatsCLI_TextOutput_ContainsExpectedSections(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.RecordSession("s1", time.Unix(1, 0), 100, 1000, 10000, 1.2345); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}

	report, _ := buildStatsReport(store, dir)
	out := formatStatsText(report)

	for _, want := range []string{"ALL-TIME", "STORAGE", "Tool calls:", "Cost avoided:", "$1.2345"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
}

// TestStatsCLI_Reset_DeletesAllSessions covers the destructive path:
// after seeding sessions, ResetSessions must return the row count and
// subsequent GetAllTimeSavings must return zeros. Idempotent — running
// twice is a no-op (zero rows the second time).
func TestStatsCLI_Reset_DeletesAllSessions(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	for i, id := range []string{"s1", "s2", "s3"} {
		if err := store.RecordSession(id, time.Unix(int64(i), 0),
			int64(10*(i+1)), 100, 1000, 0.1); err != nil {
			t.Fatalf("RecordSession %s: %v", id, err)
		}
	}

	rows, err := store.ResetSessions()
	if err != nil {
		t.Fatalf("ResetSessions: %v", err)
	}
	if rows != 3 {
		t.Errorf("rows deleted = %d, want 3", rows)
	}

	calls, _, _, _, err := store.GetAllTimeSavings()
	if err != nil {
		t.Fatalf("GetAllTimeSavings post-reset: %v", err)
	}
	if calls != 0 {
		t.Errorf("calls post-reset = %d, want 0", calls)
	}

	// Idempotent: second reset deletes nothing.
	rows2, err := store.ResetSessions()
	if err != nil {
		t.Fatalf("ResetSessions (idempotent): %v", err)
	}
	if rows2 != 0 {
		t.Errorf("second reset deleted %d rows, want 0 (idempotent)", rows2)
	}
}

// TestRunStatsCLI_FlagDispatch_JSON exercises the actual subcommand
// handler — flag parsing, dispatch to the report path, JSON envelope
// emission. The building-block tests don't catch flag-name typos or
// dispatch wiring breakage; this one does. Captures stdout via os.Pipe
// so we read what the handler actually wrote.
func TestRunStatsCLI_FlagDispatch_JSON(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.RecordSession("s1", time.Unix(1, 0), 7, 70, 700, 0.21); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	store.Close() // runStatsCLI opens its own handle.

	out := captureStdout(t, func() {
		runStatsCLI([]string{"--data-dir", dir, "--json"})
	})

	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	at, ok := parsed["all_time"].(map[string]any)
	if !ok {
		t.Fatalf("all_time missing or not an object:\n%s", out)
	}
	if got := at["calls"]; got != float64(7) {
		t.Errorf("all_time.calls = %v, want 7", got)
	}
	if got := parsed["data_dir"]; got != dir {
		t.Errorf("data_dir = %v, want %q", got, dir)
	}
}

// TestRunStatsCLI_ResetJSON_ShapeIsPinned pins the JSON output for the
// destructive `--reset --json` path. Three top-level keys are
// load-bearing for any dashboard / monitor that consumes the output:
//   - reset: bool true
//   - rows_deleted: int (count of sessions wiped)
//   - data_dir: string (so the operator knows which DB was hit)
//
// A typo in any field name would silently break integrations.
func TestRunStatsCLI_ResetJSON_ShapeIsPinned(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i, id := range []string{"s1", "s2"} {
		if err := store.RecordSession(id, time.Unix(int64(i), 0), 1, 1, 1, 0.1); err != nil {
			t.Fatalf("RecordSession %s: %v", id, err)
		}
	}
	store.Close()

	out := captureStdout(t, func() {
		runStatsCLI([]string{"--data-dir", dir, "--reset", "--json"})
	})

	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if got := parsed["reset"]; got != true {
		t.Errorf("reset = %v, want true", got)
	}
	if got := parsed["rows_deleted"]; got != float64(2) {
		t.Errorf("rows_deleted = %v, want 2", got)
	}
	if got := parsed["data_dir"]; got != dir {
		t.Errorf("data_dir = %v, want %q", got, dir)
	}
}

// TestRunStatsCLI_ResetText_PrintsRowCount covers the non-JSON reset
// path: the text output must include the row count and the DB file
// path so the operator can confirm what was wiped.
func TestRunStatsCLI_ResetText_PrintsRowCount(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.RecordSession("s1", time.Unix(1, 0), 1, 1, 1, 0.1); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	store.Close()

	out := captureStdout(t, func() {
		runStatsCLI([]string{"--data-dir", dir, "--reset"})
	})

	if !strings.Contains(out, "Wiped 1") {
		t.Errorf("text output missing 'Wiped 1' confirmation:\n%s", out)
	}
	if !strings.Contains(out, "pincher.db") {
		t.Errorf("text output missing DB path so operator knows which file was wiped:\n%s", out)
	}
}

// TestDBFileSizeKB_MissingFile pins the graceful-degrade behavior:
// a directory without pincher.db must return 0, not crash. Used by
// buildStatsReport which we don't want to fail just because a file
// stat returned ENOENT.
func TestDBFileSizeKB_MissingFile(t *testing.T) {
	dir := t.TempDir() // no pincher.db inside
	if got := dbFileSizeKB(dir); got != 0 {
		t.Errorf("dbFileSizeKB on empty dir = %d, want 0", got)
	}
}

// captureStdout temporarily redirects os.Stdout to a pipe, runs `fn`,
// then returns whatever was written. Used by the runStatsCLI tests so
// they can assert on the actual handler's output without spawning a
// subprocess. Restores os.Stdout via defer regardless of fn's behavior.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	w.Close()
	return <-done
}

// TestStatsCLI_Reset_PreservesSymbols guards against a refactor that
// would broaden ResetSessions to delete adjacent tables. After reset,
// projects + symbols must still exist exactly as before.
func TestStatsCLI_Reset_PreservesSymbols(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if err := store.UpsertProject(db.Project{
		ID: "/tmp/p", Path: "/tmp/p", Name: "p",
		FileCount: 1, SymCount: 1,
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := store.BulkUpsertSymbols([]db.Symbol{{
		ID: "/tmp/p::pkg.Foo#Function", ProjectID: "/tmp/p",
		FilePath: "x.go", Name: "Foo", QualifiedName: "pkg.Foo",
		Kind: "Function", Language: "Go",
	}}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	if err := store.RecordSession("s1", time.Unix(1, 0), 1, 0, 0, 0); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}

	if _, err := store.ResetSessions(); err != nil {
		t.Fatalf("ResetSessions: %v", err)
	}

	projects, err := store.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Errorf("project count post-reset = %d, want 1", len(projects))
	}
	sym, err := store.GetSymbol("/tmp/p::pkg.Foo#Function")
	if err != nil {
		t.Fatalf("GetSymbol: %v", err)
	}
	if sym == nil {
		t.Error("symbol gone post-reset; reset should not touch symbols")
	}
}
