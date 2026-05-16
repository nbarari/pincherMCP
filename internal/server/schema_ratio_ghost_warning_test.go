package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1068: companion to #1067 (architecture ratio-ghost) and #1010
// (doctor ratio extension). The strict #1042 schema ghost diagnosis
// fires only on edgeCount == 0 — ratio-class ghosts (a handful of
// edges leak through the resolver) slip past. Same 0.001 threshold
// across all three tools so the ghost signature is consistent.

func TestHandleSchema_LowRatio_AttachesGhostWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-schema-ratio"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
		FileCount: 50, SymCount: 5000, EdgeCount: 2, // ratio 0.0004
	})
	srv.sessionID = pid

	// Seed enough symbols for GraphStats to return a non-zero symCount
	// AND a non-zero edgeCount via an edge below.
	syms := []db.Symbol{}
	for i := 0; i < 5000; i++ {
		syms = append(syms, db.Symbol{
			ID:                   pid + "::pkg.S" + string(rune('A'+(i%26))) + string(rune('A'+(i/26))) + "#Function",
			ProjectID:            pid,
			FilePath:             "a.go",
			Name:                 "S",
			QualifiedName:        "pkg.S",
			Kind:                 "Function",
			Language:             "Go",
			ExtractionConfidence: 1.0,
		})
	}
	mustUpsertSymbols(t, store, syms)
	// Seed exactly 2 edges so GraphStats reports edgeCount > 0 but the
	// ratio remains below 0.001.
	if err := store.BulkUpsertEdges([]db.Edge{
		{ProjectID: pid, FromID: syms[0].ID, ToID: syms[1].ID, Kind: "CALLS", Confidence: 1.0},
		{ProjectID: pid, FromID: syms[1].ID, ToID: syms[2].ID, Kind: "CALLS", Confidence: 1.0},
	}); err != nil {
		t.Fatalf("BulkUpsertEdges: %v", err)
	}

	res, err := srv.handleSchema(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleSchema: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("expected _meta envelope on ratio-ghost; got body=%v", body)
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
		t.Errorf("expected ratio-ghost warning naming the ratio + ghost-extraction signature; got warnings=%v", warnings)
	}
}
