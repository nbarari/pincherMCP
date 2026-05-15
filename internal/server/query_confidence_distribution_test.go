package server

import (
	"context"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #873: handleQuery always emitted `_meta.confidence_distribution`,
// even when the user's RETURN didn't project `extraction_confidence`.
// In that case `confs` stayed empty and `confidenceDistribution([])`
// returned `{"0.0-0.5":0,"0.5-0.7":0,"0.7-0.9":0,"0.9-1.0":0}` — which
// reads as "every result landed in confidence 0", the opposite of the
// truth. Other tools (search, trace) always populate the histogram
// because their result rows carry confidence — only `query` had this
// query-shape-dependent gap.
//
// Fix: omit the field entirely when there's no confidence data to
// summarize. The non-empty case still emits the histogram.

func TestHandleQuery_NoConfidenceColumn_OmitsDistribution(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-noconf"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::pkg.f#Function", ProjectID: pid, FilePath: "f.go",
			Name: "f", QualifiedName: "pkg.f", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql": `MATCH (n:Function) RETURN n.name`,
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if _, has := meta["confidence_distribution"]; has {
		t.Errorf("RETURN n.name doesn't project extraction_confidence — confidence_distribution must be omitted, not emitted as all-zero bins. meta=%v", meta)
	}
}

// Control: when the query DOES project extraction_confidence the
// histogram still surfaces.
func TestHandleQuery_WithConfidenceColumn_EmitsDistribution(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-conf"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::pkg.f#Function", ProjectID: pid, FilePath: "f.go",
			Name: "f", QualifiedName: "pkg.f", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql": `MATCH (n:Function) RETURN n.name, n.extraction_confidence`,
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	dist, has := meta["confidence_distribution"].(map[string]any)
	if !has {
		t.Fatalf("confidence_distribution should be present when query projects extraction_confidence; meta=%v", meta)
	}
	// One symbol at confidence 1.0 lands in the 0.9-1.0 bucket.
	if got, _ := dist["0.9-1.0"].(float64); got != 1 {
		t.Errorf("expected 1 result in 0.9-1.0 bin; dist=%v", dist)
	}
}
