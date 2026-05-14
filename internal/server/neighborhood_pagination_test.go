package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #293: large files (server.go has 114 symbols) used to dump every
// neighbor and blow the response budget. The handler now paginates
// (default limit=50) and surfaces the next page in _meta.next_steps.

// setupBigNeighborhood seeds N symbols in one file so a default-limit
// call returns a partial slice and triggers the next-page path.
func setupBigNeighborhood(t *testing.T, n int) (*Server, *db.Store, string) {
	t.Helper()
	srv, store, _ := newTestServer(t)
	projectID := "big-neighborhood"
	store.UpsertProject(db.Project{
		ID: projectID, Path: "/tmp/" + projectID, Name: projectID, IndexedAt: time.Now(),
	})
	srv.sessionID = projectID

	syms := make([]db.Symbol, 0, n)
	for i := 0; i < n; i++ {
		syms = append(syms, db.Symbol{
			ID:                   fmt.Sprintf("big::main.F%d#Function", i),
			ProjectID:            projectID,
			FilePath:             "main.go",
			Name:                 fmt.Sprintf("F%d", i),
			QualifiedName:        fmt.Sprintf("main.F%d", i),
			Kind:                 "Function",
			Language:             "Go",
			StartByte:            i * 100,
			EndByte:              i*100 + 50,
			StartLine:            i + 1,
			EndLine:              i + 1,
			Signature:            fmt.Sprintf("func F%d()", i),
			IsExported:           true,
			ExtractionConfidence: 1.0,
		})
	}
	if err := store.BulkUpsertSymbols(syms); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	return srv, store, projectID
}

// Default limit (50) caps the response on a 100-symbol file. count
// reports the total (99 — seed excluded by default), neighbors holds
// the first 50.
func TestHandleNeighborhood_DefaultLimitPaginatesBigFiles(t *testing.T) {
	t.Parallel()
	srv, _, _ := setupBigNeighborhood(t, 100)
	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "big::main.F0#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)

	count, _ := body["count"].(float64)
	if int(count) != 99 {
		t.Errorf("count = %v, want 99 (total in file minus seed)", count)
	}

	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) != 50 {
		t.Errorf("len(neighbors) = %d, want 50 (default limit)", len(neighbors))
	}

	// next_steps must surface the next page.
	meta, _ := body["_meta"].(map[string]any)
	steps, _ := meta["next_steps"].([]any)
	if len(steps) == 0 {
		t.Fatalf("expected _meta.next_steps to surface the next page, got: %v", meta)
	}
	step, _ := steps[0].(map[string]any)
	if step["tool"] != "neighborhood" {
		t.Errorf("next_steps[0].tool = %v, want neighborhood", step["tool"])
	}
	args, _ := step["args"].(string)
	if !strings.Contains(args, `"offset":50`) {
		t.Errorf("next_steps args should advance offset to 50, got: %s", args)
	}
}

// Explicit limit + offset returns the requested window. Verifies the
// pagination math without involving the default.
func TestHandleNeighborhood_ExplicitOffsetReturnsWindow(t *testing.T) {
	t.Parallel()
	srv, _, _ := setupBigNeighborhood(t, 100)
	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":     "big::main.F0#Function",
		"limit":  20,
		"offset": 50,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)

	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) != 20 {
		t.Errorf("len(neighbors) = %d, want 20", len(neighbors))
	}

	page, _ := body["page"].(map[string]any)
	if page["offset"].(float64) != 50 {
		t.Errorf("page.offset = %v, want 50", page["offset"])
	}
	if page["limit"].(float64) != 20 {
		t.Errorf("page.limit = %v, want 20", page["limit"])
	}
}

// Tail page: limit=20, offset=90 returns 9 (after seed exclusion at
// index 0, neighbors 1..99 indexed 0..98 of the filtered slice).
// next_steps should NOT surface — there's nothing past the window.
func TestHandleNeighborhood_TailPageHasNoNextStep(t *testing.T) {
	t.Parallel()
	srv, _, _ := setupBigNeighborhood(t, 100)
	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":     "big::main.F0#Function",
		"offset": 90,
		"limit":  20,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)

	neighbors, _ := body["neighbors"].([]any)
	// 99 total filtered; offset 90 → 9 returned.
	if len(neighbors) != 9 {
		t.Errorf("len(neighbors) = %d, want 9 (tail page)", len(neighbors))
	}
	meta, _ := body["_meta"].(map[string]any)
	if steps, ok := meta["next_steps"].([]any); ok && len(steps) > 0 {
		// next_steps may carry other entries (e.g. from jsonResultWithMeta);
		// what matters is no neighborhood-pagination step.
		for _, s := range steps {
			step, _ := s.(map[string]any)
			if step["tool"] == "neighborhood" {
				t.Errorf("tail page shouldn't have a neighborhood next_step: %v", step)
			}
		}
	}
}

// Out-of-range offset clamps to a zero-length window without erroring.
// The agent might compute a stale offset off an old `count`.
func TestHandleNeighborhood_OutOfRangeOffsetReturnsEmpty(t *testing.T) {
	t.Parallel()
	srv, _, _ := setupBigNeighborhood(t, 10)
	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":     "big::main.F0#Function",
		"offset": 999,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)
	if result.IsError {
		t.Fatalf("out-of-range offset should not error: %v", body)
	}
	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) != 0 {
		t.Errorf("len(neighbors) = %d, want 0 (offset past end)", len(neighbors))
	}
	count, _ := body["count"].(float64)
	if int(count) != 9 {
		t.Errorf("count = %v, want 9 (still reports total)", count)
	}
}

// Negative limit / offset clamp to defaults — defensive against a
// caller passing 0 or -1.
func TestHandleNeighborhood_NegativeArgsClampToDefaults(t *testing.T) {
	t.Parallel()
	srv, _, _ := setupBigNeighborhood(t, 10)
	result, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":     "big::main.F0#Function",
		"limit":  0,
		"offset": -5,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, result)
	page, _ := body["page"].(map[string]any)
	if page["limit"].(float64) != 50 {
		t.Errorf("page.limit = %v, want 50 (default after clamping zero)", page["limit"])
	}
	if page["offset"].(float64) != 0 {
		t.Errorf("page.offset = %v, want 0 (clamped from negative)", page["offset"])
	}
}

// #712: neighborhood clamps limit<=0 and offset<0 silently. The clamp
// must now surface in _meta.warnings — same treatment as search/list/trace.
func TestHandleNeighborhood_NegativeInputsWarn(t *testing.T) {
	t.Parallel()
	srv, _, _ := setupBigNeighborhood(t, 20)
	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":     "big::main.F0#Function",
		"limit":  -3,
		"offset": -5,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success; got error %s", textOf(t, res))
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warns, _ := meta["warnings"].([]any)
	if len(warns) < 2 {
		t.Fatalf("expected 2 clamp warnings (limit + offset); got %v", meta)
	}
	joined := fmt.Sprint(warns...)
	if !strings.Contains(joined, "limit") || !strings.Contains(joined, "offset") {
		t.Errorf("warnings should name both limit and offset; got %v", warns)
	}
	// The clamp still happened — page reflects the corrected values.
	page, _ := body["page"].(map[string]any)
	if page["limit"] != float64(50) || page["offset"] != float64(0) {
		t.Errorf("clamped page should be limit=50 offset=0; got %v", page)
	}
}
