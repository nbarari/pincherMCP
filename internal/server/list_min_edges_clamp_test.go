package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1041: list silently accepted negative min_edges values. Downstream
// `if minEdges > 0` made them behave like 0, but the documented
// contract is non-negative and a typo'd negative deserves the same
// clamp warning limit/offset/active_within_days already give.

func TestHandleList_NegativeMinEdges_ClampsAndWarns(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	// Seed one project so list has something to consider.
	store.UpsertProject(db.Project{
		ID: "p-list-min-edges", Path: t.TempDir(), Name: "p-list-min-edges",
		IndexedAt: time.Now(), FileCount: 1, SymCount: 1, EdgeCount: 5,
	})
	srv.sessionID = "p-list-min-edges"

	result, err := srv.handleList(context.Background(), makeReq(map[string]any{
		"min_edges": float64(-5),
		"limit":     float64(2),
	}))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	foundWarn := false
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "min_edges=-5 clamped to 0") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("expected clamp warning for negative min_edges; got warnings=%v", warnings)
	}
}

// Control: a non-negative min_edges doesn't trip the clamp warning.
func TestHandleList_NonNegativeMinEdges_NoClampWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{
		ID: "p-list-min-edges-ok", Path: t.TempDir(), Name: "p-list-min-edges-ok",
		IndexedAt: time.Now(), FileCount: 1, SymCount: 1, EdgeCount: 5,
	})
	srv.sessionID = "p-list-min-edges-ok"

	result, err := srv.handleList(context.Background(), makeReq(map[string]any{
		"min_edges": float64(0),
	}))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "min_edges") && strings.Contains(s, "clamped") {
			t.Errorf("must not warn on non-negative min_edges; got %s", s)
		}
	}
}
