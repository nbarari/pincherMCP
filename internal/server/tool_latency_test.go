package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// #1630 v0.85: per-tool latency aggregation tests.

func TestRecordToolLatency_AccumulatesAcrossCalls_1630(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	srv.recordToolLatency("search", 10)
	srv.recordToolLatency("search", 20)
	srv.recordToolLatency("search", 30)
	srv.recordToolLatency("trace", 100)

	rows := srv.topToolsByTotalTime(5)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// trace has the higher total (100 vs 60), so it sorts first.
	if rows[0].Tool != "trace" {
		t.Errorf("rows[0].Tool=%q, want trace (higher total)", rows[0].Tool)
	}
	if rows[1].Tool != "search" {
		t.Errorf("rows[1].Tool=%q, want search", rows[1].Tool)
	}
	if rows[1].Count != 3 || rows[1].TotalMs != 60 {
		t.Errorf("search row: count=%d totalMs=%d, want 3 / 60", rows[1].Count, rows[1].TotalMs)
	}
}

func TestRecordToolLatency_TracksMax_1630(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	srv.recordToolLatency("search", 50)
	srv.recordToolLatency("search", 200) // max
	srv.recordToolLatency("search", 100)

	rows := srv.topToolsByTotalTime(5)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].MaxMs != 200 {
		t.Errorf("maxMs=%d, want 200 (highest of {50,200,100})", rows[0].MaxMs)
	}
}

func TestRecordToolLatency_EmptyToolName_NoOp_1630(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	srv.recordToolLatency("", 100)
	srv.recordToolLatency("search", 50)
	rows := srv.topToolsByTotalTime(5)
	if len(rows) != 1 {
		t.Fatalf("empty tool name should not be recorded; got %d rows", len(rows))
	}
	if rows[0].Tool != "search" {
		t.Errorf("got %q, want search only", rows[0].Tool)
	}
}

func TestTopToolsByTotalTime_TopNCap_1630(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	for i, tool := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		// Reverse-correlate latency so "g" tops the list.
		srv.recordToolLatency(tool, int64(10*(i+1)))
	}
	rows := srv.topToolsByTotalTime(3)
	if len(rows) != 3 {
		t.Errorf("got %d rows for n=3, want 3", len(rows))
	}
	if rows[0].Tool != "g" {
		t.Errorf("rows[0].Tool=%q, want g (highest total)", rows[0].Tool)
	}
}

func TestTopToolsByTotalTime_StableTieBreak_1630(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	// Two tools with identical totals — secondary sort is by count
	// desc (more calls implies more representative data), tertiary
	// by name. Same total + same count → alphabetical.
	srv.recordToolLatency("zeta", 50)
	srv.recordToolLatency("zeta", 50)
	srv.recordToolLatency("alpha", 100) // same total, fewer calls
	rows := srv.topToolsByTotalTime(2)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// zeta has more calls (2 vs 1), sorts first under the secondary rule.
	if rows[0].Tool != "zeta" {
		t.Errorf("rows[0].Tool=%q, want zeta (more calls wins on totalMs tie)", rows[0].Tool)
	}
}

// Integration: handleStats renders the BY TOOL section when there's
// per-tool data. Verifies the new section appears AFTER the existing
// SESSION/ALL-TIME/PROJECT sections without disrupting them.
func TestHandleStats_IncludesByToolSection_1630(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	// Seed some per-tool latency data manually.
	srv.recordToolLatency("search", 100)
	srv.recordToolLatency("trace", 250)
	srv.recordToolLatency("search", 50)

	body, _ := json.Marshal(map[string]any{})
	resp, err := srv.handleStats(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "stats", Arguments: body},
	})
	if err != nil || resp.IsError {
		t.Fatalf("handleStats: err=%v isErr=%v", err, resp.IsError)
	}
	text := textOf(t, resp)
	if !strings.Contains(text, "BY TOOL") {
		t.Errorf("stats output missing BY TOOL section:\n%s", text)
	}
	// #1645 v0.86: row format changed from "trace:" labels to
	// column-aligned cells. Match on the bare tool name as it appears
	// in the column.
	if !strings.Contains(text, "search") || !strings.Contains(text, "trace") {
		t.Errorf("stats output missing per-tool rows:\n%s", text)
	}
	// trace has higher total (250 > 150) so it should appear before search.
	traceIdx := strings.Index(text, "trace ")
	searchIdx := strings.Index(text, "search ")
	if traceIdx < 0 || searchIdx < 0 || traceIdx > searchIdx {
		t.Errorf("trace should appear before search (higher total); traceIdx=%d searchIdx=%d", traceIdx, searchIdx)
	}
}

// Integration: jsonResultWithMeta records latency on every successful
// call. Uses handleHealth as the probe because it always goes through
// the jsonResultWithMeta happy path (other handlers may bail out via
// errResultRich which bypasses the latency hook by design — that
// path's own latency is tracked separately and isn't in scope for
// the "BY TOOL" surface).
func TestJsonResultWithMeta_RecordsToolLatency_1630(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	body, _ := json.Marshal(map[string]any{})
	srv.handleHealth(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "health", Arguments: body},
	})

	rows := srv.topToolsByTotalTime(5)
	if len(rows) == 0 {
		t.Fatal("topToolsByTotalTime returned no rows after a health call")
	}
	var healthRow *toolLatencyRow
	for i := range rows {
		if rows[i].Tool == "health" {
			healthRow = &rows[i]
			break
		}
	}
	if healthRow == nil {
		t.Errorf("health not in top tools after handleHealth call; got %+v", rows)
	} else if healthRow.Count < 1 {
		t.Errorf("health count=%d, want >= 1", healthRow.Count)
	}
}
