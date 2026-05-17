package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #635 v0.67: HTTP integration test for /v1/tool-call-stats. Validates
// the endpoint round-trips the DB aggregation through JSON correctly
// and rejects non-GET methods (per #609's GET-only contract).

func TestToolCallStatsHTTP_ReturnsAggregatedRows(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)

	now := time.Now()
	saved := int64(1500)
	pct := 88.5
	if err := store.RecordToolCalls([]db.ToolCallEvent{
		{SessionID: "s1", Tool: "search", TS: now, TokensUsed: 100, TokensSaved: &saved, TokensSavedPct: &pct},
		{SessionID: "s1", Tool: "search", TS: now.Add(-1 * time.Minute), TokensUsed: 120, TokensSaved: &saved, TokensSavedPct: &pct},
	}); err != nil {
		t.Fatalf("RecordToolCalls: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/tool-call-stats", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /v1/tool-call-stats returned %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	tallies, ok := resp["tallies"].([]any)
	if !ok {
		t.Fatalf("response missing tallies array; got %v", resp)
	}
	if len(tallies) != 1 {
		t.Errorf("expected 1 tool (search); got %d (%v)", len(tallies), tallies)
	}
	if window, _ := resp["window_seconds"].(float64); window != 604800 {
		t.Errorf("default window_seconds = %v; want 604800 (7 days)", window)
	}
}

// Window-clamp via query param.
func TestToolCallStatsHTTP_HonorsWindowParam(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	now := time.Now()
	saved := int64(100)
	store.RecordToolCalls([]db.ToolCallEvent{
		{SessionID: "s1", Tool: "search", TS: now.Add(-3 * time.Hour), TokensUsed: 100, TokensSaved: &saved},
	})

	// 1-hour window excludes the 3-hour-old row.
	req := httptest.NewRequest("GET", "/v1/tool-call-stats?window_seconds=3600", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	tallies, _ := resp["tallies"].([]any)
	if len(tallies) != 0 {
		t.Errorf("expected 0 rows with 1-hour window (event was 3h old); got %d", len(tallies))
	}
}

// Negative: POST is rejected per the GET-only contract.
func TestToolCallStatsHTTP_PostRejected(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/v1/tool-call-stats", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 404 && rr.Code != 405 {
		t.Errorf("expected 404/405 on POST; got %d", rr.Code)
	}
}

// Control: empty store yields a zero-len tallies array, not null.
// Matches the #330 invariant; dashboard relies on `.length === 0`
// rather than null checks.
func TestToolCallStatsHTTP_EmptyStoreReturnsArrayNotNull(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/v1/tool-call-stats", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !containsSubstr(body, `"tallies":[]`) {
		t.Errorf("expected zero-len tallies array; got body: %s", body)
	}
}

// #635 panel 3: HTTP smoke for /v1/tool-payload-stats.
// Same shape as the per-tool endpoint — GET-only, sorted DESC by
// max_bytes server-side, zero-len array on empty.
func TestToolPayloadStatsHTTP_ReturnsRowsSortedByMaxBytes(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	now := time.Now()
	store.RecordToolCalls([]db.ToolCallEvent{
		{SessionID: "s1", Tool: "search", TS: now, TokensUsed: 1, ResponseBytes: 500},
		{SessionID: "s1", Tool: "guide", TS: now, TokensUsed: 1, ResponseBytes: 50000},
		{SessionID: "s1", Tool: "symbol", TS: now, TokensUsed: 1, ResponseBytes: 200},
	})

	req := httptest.NewRequest("GET", "/v1/tool-payload-stats", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	tallies, ok := resp["tallies"].([]any)
	if !ok || len(tallies) != 3 {
		t.Fatalf("expected 3 rows; got %d (%v)", len(tallies), tallies)
	}
	first, _ := tallies[0].(map[string]any)
	if first["tool"] != "guide" {
		t.Errorf("expected guide first (highest max_bytes); got %v", first)
	}
}

func TestToolPayloadStatsHTTP_PostRejected(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/v1/tool-payload-stats", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 404 && rr.Code != 405 {
		t.Errorf("expected 404/405 on POST; got %d", rr.Code)
	}
}

func TestToolPayloadStatsHTTP_EmptyStoreReturnsArrayNotNull(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/v1/tool-payload-stats", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !containsSubstr(body, `"tallies":[]`) {
		t.Errorf("expected zero-len tallies array; got body: %s", body)
	}
}
