package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1073: closes the ratio-class ghost-extraction series across the
// graph-tool surface (#1010 doctor, #1067 architecture, #1068 schema,
// #1071 dead_code). query's strict diagnosis only fires when rows is
// empty AND edges == 0. A ratio-class ghost project where the query
// happens to hit the resolved subset returns rows that don't represent
// the project. Warning fires regardless of rows count.

func TestHandleQuery_LowRatio_AttachesGhostWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-q-ratio"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
		FileCount: 50, SymCount: 5000, EdgeCount: 2,
	})
	srv.sessionID = pid

	syms := []db.Symbol{}
	for i := 0; i < 5000; i++ {
		syms = append(syms, db.Symbol{
			ID:                   pid + "::pkg.Q" + string(rune('A'+(i%26))) + string(rune('A'+(i/26))) + "#Function",
			ProjectID:            pid,
			FilePath:             "a.go",
			Name:                 "Q",
			QualifiedName:        "pkg.Q",
			Kind:                 "Function",
			Language:             "Go",
			ExtractionConfidence: 1.0,
		})
	}
	mustUpsertSymbols(t, store, syms)
	if err := store.BulkUpsertEdges([]db.Edge{
		{ProjectID: pid, FromID: syms[0].ID, ToID: syms[1].ID, Kind: "CALLS", Confidence: 1.0},
		{ProjectID: pid, FromID: syms[1].ID, ToID: syms[2].ID, Kind: "CALLS", Confidence: 1.0},
	}); err != nil {
		t.Fatalf("BulkUpsertEdges: %v", err)
	}

	// Query that returns rows so we hit the "edges > 0 but ratio bad"
	// branch (rows != 0). The bare-node MATCH ignores the edge table.
	res, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql": "MATCH (n:Function) RETURN n.name LIMIT 5",
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("expected _meta on ratio-ghost query; got %v", body)
	}
	warnings, _ := meta["warnings"].([]any)
	found := false
	for _, w := range warnings {
		ws, _ := w.(string)
		if strings.Contains(ws, "ratio") && strings.Contains(ws, "ghost-extraction") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ratio-ghost warning; got warnings=%v", warnings)
	}
}
