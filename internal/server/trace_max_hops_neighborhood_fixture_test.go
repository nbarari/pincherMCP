package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1228 (thin-client umbrella PR 4): two changes.
//   1. trace max_hops cap (default 50) — hub functions returning 100+
//      hops at depth=1 get capped + truncated=true surfaced
//   2. neighborhood include_fixtures (default false) — seed-in-fixture
//      returns rich-error instead of silently dumping the fixture's
//      symbol list

// Positive: trace cap fires when hop count exceeds default 50.
// _meta.truncated + total_before_cap + max_hops surface; agent can
// re-issue with a wider cap.
func TestHandleTrace_MaxHops_DefaultCapFiresOnHub(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-hub", "/tmp/p-hub", "phub")
	srv.sessionID = "p-hub"

	// Seed a hub: 1 target, 75 inbound callers.
	syms := []db.Symbol{{
		ID: "hub.go::pkg.Hub#Function", ProjectID: "p-hub",
		FilePath: "hub.go", Name: "Hub", QualifiedName: "pkg.Hub",
		Kind: "Function", Language: "Go", ExtractionConfidence: 1.0,
	}}
	edges := []db.Edge{}
	for i := 0; i < 75; i++ {
		callerID := "caller" + itoaTrace(i) + ".go::pkg.Caller" + itoaTrace(i) + "#Function"
		syms = append(syms, db.Symbol{
			ID:        callerID,
			ProjectID: "p-hub",
			FilePath:  "caller" + itoaTrace(i) + ".go",
			Name:      "Caller" + itoaTrace(i), QualifiedName: "pkg.Caller" + itoaTrace(i),
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0,
		})
		edges = append(edges, db.Edge{
			FromID: callerID, ToID: "hub.go::pkg.Hub#Function",
			Kind: "CALLS", Confidence: 1.0,
		})
	}
	mustUpsertSymbols(t, store, syms)
	mustUpsertEdges(t, store, "p-hub", edges)

	res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":      "Hub",
		"direction": "inbound",
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, res)

	// Count hops returned.
	hops, _ := body["hops"].([]any)
	totalHops := 0
	for _, h := range hops {
		nodes, _ := h.(map[string]any)["nodes"].([]any)
		totalHops += len(nodes)
	}
	if totalHops > 50 {
		t.Errorf("hop count %d exceeds default max_hops=50 cap", totalHops)
	}
	// truncated surfaced
	meta, _ := body["_meta"].(map[string]any)
	if tr, _ := meta["truncated"].(bool); !tr {
		t.Errorf("_meta.truncated must be true when cap fires; got %v (meta keys: %v)", meta["truncated"], mapKeysCap(meta))
	}
	if tb, _ := meta["total_before_cap"].(float64); int(tb) != 75 {
		t.Errorf("_meta.total_before_cap = %v; want 75", tb)
	}
	if mh, _ := meta["max_hops"].(float64); int(mh) != 50 {
		t.Errorf("_meta.max_hops = %v; want 50", mh)
	}
}

// Cross-check: caller passes explicit max_hops=200 to widen the cap.
// All 75 hops should surface, truncated should NOT be set.
func TestHandleTrace_MaxHops_ExplicitWidenLetsAllHopsThrough(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-wid", "/tmp/p-wid", "pwid")
	srv.sessionID = "p-wid"

	syms := []db.Symbol{{
		ID: "hub.go::pkg.Hub#Function", ProjectID: "p-wid",
		FilePath: "hub.go", Name: "Hub", QualifiedName: "pkg.Hub",
		Kind: "Function", Language: "Go", ExtractionConfidence: 1.0,
	}}
	edges := []db.Edge{}
	for i := 0; i < 75; i++ {
		callerID := "c" + itoaTrace(i) + ".go::pkg.C" + itoaTrace(i) + "#Function"
		syms = append(syms, db.Symbol{
			ID: callerID, ProjectID: "p-wid",
			FilePath: "c" + itoaTrace(i) + ".go",
			Name:     "C" + itoaTrace(i), QualifiedName: "pkg.C" + itoaTrace(i),
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0,
		})
		edges = append(edges, db.Edge{
			FromID: callerID, ToID: "hub.go::pkg.Hub#Function",
			Kind: "CALLS", Confidence: 1.0,
		})
	}
	mustUpsertSymbols(t, store, syms)
	mustUpsertEdges(t, store, "p-wid", edges)

	res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":      "Hub",
		"direction": "inbound",
		"max_hops":  200,
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, res)
	hops, _ := body["hops"].([]any)
	totalHops := 0
	for _, h := range hops {
		nodes, _ := h.(map[string]any)["nodes"].([]any)
		totalHops += len(nodes)
	}
	if totalHops != 75 {
		t.Errorf("max_hops=200 should surface all 75 hops; got %d", totalHops)
	}
	meta, _ := body["_meta"].(map[string]any)
	if tr, _ := meta["truncated"].(bool); tr {
		t.Errorf("_meta.truncated must NOT be true when cap not exceeded; got true")
	}
}

// Negative: low hop counts don't trigger truncation surfacing.
func TestHandleTrace_MaxHops_BelowCapDoesNotSurfaceTruncation(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-low", "/tmp/p-low", "plow")
	srv.sessionID = "p-low"

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "a.go::pkg.A#Function", ProjectID: "p-low",
			FilePath: "a.go", Name: "A", QualifiedName: "pkg.A",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		{ID: "b.go::pkg.B#Function", ProjectID: "p-low",
			FilePath: "b.go", Name: "B", QualifiedName: "pkg.B",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
	})
	mustUpsertEdges(t, store, "p-low", []db.Edge{{
		FromID: "a.go::pkg.A#Function", ToID: "b.go::pkg.B#Function",
		Kind: "CALLS", Confidence: 1.0,
	}})

	res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":      "A",
		"direction": "outbound",
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if _, present := meta["truncated"]; present {
		t.Errorf("_meta.truncated must NOT be set when hop count < cap; got %v", meta["truncated"])
	}
}

// Positive: neighborhood with a fixture-path seed returns rich-error
// pointing at include_fixtures=true.
func TestHandleNeighborhood_Fixture_StrictErrorByDefault(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-nbf", "/tmp/p-nbf", "pnbf")
	srv.sessionID = "p-nbf"

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "testdata/__fixtures__/foo.go::pkg.Seed#Function", ProjectID: "p-nbf",
		FilePath: "testdata/__fixtures__/foo.go",
		Name:     "Seed", QualifiedName: "pkg.Seed",
		Kind: "Function", Language: "Go", ExtractionConfidence: 1.0,
	}})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "testdata/__fixtures__/foo.go::pkg.Seed#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if !res.IsError {
		t.Fatalf("fixture-path seed must return rich-error by default; got IsError=false")
	}
	body := decode(t, res)
	errMsg, _ := body["error"].(string)
	if !strings.Contains(errMsg, "include_fixtures=true") {
		t.Errorf("error must mention include_fixtures=true opt-in; got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "fixture") {
		t.Errorf("error must explain WHY (fixture path); got: %s", errMsg)
	}
}

// Cross-check: include_fixtures=true unlocks the fixture-seed path.
func TestHandleNeighborhood_Fixture_OptInUnlocks(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-nbf2", "/tmp/p-nbf2", "pnbf2")
	srv.sessionID = "p-nbf2"

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "testdata/__fixtures__/foo.go::pkg.Seed#Function", ProjectID: "p-nbf2",
			FilePath: "testdata/__fixtures__/foo.go",
			Name:     "Seed", QualifiedName: "pkg.Seed",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		{ID: "testdata/__fixtures__/foo.go::pkg.Sibling#Function", ProjectID: "p-nbf2",
			FilePath: "testdata/__fixtures__/foo.go",
			Name:     "Sibling", QualifiedName: "pkg.Sibling",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
	})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":               "testdata/__fixtures__/foo.go::pkg.Seed#Function",
		"include_fixtures": true,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		t.Fatalf("include_fixtures=true must NOT error; got IsError=true")
	}
}

// Control: non-fixture seed proceeds normally regardless of the flag.
func TestHandleNeighborhood_Fixture_NonFixtureSeedUnaffected(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-nbf3", "/tmp/p-nbf3", "pnbf3")
	srv.sessionID = "p-nbf3"

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "internal/svc/svc.go::pkg.Real#Function", ProjectID: "p-nbf3",
		FilePath: "internal/svc/svc.go",
		Name:     "Real", QualifiedName: "pkg.Real",
		Kind: "Function", Language: "Go", ExtractionConfidence: 1.0,
	}})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "internal/svc/svc.go::pkg.Real#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		t.Fatalf("non-fixture seed must not trip the guard; got IsError=true")
	}
}

func itoaTrace(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func mapKeysCap(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
