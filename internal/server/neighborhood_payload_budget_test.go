package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1442 part 2 v0.72: neighborhood's limit clamp doesn't
// prevent the response payload from exceeding the MCP per-call
// token cap. Real repro: limit=200 on db.go's 176 neighbors
// returned a hard "exceeds maximum allowed tokens" error with
// no recovery path — pre-fix the user had to know to manually
// page in chunks of 50, AND that limit=500 (the documented max)
// was actively dangerous.
//
// Token-aware truncation tracks running byte cost while building
// entries and stops before the response blows the budget.
// Truncation surfaces in _meta.warnings + _meta.next_steps with
// the offset to continue from — same shape as the existing
// pagination next_steps.

func setupBudgetFixture(t *testing.T, n int, sigLen int) (*Server, string, db.Symbol) {
	t.Helper()
	srv, store, _ := newTestServer(t)
	pid := "pb"
	if err := store.UpsertProject(db.Project{ID: pid, Path: "/tmp/pb", Name: "pb"}); err != nil {
		t.Fatal(err)
	}
	bigSig := strings.Repeat("X", sigLen)
	syms := make([]db.Symbol, 0, n)
	for i := 0; i < n; i++ {
		syms = append(syms, db.Symbol{
			ID:                   "f.go::pkg.S" + strings.Repeat("y", i%5) + intToStr(i) + "#Function",
			ProjectID:            pid,
			FilePath:             "f.go",
			Name:                 "S" + intToStr(i),
			QualifiedName:        "pkg.S" + intToStr(i),
			Kind:                 "Function",
			Language:             "Go",
			Signature:            "func S" + intToStr(i) + "() { /* " + bigSig + " */ }",
			StartByte:            i * 1000,
			EndByte:              i*1000 + 800,
			StartLine:            i*20 + 1,
			EndLine:              i*20 + 18,
			ExtractionConfidence: 1.0,
		})
	}
	if err := store.BulkUpsertSymbols(syms); err != nil {
		t.Fatal(err)
	}
	srv.sessionID = pid
	return srv, pid, syms[0]
}

func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	out := ""
	for i > 0 {
		out = string(rune('0'+i%10)) + out
		i /= 10
	}
	return out
}

// Positive — when the projected payload would exceed budget,
// truncation fires and the response carries a clamp warning +
// next_steps for the continuation offset, instead of returning
// the hard MCP token-cap error.
func TestHandleNeighborhood_PayloadBudget_TruncatesWithWarning(t *testing.T) {
	t.Parallel()
	// 50 neighbors × ~1KB signature each = ~50KB worst-case;
	// budget is 18KB so truncation MUST fire before reaching the
	// end of the requested 50-entry window.
	srv, _, seed := setupBudgetFixture(t, 50, 1000)

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":    seed.ID,
		"limit": 50,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		t.Fatalf("payload-budget truncation should yield a partial response, not an error: %s", textOf(t, res))
	}
	body := decode(t, res)
	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) == 0 {
		t.Fatalf("expected partial neighbors; got 0")
	}
	if len(neighbors) >= 49 {
		t.Errorf("expected truncation; got %d neighbors out of 49 (almost-full means budget didn't fire)", len(neighbors))
	}
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	sawTruncationWarning := false
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "payload budget") || strings.Contains(s, "truncated") {
			sawTruncationWarning = true
			break
		}
	}
	if !sawTruncationWarning {
		t.Errorf("truncation should surface a clamp warning naming payload budget; got warnings=%v", warnings)
	}
	// next_steps should point at the continuation offset.
	steps, _ := meta["next_steps"].([]any)
	if len(steps) == 0 {
		t.Errorf("truncated response should carry next_steps for pagination; got none")
	}
}

// Control — small fixtures stay un-truncated. The budget guard
// must not fire when the response fits.
func TestHandleNeighborhood_PayloadBudget_SmallFixtureNoTruncation(t *testing.T) {
	t.Parallel()
	// 5 neighbors × ~50-byte signature = ~500 bytes total; well
	// inside the 18KB budget.
	srv, _, seed := setupBudgetFixture(t, 5, 50)

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":    seed.ID,
		"limit": 50,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, res)
	neighbors, _ := body["neighbors"].([]any)
	// 5 total - 1 self = 4 returned (include_self defaults false).
	if len(neighbors) != 4 {
		t.Errorf("small fixture should not truncate; got %d, want 4 (5 symbols - 1 self)", len(neighbors))
	}
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "payload budget") || strings.Contains(s, "truncated") {
			t.Errorf("small fixture should NOT carry truncation warning; got %q", s)
		}
	}
}

// Cross-check — fields projection (Part 1) and token-aware
// truncation (Part 2) compose correctly. With fields=id only,
// each entry is much smaller, so MORE entries fit in the same
// budget than with the default heavy shape.
func TestHandleNeighborhood_PayloadBudget_FieldsProjectionLetsMoreFit(t *testing.T) {
	t.Parallel()
	srv, _, seed := setupBudgetFixture(t, 50, 1000)

	// Heavy default shape — truncates early.
	resDefault, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":    seed.ID,
		"limit": 50,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood default: %v", err)
	}
	defaultCount := len(decode(t, resDefault)["neighbors"].([]any))

	// Lean fields-projected shape — should fit more (or all).
	resLean, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":     seed.ID,
		"limit":  50,
		"fields": "id,name,kind",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood lean: %v", err)
	}
	leanCount := len(decode(t, resLean)["neighbors"].([]any))

	if leanCount <= defaultCount {
		t.Errorf("fields projection should let more entries fit in the budget; got default=%d lean=%d", defaultCount, leanCount)
	}
}

// Cross-check — first entry always returns even if it alone
// exceeds the budget. Without this, the response would be empty
// with no useful information; the caller gets at least one
// entry plus the truncation hint, matching the existing "always
// return at least one row when possible" pattern.
func TestHandleNeighborhood_PayloadBudget_AlwaysReturnsAtLeastOneEntry(t *testing.T) {
	t.Parallel()
	// One neighbor with a signature larger than the entire
	// budget. Pre-#1442p2 logic would crash on the token cap;
	// the truncation loop's `len(neighbors) > 0` guard means
	// at least the first entry surfaces.
	srv, _, seed := setupBudgetFixture(t, 3, 25000)

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":    seed.ID,
		"limit": 50,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, res)
	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) == 0 {
		t.Errorf("at least one entry must surface even on huge-entry fixtures (got 0)")
	}
}
