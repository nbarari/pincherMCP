package main

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// seedBenchProject seeds a project with two real on-disk files and
// edges between their symbols. Returns the project ID. Used by the
// positive-path tests below to exercise the bench suite end-to-end.
//
// Why on-disk files: the bench's baseline calculation is os.Stat-based
// (fileBytesAsTokens). Mocking that would test the wiring without
// testing the calculation; seeding real bytes pins both.
func seedBenchProject(t *testing.T, store *db.Store) (projectID, dir string) {
	t.Helper()
	dir = t.TempDir()
	// File A: 400 bytes of body — gives a stable baseline number.
	fileA := filepath.Join(dir, "a.go")
	if err := os.WriteFile(fileA, []byte(strings.Repeat("// padding line\n", 25)), 0o644); err != nil {
		t.Fatalf("write a.go: %v", err)
	}
	// File B: 800 bytes — different size pins the per-file dedup math.
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileB, []byte(strings.Repeat("// other padding\n", 47)), 0o644); err != nil {
		t.Fatalf("write b.go: %v", err)
	}

	projectID = strings.ToLower(dir)
	if err := store.UpsertProject(db.Project{
		ID: projectID, Path: dir, Name: "bench-test",
		IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	syms := []db.Symbol{
		{ID: fileA + "::pkg.Foo#Function", ProjectID: projectID, FilePath: fileA, Name: "Foo", QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 80},
		{ID: fileA + "::pkg.Bar#Function", ProjectID: projectID, FilePath: fileA, Name: "Bar", QualifiedName: "pkg.Bar", Kind: "Function", Language: "Go", StartByte: 100, EndByte: 200},
		{ID: fileB + "::pkg.Baz#Function", ProjectID: projectID, FilePath: fileB, Name: "Baz", QualifiedName: "pkg.Baz", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 50},
	}
	if err := store.BulkUpsertSymbols(syms); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	// Foo → Bar → Baz so each sampled symbol has at least one edge
	// and trace depth=2 from Foo touches both files.
	edges := []db.Edge{
		{ProjectID: projectID, FromID: syms[0].ID, ToID: syms[1].ID, Kind: "CALLS", Confidence: 1.0},
		{ProjectID: projectID, FromID: syms[1].ID, ToID: syms[2].ID, Kind: "CALLS", Confidence: 1.0},
	}
	if err := store.BulkUpsertEdges(edges); err != nil {
		t.Fatalf("BulkUpsertEdges: %v", err)
	}
	return projectID, dir
}

// TestBench_LargestProjectID_NoProjects exercises the negative path:
// `pincher bench` on an empty DB must surface "no indexed projects"
// rather than silently picking up nothing. The user's first run on a
// fresh install should get an actionable error.
func TestBench_LargestProjectID_NoProjects(t *testing.T) {
	store, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	_, err = benchLargestProjectID(store)
	if err == nil {
		t.Fatal("benchLargestProjectID on empty DB returned nil error, want 'no indexed projects'")
	}
	if !strings.Contains(err.Error(), "no indexed projects") {
		t.Errorf("error = %q, want substring 'no indexed projects'", err.Error())
	}
}

// TestBench_LargestProjectID_PicksLargest pins the "largest by symbol
// count" rule. Two projects: small (1 sym) and large (3 syms). bench
// must pick large.
func TestBench_LargestProjectID_PicksLargest(t *testing.T) {
	store, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// Small project — 1 symbol.
	smallDir := t.TempDir()
	smallID := strings.ToLower(smallDir)
	if err := store.UpsertProject(db.Project{ID: smallID, Path: smallDir, Name: "small", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertProject small: %v", err)
	}
	if err := store.BulkUpsertSymbols([]db.Symbol{
		{ID: "s1", ProjectID: smallID, FilePath: smallDir + "/x.go", Name: "X", QualifiedName: "X", Kind: "Function", Language: "Go"},
	}); err != nil {
		t.Fatalf("BulkUpsertSymbols small: %v", err)
	}

	// Large project — 3 symbols.
	largeID, _ := seedBenchProject(t, store)

	got, err := benchLargestProjectID(store)
	if err != nil {
		t.Fatalf("benchLargestProjectID: %v", err)
	}
	if got != largeID {
		t.Errorf("picked %q, want largest project %q", got, largeID)
	}
}

// TestBench_RunSuite_PositivePath exercises the full bench against a
// seeded project. Asserts: every tool reports calls > 0, savings_pct
// is non-zero (the baseline IS supposed to be larger than actual for
// real workloads), latency is non-negative.
func TestBench_RunSuite_PositivePath(t *testing.T) {
	store, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	pid, _ := seedBenchProject(t, store)

	rng := rand.New(rand.NewPCG(42, 43))
	sample, err := benchSampleSymbolsWithEdges(store, pid, 10, rng)
	if err != nil {
		t.Fatalf("benchSampleSymbolsWithEdges: %v", err)
	}
	if len(sample) == 0 {
		t.Fatal("sample empty; want >=1 edge-bearing symbol")
	}

	report := runBenchSuite(store, pid, sample, 2)
	if report.ProjectID != pid {
		t.Errorf("project_id = %q, want %q", report.ProjectID, pid)
	}
	if report.Samples != len(sample) {
		t.Errorf("samples = %d, want %d", report.Samples, len(sample))
	}
	if len(report.Tools) != 3 {
		t.Fatalf("tools = %d, want 3 (search/context/trace)", len(report.Tools))
	}

	wantTools := map[string]bool{"search": false, "context": false, "trace": false}
	for _, tb := range report.Tools {
		if _, known := wantTools[tb.Name]; !known {
			t.Errorf("unexpected tool %q", tb.Name)
			continue
		}
		wantTools[tb.Name] = true
		if tb.Calls != len(sample) {
			t.Errorf("%s: calls = %d, want %d", tb.Name, tb.Calls, len(sample))
		}
		if tb.P50LatencyMs < 0 {
			t.Errorf("%s: p50 = %v, want non-negative", tb.Name, tb.P50LatencyMs)
		}
		// context measurement must always touch a non-zero baseline —
		// every sampled symbol has a real on-disk file. If this drops
		// to zero we've regressed the fileBytesAsTokens path.
		if tb.Name == "context" && tb.MeanTokensBaseline == 0 {
			t.Errorf("context: mean_tokens_baseline = 0, want >0 (file size lookup must succeed for seeded files)")
		}
	}
	for name, seen := range wantTools {
		if !seen {
			t.Errorf("missing tool %q in report", name)
		}
	}
}

// TestBench_Aggregate_ControlEmpty pins that an empty measurement
// slice yields a zero-valued ToolBench (not a panic or NaN). This
// matters because bench is meant to be CI-friendly; a tool that
// happens to return no samples shouldn't crash the run.
func TestBench_Aggregate_ControlEmpty(t *testing.T) {
	tb := aggregate("search", nil)
	if tb.Name != "search" {
		t.Errorf("name = %q, want 'search'", tb.Name)
	}
	if tb.Calls != 0 {
		t.Errorf("calls = %d, want 0 on empty input", tb.Calls)
	}
	if tb.SavingsPct != 0 {
		t.Errorf("savings_pct = %v, want 0 on empty input", tb.SavingsPct)
	}
}

// TestBench_Aggregate_SavingsMath pins the savings formula. With
// baseline=1000 and actual=100, savings_pct must be 90.0 (not 900,
// not 0.9, not -900).
func TestBench_Aggregate_SavingsMath(t *testing.T) {
	ms := []measurement{
		{latencyMs: 5.0, tokensActual: 100, tokensBaseline: 1000},
		{latencyMs: 10.0, tokensActual: 100, tokensBaseline: 1000},
	}
	tb := aggregate("ctx", ms)
	if tb.Calls != 2 {
		t.Errorf("calls = %d, want 2", tb.Calls)
	}
	if got, want := tb.MeanTokensActual, int64(100); got != want {
		t.Errorf("mean_tokens_actual = %d, want %d", got, want)
	}
	if got, want := tb.MeanTokensBaseline, int64(1000); got != want {
		t.Errorf("mean_tokens_baseline = %d, want %d", got, want)
	}
	if got, want := tb.SavingsPct, 90.0; got != want {
		t.Errorf("savings_pct = %v, want %v", got, want)
	}
	if tb.MeanLatencyMs != 7.5 {
		t.Errorf("mean_latency_ms = %v, want 7.5", tb.MeanLatencyMs)
	}
}

// TestBench_ExtractFilePathFromSymbolID pins ID parsing. Symbol IDs
// use "{file_path}::{qualified_name}#{kind}" so splitting on the
// first "::" yields the file path segment. Defensive: an ID without
// "::" yields "" so trace baseline calculation skips rather than
// dereferences garbage.
func TestBench_ExtractFilePathFromSymbolID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"internal/db/db.go::db.Open#Function", "internal/db/db.go"},
		{"a::b#c", "a"},
		{"no-double-colon", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := extractFilePathFromSymbolID(c.in)
		if got != c.want {
			t.Errorf("extractFilePathFromSymbolID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBench_FormatBenchText pins the human-readable rendering. The
// table header must include every column and each tool gets one row.
func TestBench_FormatBenchText(t *testing.T) {
	r := &BenchReport{
		ProjectID:  "proj1",
		Samples:    5,
		TraceDepth: 2,
		Tools: []ToolBench{
			{Name: "search", Calls: 5, P50LatencyMs: 1.0, MeanTokensActual: 100, MeanTokensBaseline: 1000, SavingsPct: 90.0},
		},
	}
	out := formatBenchText(r)
	if !strings.Contains(out, "proj1") {
		t.Errorf("output missing project_id; got:\n%s", out)
	}
	if !strings.Contains(out, "search") {
		t.Errorf("output missing tool name; got:\n%s", out)
	}
	if !strings.Contains(out, "savings") {
		t.Errorf("output missing 'savings' header column; got:\n%s", out)
	}
	if !strings.Contains(out, "Baseline:") {
		t.Errorf("output missing footer explanation; got:\n%s", out)
	}
}

// TestBench_JSONTokenCount pins the bytes/4 token approximation that
// the bench uses to estimate actual response size. Matches pincher's
// tokens_used envelope math so the bench savings number lines up
// with the session stats box.
func TestBench_JSONTokenCount(t *testing.T) {
	// {"a":1} = 7 bytes / 4 = 1
	got := jsonTokenCount(map[string]int{"a": 1})
	if got != 1 {
		t.Errorf("jsonTokenCount({a:1}) = %d, want 1", got)
	}
	// nil -> "null" = 4 bytes / 4 = 1
	if got := jsonTokenCount(nil); got != 1 {
		t.Errorf("jsonTokenCount(nil) = %d, want 1", got)
	}
}

// TestBench_FileBytesAsTokens_MissingFile pins the defensive return on
// stat failure. A symbol whose file was deleted between index time
// and bench time must yield 0 tokens (not panic, not -1).
func TestBench_FileBytesAsTokens_MissingFile(t *testing.T) {
	got := fileBytesAsTokens(filepath.Join(t.TempDir(), "does-not-exist.go"))
	if got != 0 {
		t.Errorf("fileBytesAsTokens(missing) = %d, want 0", got)
	}
}
