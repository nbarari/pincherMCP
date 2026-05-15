package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #875: min_confidence accepts any float, but extraction_confidence is
// in [0,1] — any value > 1.0 silently filtered every result with no
// signal. Same silent-confidently-wrong class as the trace `depth`
// clamp (#703). clampMinConfidence now clamps to 1.0 + emits a warning
// across all four affected handlers (search, query, trace, dead_code).

// warningsFromMeta pulls _meta.warnings off a decoded body.
func warningsFromMeta(body map[string]any) []any {
	meta, _ := body["_meta"].(map[string]any)
	w, _ := meta["warnings"].([]any)
	return w
}

func warningContains(warnings []any, needle string) bool {
	for _, w := range warnings {
		if s, ok := w.(string); ok && strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func setupSeededProject(t *testing.T) (*Server, string) {
	t.Helper()
	srv, store, _ := newTestServer(t)
	pid := "p-clamp"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::pkg.caller#Function", ProjectID: pid, FilePath: "f.go",
			Name: "caller", QualifiedName: "pkg.caller", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		{ID: pid + "::pkg.target#Function", ProjectID: pid, FilePath: "f.go",
			Name: "target", QualifiedName: "pkg.target", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})
	if err := store.BulkUpsertEdges([]db.Edge{
		{ProjectID: pid, FromID: pid + "::pkg.caller#Function", ToID: pid + "::pkg.target#Function",
			Kind: "CALLS", Confidence: 1},
	}); err != nil {
		t.Fatal(err)
	}
	return srv, pid
}

func TestHandleTrace_MinConfidenceOver1_ClampsAndWarns(t *testing.T) {
	t.Parallel()
	srv, _ := setupSeededProject(t)

	result, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":           "caller",
		"min_confidence": float64(2),
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, result)
	ws := warningsFromMeta(body)
	if !warningContains(ws, "min_confidence=2 clamped to 1.0") {
		t.Errorf("expected clamp warning on trace min_confidence=2; got warnings=%v", ws)
	}
}

func TestHandleDeadCode_MinConfidenceOver1_ClampsAndWarns(t *testing.T) {
	t.Parallel()
	srv, _ := setupSeededProject(t)

	result, err := srv.handleDeadCode(context.Background(), makeReq(map[string]any{
		"min_confidence": float64(5),
	}))
	if err != nil {
		t.Fatalf("handleDeadCode: %v", err)
	}
	body := decode(t, result)
	ws := warningsFromMeta(body)
	if !warningContains(ws, "min_confidence=5 clamped to 1.0") {
		t.Errorf("expected clamp warning on dead_code min_confidence=5; got warnings=%v", ws)
	}
}

func TestHandleQuery_MinConfidenceOver1_ClampsAndWarns(t *testing.T) {
	t.Parallel()
	srv, _ := setupSeededProject(t)

	result, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql":        `MATCH (n:Function) RETURN n.name`,
		"min_confidence": float64(3),
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, result)
	ws := warningsFromMeta(body)
	if !warningContains(ws, "min_confidence=3 clamped to 1.0") {
		t.Errorf("expected clamp warning on query min_confidence=3; got warnings=%v", ws)
	}
}

func TestHandleSearch_MinConfidenceOver1_ClampsAndWarns(t *testing.T) {
	t.Parallel()
	srv, _ := setupSeededProject(t)

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":          "caller",
		"min_confidence": float64(4),
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	ws := warningsFromMeta(body)
	if !warningContains(ws, "min_confidence=4 clamped to 1.0") {
		t.Errorf("expected clamp warning on search min_confidence=4; got warnings=%v", ws)
	}
}

// Control: in-range min_confidence on any handler does NOT trip the
// clamp warning.
func TestHandlers_InRangeMinConfidence_NoClampWarning(t *testing.T) {
	t.Parallel()
	srv, _ := setupSeededProject(t)

	result, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":           "caller",
		"min_confidence": 0.7,
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, result)
	ws := warningsFromMeta(body)
	if warningContains(ws, "clamped to 1.0") {
		t.Errorf("in-range min_confidence=0.7 must not warn; got %v", ws)
	}
}
