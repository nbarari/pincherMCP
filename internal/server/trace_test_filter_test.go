package server

import (
	"context"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// #398: trace defaults to filtering test files + testdata fixtures.
// Setup: 1 production caller + 1 test caller + 1 fixture caller all
// with inbound CALLS edges to the same target. Default call must
// surface only the production caller; include_tests=true must
// surface all three.
func TestHandleTrace_DefaultFiltersTestsAndFixtures(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()})

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p1::pkg.Target#Function", ProjectID: "p1", FilePath: "internal/svc/svc.go",
			Name: "Target", QualifiedName: "pkg.Target", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		{ID: "p1::pkg.prodCaller#Function", ProjectID: "p1", FilePath: "internal/svc/svc.go",
			Name: "prodCaller", QualifiedName: "pkg.prodCaller", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		{ID: "p1::pkg.TestCaller#Function", ProjectID: "p1", FilePath: "internal/svc/svc_test.go",
			Name: "TestCaller", QualifiedName: "pkg.TestCaller", Kind: "Function", Language: "Go",
			IsTest: true, ExtractionConfidence: 1.0},
		{ID: "p1::corpus.fixCaller#Function", ProjectID: "p1",
			FilePath: "testdata/corpus/foo/main.go",
			Name:     "fixCaller", QualifiedName: "corpus.fixCaller", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})
	if err := store.BulkUpsertEdges([]db.Edge{
		{ProjectID: "p1", FromID: "p1::pkg.prodCaller#Function",
			ToID: "p1::pkg.Target#Function", Kind: "CALLS", Confidence: 1},
		{ProjectID: "p1", FromID: "p1::pkg.TestCaller#Function",
			ToID: "p1::pkg.Target#Function", Kind: "CALLS", Confidence: 1},
		{ProjectID: "p1", FromID: "p1::corpus.fixCaller#Function",
			ToID: "p1::pkg.Target#Function", Kind: "CALLS", Confidence: 1},
	}); err != nil {
		t.Fatal(err)
	}

	// Default: only prodCaller surfaces.
	result, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":      "Target",
		"direction": "inbound",
	}))
	if err != nil {
		t.Fatal(err)
	}
	gotNames := traceHopNames(t, result)
	if !gotNames["prodCaller"] {
		t.Errorf("default trace should surface prodCaller; got %v", gotNames)
	}
	if gotNames["TestCaller"] {
		t.Errorf("default trace should filter TestCaller (test file); got %v", gotNames)
	}
	if gotNames["fixCaller"] {
		t.Errorf("default trace should filter fixCaller (testdata fixture); got %v", gotNames)
	}

	// include_tests=true: all three surface.
	result, err = srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":          "Target",
		"direction":     "inbound",
		"include_tests": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	gotNames = traceHopNames(t, result)
	for _, want := range []string{"prodCaller", "TestCaller", "fixCaller"} {
		if !gotNames[want] {
			t.Errorf("include_tests=true should surface %s; got %v", want, gotNames)
		}
	}
}

func traceHopNames(t *testing.T, result *mcp.CallToolResult) map[string]bool {
	t.Helper()
	body := decode(t, result)
	hops, _ := body["hops"].([]any)
	got := map[string]bool{}
	for _, h := range hops {
		hop, _ := h.(map[string]any)
		nodes, _ := hop["nodes"].([]any)
		for _, n := range nodes {
			entry, _ := n.(map[string]any)
			if name, ok := entry["name"].(string); ok {
				got[name] = true
			}
		}
	}
	return got
}
