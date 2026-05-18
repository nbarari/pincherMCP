package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
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
	if report.AllTime.TokensSaved != 0 {
		t.Errorf("tokens_saved = %v, want 0 on empty DB", report.AllTime.TokensSaved)
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

	if err := store.RecordSession("s1", time.Unix(1, 0), 10, 100, 1000, 0.5, "", 0, ""); err != nil {
		t.Fatalf("RecordSession s1: %v", err)
	}
	if err := store.RecordSession("s2", time.Unix(3, 0), 5, 50, 500, 0.25, "", 0, ""); err != nil {
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
	if got, want := report.AllTime.TokensUsed, int64(150); got != want {
		t.Errorf("tokens_used = %d, want %d", got, want)
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
	for _, key := range []string{"calls", "tokens_used", "tokens_saved"} {
		if _, ok := at[key]; !ok {
			t.Errorf("all_time missing key %q:\n%s", key, encoded)
		}
	}
	if _, present := at["cost_avoided"]; present {
		t.Errorf("all_time must NOT carry cost_avoided — removed in #476 SAVINGS_HONESTY")
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
	if err := store.RecordSession("s1", time.Unix(1, 0), 100, 1000, 10000, 1.2345, "", 0, ""); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}

	report, _ := buildStatsReport(store, dir)
	out := formatStatsText(report)

	for _, want := range []string{"ALL-TIME", "STORAGE", "Tool calls:", "Tokens saved:"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
	// #476 SAVINGS_HONESTY: no $-figures in the human-readable output.
	for _, banned := range []string{"Cost avoided:", "$1.2345", "$"} {
		if strings.Contains(out, banned) {
			t.Errorf("text output contains banned token %q (removed in #476 SAVINGS_HONESTY):\n%s", banned, out)
		}
	}
}

// TestStatsCLI_TextOutput_BoxAlignmentWithWideContent regression-pins
// the dynamic box-width fix. Pre-fix, the box was hardcoded to 44 chars,
// so any project row whose value (e.g. "447,201 syms / 39,276 files" =
// 28 chars) plus the 23-char label-prefix-padding exceeded 44 would
// overflow the closing │ visually rightward. Post-fix, the box auto-sizes
// to fit the widest content (capped at 100 chars).
//
// Test strategy: build a report with a project whose symbol/file counts
// produce a value wider than the prior 44-char box, render, then assert:
//   - every line ending with │ has the SAME column position
//   - no line is wider than the closing │ on the bottom border
//   - the wide value is fully present (not truncated under the cap)
func TestStatsCLI_TextOutput_BoxAlignmentWithWideContent(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.RecordSession("s1", time.Unix(1, 0), 1, 1, 1, 0.1, "", 0, ""); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	// Seed a project whose value-line width forces dynamic resizing.
	// 447,201 / 39,276 mirrors a real large-project shape.
	if err := store.UpsertProject(db.Project{
		ID: "/tmp/big", Path: "/tmp/big", Name: "thinksmart-shaped",
		FileCount: 39276, SymCount: 447201,
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	report, _ := buildStatsReport(store, dir)
	out := formatStatsText(report)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 4 {
		t.Fatalf("output too short to be a rendered box:\n%s", out)
	}

	// Every box line should have the same visual width — measured by the
	// rune count, since the box-drawing characters are multi-byte UTF-8.
	expectedWidth := len([]rune(lines[0]))
	for i, ln := range lines {
		if got := len([]rune(ln)); got != expectedWidth {
			t.Errorf("line %d width = %d runes, want %d (box overflow):\n%s",
				i, got, expectedWidth, out)
		}
	}

	// The wide value must be present in the output (not truncated under
	// the 100-char cap, since 447,201 syms / 39,276 files fits).
	for _, want := range []string{"447,201 syms", "39,276 files", "thinksmart-shaped:"} {
		if !strings.Contains(out, want) {
			t.Errorf("wide-value content missing %q:\n%s", want, out)
		}
	}
}

// TestStatsCLI_TextOutput_NoProjectsStillRenders pins that the box
// closes cleanly when r.Projects is empty (only ALL-TIME + STORAGE
// sections render). The dynamic-width refactor uses a magic index of 6
// for "first project row" — if a future change shifts the fixed rows,
// this test catches the off-by-one.
func TestStatsCLI_TextOutput_NoProjectsStillRenders(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.RecordSession("s1", time.Unix(1, 0), 5, 50, 500, 0.05, "", 0, ""); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}

	report, _ := buildStatsReport(store, dir)
	out := formatStatsText(report)

	// Box must contain ALL-TIME + STORAGE headers but NOT the PROJECTS
	// header (no projects → that section is skipped).
	for _, want := range []string{"ALL-TIME", "STORAGE", "Tool calls:", "Data dir:"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q in projects-empty render:\n%s", want, out)
		}
	}
	if strings.Contains(out, "PROJECTS") {
		t.Errorf("PROJECTS header rendered despite empty projects list:\n%s", out)
	}

	// Box must still be visually closed: every line same rune-width.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	expectedWidth := len([]rune(lines[0]))
	for i, ln := range lines {
		if got := len([]rune(ln)); got != expectedWidth {
			t.Errorf("line %d width = %d runes, want %d (box not closed):\n%s",
				i, got, expectedWidth, out)
		}
	}
}

// TestStatsCLI_TextOutput_LanguagesSection (#240) verifies the
// per-language tally renders between STORAGE and PROJECTS. Pins:
//   - LANGUAGES header present when calls_by_language is non-empty
//   - Sort order: count descending, lexical ascending tie-breaker
//   - Box still closes (every line same rune-width) — guards against the
//     section-insert breaking the dynamic box-width math from #238.
func TestStatsCLI_TextOutput_LanguagesSection(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	// Two sessions whose JSON payloads sum across to give a well-known
	// per-language tally with a tie (Markdown=3, Python=3) so we can
	// pin the lexical tie-breaker.
	if err := store.RecordSession("a", time.Unix(1, 0), 12, 100, 1000, 0.1, "", 0, `{"Go":7,"Markdown":3,"Python":2}`); err != nil {
		t.Fatalf("RecordSession a: %v", err)
	}
	if err := store.RecordSession("b", time.Unix(2, 0), 4, 40, 400, 0.04, "", 0, `{"Python":1}`); err != nil {
		t.Fatalf("RecordSession b: %v", err)
	}

	report, _ := buildStatsReport(store, dir)
	out := formatStatsText(report)

	if !strings.Contains(out, "LANGUAGES") {
		t.Errorf("LANGUAGES header missing from output:\n%s", out)
	}
	for _, lang := range []string{"Go:", "Markdown:", "Python:"} {
		if !strings.Contains(out, lang) {
			t.Errorf("language label %q missing from output:\n%s", lang, out)
		}
	}

	// Sort order: count desc with lex tie-break → Go(7), Markdown(3), Python(3).
	// Find each label's position and assert ordering.
	idxGo := strings.Index(out, "Go:")
	idxMd := strings.Index(out, "Markdown:")
	idxPy := strings.Index(out, "Python:")
	if !(idxGo < idxMd && idxMd < idxPy) {
		t.Errorf("language order wrong: Go=%d Markdown=%d Python=%d (want Go < Markdown < Python via count-desc + lex tie-break):\n%s",
			idxGo, idxMd, idxPy, out)
	}

	// LANGUAGES inserted between STORAGE and PROJECTS (no projects here →
	// PROJECTS not present, so just check STORAGE precedes LANGUAGES).
	idxStorage := strings.Index(out, "STORAGE")
	idxLang := strings.Index(out, "LANGUAGES")
	if !(idxStorage < idxLang) {
		t.Errorf("LANGUAGES section did not render between STORAGE and PROJECTS:\n%s", out)
	}

	// Box still closes — every line same rune-width.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	expectedWidth := len([]rune(lines[0]))
	for i, ln := range lines {
		if got := len([]rune(ln)); got != expectedWidth {
			t.Errorf("line %d width = %d runes, want %d (LANGUAGES insert broke box alignment):\n%s",
				i, got, expectedWidth, out)
		}
	}
}

// TestStatsCLI_TextOutput_LanguagesSectionAbsentWhenEmpty pins backward
// compatibility: pre-v16 sessions (or v16 sessions with no language data)
// must render exactly as before — no empty LANGUAGES box.
func TestStatsCLI_TextOutput_LanguagesSectionAbsentWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	// Session recorded with empty calls_by_language → SQL NULL → not in aggregate.
	if err := store.RecordSession("legacy", time.Unix(1, 0), 5, 50, 500, 0.05, "", 0, ""); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}

	report, _ := buildStatsReport(store, dir)
	out := formatStatsText(report)

	if strings.Contains(out, "LANGUAGES") {
		t.Errorf("LANGUAGES header rendered despite empty per-language data:\n%s", out)
	}
}

// TestStatsCLI_TextOutput_RetriesSection (#241) pins the RETRIES
// section: when at least one query-shaped call has been recorded, the
// section appears with the four counter rows + retry-rate. Sorted
// after LANGUAGES, before PROJECTS.
func TestStatsCLI_TextOutput_RetriesSection(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.RecordSessionWithMetrics("s1", time.Unix(1, 0), 5, 50, 500, 0.05, "", 0, "",
		db.QueryMetrics{QueriesTotal: 100, QueriesZeroResult: 18, QueriesRetriedSucceeded: 14, TokensBurnedOnFailures: 4200}); err != nil {
		t.Fatalf("RecordSessionWithMetrics: %v", err)
	}

	report, _ := buildStatsReport(store, dir)
	out := formatStatsText(report)

	if !strings.Contains(out, "RETRIES") {
		t.Errorf("RETRIES header missing from output:\n%s", out)
	}
	for _, label := range []string{"Total queries:", "Zero-result:", "Zero-result rate:", "Recovered:", "Retry success:", "Tokens burned:"} {
		if !strings.Contains(out, label) {
			t.Errorf("retries label %q missing from output:\n%s", label, out)
		}
	}
	if !strings.Contains(out, "18.0%") {
		t.Errorf("zero-result rate 18.0%% missing from output:\n%s", out)
	}
	// #1494: "Retry rate:" was the misleading pre-fix label; if it
	// resurfaces, the rename regressed.
	if strings.Contains(out, "Retry rate:") {
		t.Errorf("legacy 'Retry rate:' label present — #1494 rename regressed:\n%s", out)
	}
	if !strings.Contains(out, "4,200") {
		t.Errorf("commified tokens-burned 4,200 missing from output:\n%s", out)
	}

	// Box still closes — every line same rune-width.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	expectedWidth := len([]rune(lines[0]))
	for i, ln := range lines {
		if got := len([]rune(ln)); got != expectedWidth {
			t.Errorf("line %d width = %d runes, want %d (RETRIES insert broke box alignment):\n%s",
				i, got, expectedWidth, out)
		}
	}
}

// TestStatsCLI_TextOutput_RetriesSectionAbsentWhenZero (#241) pins
// the noise-free output property: a session with zero query-shaped
// calls renders without a RETRIES section. We don't want to add a
// "Retry rate: 0.0%" row to every healthy project's stats.
func TestStatsCLI_TextOutput_RetriesSectionAbsentWhenZero(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.RecordSession("legacy", time.Unix(1, 0), 5, 50, 500, 0.05, "", 0, ""); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	report, _ := buildStatsReport(store, dir)
	out := formatStatsText(report)
	if strings.Contains(out, "RETRIES") {
		t.Errorf("RETRIES header rendered despite zero query-shaped calls:\n%s", out)
	}
}

// TestStatsCLI_JSONShape_QueryMetrics pins the JSON shape: v17 binaries
// always emit the `query_metrics` block under `all_time`. Dashboards
// can render the retry-rate diagnostic without a feature-detection
// dance, and the field set is stable for shell pipelines.
func TestStatsCLI_JSONShape_QueryMetrics(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.RecordSessionWithMetrics("s1", time.Unix(1, 0), 5, 50, 500, 0.05, "", 0, "",
		db.QueryMetrics{QueriesTotal: 50, QueriesZeroResult: 5, QueriesRetriedSucceeded: 4, TokensBurnedOnFailures: 1000}); err != nil {
		t.Fatalf("RecordSessionWithMetrics: %v", err)
	}
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
	at, ok := parsed["all_time"].(map[string]any)
	if !ok {
		t.Fatalf("all_time is not an object:\n%s", encoded)
	}
	qm, ok := at["query_metrics"].(map[string]any)
	if !ok {
		t.Fatalf("query_metrics missing or not an object:\n%s", encoded)
	}
	if got := qm["queries_total"]; got != float64(50) {
		t.Errorf("queries_total = %v, want 50", got)
	}
	if got := qm["queries_zero_result"]; got != float64(5) {
		t.Errorf("queries_zero_result = %v, want 5", got)
	}
	// #1494 rename: retry_rate → zero_result_rate (the value is
	// queries_zero_result/queries_total, which has never been a true
	// retry rate). retry_success_rate is the new positive signal.
	if got := qm["zero_result_rate"]; got != 0.1 {
		t.Errorf("zero_result_rate = %v, want 0.1 (5/50)", got)
	}
	if _, present := qm["retry_rate"]; present {
		t.Errorf("legacy retry_rate field present — #1494 rename regressed")
	}
	if got := qm["retry_success_rate"]; got != 0.8 {
		t.Errorf("retry_success_rate = %v, want 0.8 (4/5)", got)
	}
}

// TestStatsCLI_JSONShape_CallsByLanguage pins the JSON shape: when
// per-language data is present, `all_time.calls_by_language` is a
// flat string→int map. Dashboards and shell pipelines depend on this
// shape — a future refactor that wraps the map in an object would
// silently break them.
func TestStatsCLI_JSONShape_CallsByLanguage(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.RecordSession("s1", time.Unix(1, 0), 5, 50, 500, 0.05, "", 0, `{"Go":3,"YAML":2}`); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}

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
	at, ok := parsed["all_time"].(map[string]any)
	if !ok {
		t.Fatalf("all_time is not an object:\n%s", encoded)
	}
	cbl, ok := at["calls_by_language"].(map[string]any)
	if !ok {
		t.Fatalf("calls_by_language is not an object (or missing):\n%s", encoded)
	}
	if got := cbl["Go"]; got != float64(3) {
		t.Errorf("Go count = %v, want 3 (JSON numbers parse as float64)", got)
	}
	if got := cbl["YAML"]; got != float64(2) {
		t.Errorf("YAML count = %v, want 2", got)
	}
}

// TestStatsCLI_TextOutput_PathologicalLengthHitsCap exercises the
// 100-char cap and the value-truncation path. Seeds a project with an
// extreme symbol/file count + ridiculously long name so the natural
// width would exceed 100. Asserts the box still closes (truncation
// kicks in) and the output stays bounded.
func TestStatsCLI_TextOutput_PathologicalLengthHitsCap(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.RecordSession("s1", time.Unix(1, 0), 1, 1, 1, 0.1, "", 0, ""); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	// Project name 90 chars + huge counts → natural value width would
	// blow past 100. Triggers cap + truncation logic.
	longName := strings.Repeat("x", 90)
	if err := store.UpsertProject(db.Project{
		ID: "/tmp/long", Path: "/tmp/long", Name: longName,
		FileCount: 9999999, SymCount: 999999999,
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	report, _ := buildStatsReport(store, dir)
	out := formatStatsText(report)

	// Box must close cleanly even at the cap.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	expectedWidth := len([]rune(lines[0]))
	for i, ln := range lines {
		if got := len([]rune(ln)); got != expectedWidth {
			t.Errorf("line %d width = %d runes, want %d (cap-truncation didn't keep box square):\n%s",
				i, got, expectedWidth, out)
		}
	}

	// Width must be at the cap, not unbounded.
	const maxBoxOuterWidth = 102 // 100 inner + 2 borders
	if expectedWidth > maxBoxOuterWidth {
		t.Errorf("box width %d exceeds cap of %d — truncation didn't engage:\n%s",
			expectedWidth, maxBoxOuterWidth, out)
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
			int64(10*(i+1)), 100, 1000, 0.1, "", 0, ""); err != nil {
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
	if err := store.RecordSession("s1", time.Unix(1, 0), 7, 70, 700, 0.21, "", 0, ""); err != nil {
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
		if err := store.RecordSession(id, time.Unix(int64(i), 0), 1, 1, 1, 0.1, "", 0, ""); err != nil {
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
	if err := store.RecordSession("s1", time.Unix(1, 0), 1, 1, 1, 0.1, "", 0, ""); err != nil {
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
	if err := store.RecordSession("s1", time.Unix(1, 0), 1, 0, 0, 0, "", 0, ""); err != nil {
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
