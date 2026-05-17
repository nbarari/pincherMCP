package db

import (
	"path/filepath"
	"testing"
	"time"
)

// #635 v0.67: ToolCallStatsByTool — aggregation feed for the dashboard
// per-tool breakdown panel. Tests follow the positive + negative +
// control + cross-check pattern.

// Positive: aggregates calls per-tool across multiple rows.
func TestToolCallStatsByTool_AggregatesPerTool(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	now := time.Now()
	savedA := int64(1000)
	savedB := int64(2500)
	pctA := 90.5
	pctB := 95.0
	events := []ToolCallEvent{
		{SessionID: "s1", Tool: "search", ComplexityTier: "lite", ResponseBytes: 500, TokensUsed: 120, TokensSaved: &savedA, TokensSavedPct: &pctA, TS: now, RequestID: "r1"},
		{SessionID: "s1", Tool: "search", ComplexityTier: "lite", ResponseBytes: 600, TokensUsed: 130, TokensSaved: &savedB, TokensSavedPct: &pctB, TS: now.Add(-1 * time.Hour), RequestID: "r2"},
		{SessionID: "s2", Tool: "symbol", ComplexityTier: "lite", ResponseBytes: 300, TokensUsed: 80, TokensSaved: &savedA, TokensSavedPct: &pctA, TS: now.Add(-2 * time.Hour), RequestID: "r3"},
	}
	if err := store.RecordToolCalls(events); err != nil {
		t.Fatalf("RecordToolCalls: %v", err)
	}

	tallies, err := store.ToolCallStatsByTool(0, 0) // defaults
	if err != nil {
		t.Fatalf("ToolCallStatsByTool: %v", err)
	}
	if len(tallies) != 2 {
		t.Fatalf("expected 2 tool rows; got %d (%+v)", len(tallies), tallies)
	}
	// search has 2 calls; symbol has 1 → search comes first (DESC sort).
	if tallies[0].Tool != "search" {
		t.Errorf("expected search first (highest call_count); got %+v", tallies)
	}
	if tallies[0].CallCount != 2 {
		t.Errorf("search call_count = %d; want 2", tallies[0].CallCount)
	}
	if tallies[0].SumTokensSaved != savedA+savedB {
		t.Errorf("search sum_tokens_saved = %d; want %d", tallies[0].SumTokensSaved, savedA+savedB)
	}
	// avg_tokens_saved_pct = (90.5 + 95.0) / 2 = 92.75
	if got := tallies[0].AvgTokensSavedPct; got < 92.7 || got > 92.8 {
		t.Errorf("search avg_tokens_saved_pct = %v; want ~92.75", got)
	}
	if tallies[1].Tool != "symbol" || tallies[1].CallCount != 1 {
		t.Errorf("expected symbol second with 1 call; got %+v", tallies[1])
	}
}

// Negative: rows outside the window are excluded.
func TestToolCallStatsByTool_WindowExcludesOldRows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	now := time.Now()
	saved := int64(500)
	events := []ToolCallEvent{
		// Inside the 1-hour window.
		{SessionID: "s1", Tool: "search", TS: now.Add(-30 * time.Minute), TokensUsed: 100, TokensSaved: &saved},
		// Outside — should be excluded with a 1-hour window.
		{SessionID: "s1", Tool: "search", TS: now.Add(-3 * time.Hour), TokensUsed: 100, TokensSaved: &saved},
	}
	if err := store.RecordToolCalls(events); err != nil {
		t.Fatalf("RecordToolCalls: %v", err)
	}

	tallies, err := store.ToolCallStatsByTool(3600, 0) // 1-hour window
	if err != nil {
		t.Fatalf("ToolCallStatsByTool: %v", err)
	}
	if len(tallies) != 1 || tallies[0].CallCount != 1 {
		t.Errorf("expected 1 call in 1-hour window; got %+v", tallies)
	}
}

// Negative: empty session_tool_calls returns a zero-len slice, not nil.
// JSON marshalling depends on non-nil; matches the #330 invariant.
func TestToolCallStatsByTool_EmptyReturnsZeroLenSlice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	tallies, err := store.ToolCallStatsByTool(0, 0)
	if err != nil {
		t.Fatalf("ToolCallStatsByTool on empty store: %v", err)
	}
	if tallies == nil {
		t.Errorf("expected zero-len slice on empty store; got nil")
	}
	if len(tallies) != 0 {
		t.Errorf("expected 0 rows on empty store; got %d", len(tallies))
	}
}

// Control: limit caps the row count even when many tools have calls.
func TestToolCallStatsByTool_LimitCaps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	now := time.Now()
	events := []ToolCallEvent{
		{SessionID: "s1", Tool: "a", TS: now, TokensUsed: 1},
		{SessionID: "s1", Tool: "b", TS: now, TokensUsed: 1},
		{SessionID: "s1", Tool: "c", TS: now, TokensUsed: 1},
		{SessionID: "s1", Tool: "d", TS: now, TokensUsed: 1},
		{SessionID: "s1", Tool: "e", TS: now, TokensUsed: 1},
	}
	if err := store.RecordToolCalls(events); err != nil {
		t.Fatalf("RecordToolCalls: %v", err)
	}

	tallies, err := store.ToolCallStatsByTool(0, 3) // limit 3
	if err != nil {
		t.Fatalf("ToolCallStatsByTool: %v", err)
	}
	if len(tallies) != 3 {
		t.Errorf("expected 3 rows with limit=3; got %d", len(tallies))
	}
}

// Per-tier aggregate — mirror tests for ToolCallStatsByTier (#635 panel 2).
// Lighter coverage since the implementation is structurally identical to
// the per-tool query — these tests pin the tier-specific behaviour:
// empty-tier filtering and the lite/standard/heavy GROUP BY shape.

func TestToolCallStatsByTier_GroupsByTier(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	now := time.Now()
	saved := int64(500)
	events := []ToolCallEvent{
		{SessionID: "s1", Tool: "search", ComplexityTier: "lite", TS: now, TokensUsed: 100, TokensSaved: &saved},
		{SessionID: "s1", Tool: "symbol", ComplexityTier: "lite", TS: now, TokensUsed: 80, TokensSaved: &saved},
		{SessionID: "s1", Tool: "trace", ComplexityTier: "standard", TS: now, TokensUsed: 250, TokensSaved: &saved},
		{SessionID: "s1", Tool: "guide", ComplexityTier: "heavy", TS: now, TokensUsed: 400, TokensSaved: &saved},
	}
	if err := store.RecordToolCalls(events); err != nil {
		t.Fatalf("RecordToolCalls: %v", err)
	}

	tallies, err := store.ToolCallStatsByTier(0)
	if err != nil {
		t.Fatalf("ToolCallStatsByTier: %v", err)
	}
	if len(tallies) != 3 {
		t.Fatalf("expected 3 tier rows (lite/standard/heavy); got %d: %+v", len(tallies), tallies)
	}
	byTier := map[string]ToolCallTierTallyRow{}
	for _, r := range tallies {
		byTier[r.Tier] = r
	}
	if byTier["lite"].CallCount != 2 {
		t.Errorf("lite call_count = %d; want 2 (search + symbol)", byTier["lite"].CallCount)
	}
	if byTier["standard"].CallCount != 1 {
		t.Errorf("standard call_count = %d; want 1 (trace)", byTier["standard"].CallCount)
	}
	if byTier["heavy"].CallCount != 1 {
		t.Errorf("heavy call_count = %d; want 1 (guide)", byTier["heavy"].CallCount)
	}
}

func TestToolCallStatsByTier_EmptyTierRowsFiltered(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	now := time.Now()
	// One row with empty tier (pre-#1191 shape) — should NOT surface.
	// One row with a real tier — should surface.
	events := []ToolCallEvent{
		{SessionID: "s1", Tool: "old_tool", ComplexityTier: "", TS: now, TokensUsed: 50},
		{SessionID: "s1", Tool: "search", ComplexityTier: "lite", TS: now, TokensUsed: 100},
	}
	if err := store.RecordToolCalls(events); err != nil {
		t.Fatalf("RecordToolCalls: %v", err)
	}

	tallies, err := store.ToolCallStatsByTier(0)
	if err != nil {
		t.Fatalf("ToolCallStatsByTier: %v", err)
	}
	if len(tallies) != 1 || tallies[0].Tier != "lite" {
		t.Errorf("expected 1 row (lite); got %+v — empty-tier row should be filtered", tallies)
	}
}

// Cross-check: admin-shape tools without a Read/Grep baseline record
// NULL in tokens_saved_pct. The avg should exclude NULL rather than
// degrading toward zero on read-heavy sessions.
func TestToolCallStatsByTool_NullPctExcludedFromAvg(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	now := time.Now()
	savedA := int64(1000)
	pct := 80.0
	events := []ToolCallEvent{
		// One row with a real pct.
		{SessionID: "s1", Tool: "search", TS: now, TokensUsed: 100, TokensSaved: &savedA, TokensSavedPct: &pct},
		// One row with NIL pct (admin-shape).
		{SessionID: "s1", Tool: "search", TS: now.Add(-1 * time.Minute), TokensUsed: 100, TokensSaved: nil, TokensSavedPct: nil},
	}
	if err := store.RecordToolCalls(events); err != nil {
		t.Fatalf("RecordToolCalls: %v", err)
	}

	tallies, err := store.ToolCallStatsByTool(0, 0)
	if err != nil {
		t.Fatalf("ToolCallStatsByTool: %v", err)
	}
	if len(tallies) != 1 {
		t.Fatalf("expected 1 tool row; got %d", len(tallies))
	}
	// avg should be 80.0 (only the non-NULL row counted), NOT 40.0
	// (the average if NULL counted as 0).
	if got := tallies[0].AvgTokensSavedPct; got < 79.9 || got > 80.1 {
		t.Errorf("avg_tokens_saved_pct = %v; want ~80.0 (NULL pct row excluded)", got)
	}
}

// #635 v0.67 panel 3: ToolCallPayloadSizeByTool — surfaces the
// min/avg/max response_bytes per tool so the dashboard can flag
// outliers (tools whose max is many multiples of their avg are the
// occasional bill-blowers).

// Positive: per-tool min/avg/max derived correctly across multiple rows
// and sorted by max_bytes DESC.
func TestToolCallPayloadSizeByTool_SortedByMaxBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	now := time.Now()
	events := []ToolCallEvent{
		// "search" — moderate range, max 600.
		{SessionID: "s1", Tool: "search", TS: now, TokensUsed: 1, ResponseBytes: 100},
		{SessionID: "s1", Tool: "search", TS: now, TokensUsed: 1, ResponseBytes: 600},
		// "guide" — single huge outlier, max 50000.
		{SessionID: "s1", Tool: "guide", TS: now, TokensUsed: 1, ResponseBytes: 50000},
		// "symbol" — small payload, max 200.
		{SessionID: "s1", Tool: "symbol", TS: now, TokensUsed: 1, ResponseBytes: 200},
	}
	if err := store.RecordToolCalls(events); err != nil {
		t.Fatalf("RecordToolCalls: %v", err)
	}

	rows, err := store.ToolCallPayloadSizeByTool(0, 0)
	if err != nil {
		t.Fatalf("ToolCallPayloadSizeByTool: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 tool rows; got %d (%+v)", len(rows), rows)
	}
	// guide max=50000 → first; search max=600 → second; symbol max=200 → third.
	if rows[0].Tool != "guide" || rows[0].MaxBytes != 50000 {
		t.Errorf("expected guide/50000 first; got %+v", rows[0])
	}
	if rows[1].Tool != "search" || rows[1].MaxBytes != 600 || rows[1].MinBytes != 100 {
		t.Errorf("expected search min=100 max=600 second; got %+v", rows[1])
	}
	// search avg = (100+600)/2 = 350.
	if got := rows[1].AvgBytes; got < 349.9 || got > 350.1 {
		t.Errorf("search avg_bytes = %v; want ~350", got)
	}
	if rows[2].Tool != "symbol" || rows[2].MaxBytes != 200 {
		t.Errorf("expected symbol/200 third; got %+v", rows[2])
	}
}

// Negative: empty store returns []ToolCallPayloadRow{} not nil — JSON
// invariant the dashboard JS relies on.
func TestToolCallPayloadSizeByTool_EmptyReturnsZeroLenSlice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	rows, err := store.ToolCallPayloadSizeByTool(0, 0)
	if err != nil {
		t.Fatalf("ToolCallPayloadSizeByTool on empty store: %v", err)
	}
	if rows == nil {
		t.Errorf("expected zero-len slice on empty store; got nil")
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows on empty store; got %d", len(rows))
	}
}

// Control: limit caps row count.
func TestToolCallPayloadSizeByTool_LimitCaps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	now := time.Now()
	events := []ToolCallEvent{
		{SessionID: "s1", Tool: "a", TS: now, ResponseBytes: 500},
		{SessionID: "s1", Tool: "b", TS: now, ResponseBytes: 400},
		{SessionID: "s1", Tool: "c", TS: now, ResponseBytes: 300},
		{SessionID: "s1", Tool: "d", TS: now, ResponseBytes: 200},
	}
	if err := store.RecordToolCalls(events); err != nil {
		t.Fatalf("RecordToolCalls: %v", err)
	}

	rows, err := store.ToolCallPayloadSizeByTool(0, 2)
	if err != nil {
		t.Fatalf("ToolCallPayloadSizeByTool: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 rows with limit=2; got %d", len(rows))
	}
}
