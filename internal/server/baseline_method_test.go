package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #477: every tool response stamps `_meta.baseline_method`. Three values:
//   - "full_file_read" — replaces a Read of source files (search/symbol/...)
//   - "partial_read"   — second access to the same file (per-session dedup)
//   - "none"           — admin tool with no Read/Grep alternative
//
// "none" tools must emit `tokens_saved: null` (not 0) and must NOT
// emit a `savings:` line. Stats accumulator must skip them.

// Admin tool (architecture) → baseline_method="none", tokens_saved=null,
// no savings line.
func TestBaselineMethod_AdminTool_StampsNoneAndNullsSaved(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.sessionID = "p1"

	req := makeReq(map[string]any{})
	req.Params.Name = "architecture"
	result, err := srv.handleArchitecture(context.Background(), req)
	if err != nil {
		t.Fatalf("handleArchitecture: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("missing _meta")
	}
	if got, _ := meta["baseline_method"].(string); got != "none" {
		t.Errorf("architecture baseline_method=%q; want %q", got, "none")
	}
	// tokens_saved should marshal to JSON null → unmarshal to nil.
	if v, present := meta["tokens_saved"]; !present || v != nil {
		t.Errorf("architecture tokens_saved should be null; got %v (present=%v)", v, present)
	}
	if _, present := meta["savings"]; present {
		t.Errorf("architecture must not emit `savings` line")
	}
}

// Read-replacement tool (symbol) → baseline_method="full_file_read",
// tokens_saved is a non-null int.
func TestBaselineMethod_ReadReplacementTool_StampsFullFileRead(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.sessionID = "p1"
	seedSimpleProject(t, srv)

	req := makeReq(map[string]any{
		"id": "p1::pkg.foo#Function",
	})
	req.Params.Name = "symbol"
	result, err := srv.handleSymbol(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("missing _meta")
	}
	if got, _ := meta["baseline_method"].(string); got != "full_file_read" {
		t.Errorf("symbol baseline_method=%q; want %q", got, "full_file_read")
	}
	// tokens_saved must be a number (non-null), even if zero.
	if v, present := meta["tokens_saved"]; !present {
		t.Fatalf("symbol must emit tokens_saved field")
	} else if _, isFloat := v.(float64); !isFloat {
		t.Errorf("symbol tokens_saved should be numeric; got %T (%v)", v, v)
	}
}

// Stats accumulator must skip "none" tools — calling architecture
// repeatedly must not inflate statsTokensSaved.
func TestBaselineMethod_NoneToolDoesNotAccumulateStats(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.sessionID = "p1"

	before := atomic.LoadInt64(&srv.statsTokensSaved)

	for i := 0; i < 5; i++ {
		req := makeReq(map[string]any{})
		req.Params.Name = "architecture"
		if _, err := srv.handleArchitecture(context.Background(), req); err != nil {
			t.Fatalf("handleArchitecture call %d: %v", i, err)
		}
	}

	after := atomic.LoadInt64(&srv.statsTokensSaved)
	if after != before {
		t.Errorf("architecture (baseline=none) must not accumulate statsTokensSaved; before=%d after=%d", before, after)
	}
}

// Pure-function unit tests on the lookup table — guards against
// classification drift when adding new tools.
func TestBaselineMethodForTool_KnownTools(t *testing.T) {
	cases := []struct {
		tool, want string
	}{
		{"symbol", "full_file_read"},
		{"symbols", "full_file_read"},
		{"context", "full_file_read"},
		{"search", "full_file_read"},
		{"query", "full_file_read"},
		{"trace", "full_file_read"},
		{"changes", "full_file_read"},
		{"dead_code", "full_file_read"},
		{"neighborhood", "full_file_read"},
		{"index", "none"},
		{"architecture", "none"},
		{"schema", "none"},
		{"list", "none"},
		{"adr", "none"},
		{"health", "none"},
		{"stats", "none"},
		{"fetch", "none"},
		{"guide", "none"},
		{"init", "none"},
	}
	for _, tc := range cases {
		got := baselineMethodForTool[tc.tool]
		if got != tc.want {
			t.Errorf("baselineMethodForTool[%q] = %q; want %q", tc.tool, got, tc.want)
		}
	}
}

// All registered tools must have a baseline classification — drift
// gate for future tool additions. Mirrors db's writerRoutedStoreMethods
// classification gate from #51.
func TestBaselineMethodForTool_AllRegisteredToolsClassified(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for name := range srv.tools {
		if _, ok := baselineMethodForTool[name]; !ok {
			t.Errorf("tool %q registered but missing from baselineMethodForTool — classify it as full_file_read / partial_read / none", name)
		}
	}
}

// seedSimpleProject plants one Function symbol under project "p1" so
// handleSymbol can resolve a known ID for the read-replacement test.
func seedSimpleProject(t *testing.T, srv *Server) {
	t.Helper()
	store := srv.store
	if err := store.UpsertProject(db.Project{
		ID: "p1", Path: t.TempDir(), Name: "p1",
		IndexedAt: time.Now(), EdgeCount: 1,
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p1::pkg.foo#Function", ProjectID: "p1", FilePath: "pkg/foo.go",
			Name: "foo", QualifiedName: "pkg.foo", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})
}
