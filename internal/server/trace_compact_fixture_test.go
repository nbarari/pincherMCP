package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1225 (thin-client umbrella PR 1): trace gains two flags —
//   - compact=true drops per-hop kind + via and the top-level risk_summary
//   - include_fixtures (default false) splits the pre-#1225 combined
//     include_tests filter so callers can keep real tests visible while
//     still dropping pinned-corpus fixture noise

// Positive: compact=true response carries no risk_summary, no per-hop
// kind, no per-hop via, no per-hop risk. The thin-client minimal shape
// is {id, name, file_path, start_line} per hop + depth at the wrapper.
func TestHandleTrace_Compact_DropsKindViaRiskRiskSummary(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-tr", "/tmp/p-tr", "trproj")
	srv.sessionID = "p-tr"
	srv.sessionRoot = "/tmp/p-tr"

	// Two-symbol graph: caller A → callee B
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "a.go::pkg.A#Function", ProjectID: "p-tr",
			FilePath: "a.go", Name: "A", QualifiedName: "pkg.A",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		{ID: "b.go::pkg.B#Function", ProjectID: "p-tr",
			FilePath: "b.go", Name: "B", QualifiedName: "pkg.B",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
	})
	mustUpsertEdges(t, store, "p-tr", []db.Edge{{
		FromID:     "a.go::pkg.A#Function",
		ToID:       "b.go::pkg.B#Function",
		Kind:       "CALLS",
		Confidence: 1.0,
	}})

	res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":      "A",
		"direction": "outbound",
		"compact":   true,
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, res)
	if _, present := body["risk_summary"]; present {
		t.Errorf("compact=true must drop top-level risk_summary; got %v", body["risk_summary"])
	}
	hops, _ := body["hops"].([]any)
	if len(hops) == 0 {
		t.Fatalf("expected at least one hop block; got 0")
	}
	depthBlock, _ := hops[0].(map[string]any)
	nodes, _ := depthBlock["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatalf("expected at least one node; got 0")
	}
	for i, n := range nodes {
		m, _ := n.(map[string]any)
		// Required fields stay:
		for _, k := range []string{"id", "name", "file_path", "start_line"} {
			if _, ok := m[k]; !ok {
				t.Errorf("node %d compact entry missing required field %q; got keys %v", i, k, mapKeys(m))
			}
		}
		// Dropped fields:
		for _, k := range []string{"kind", "via", "risk"} {
			if _, present := m[k]; present {
				t.Errorf("node %d compact entry must NOT carry %q; got %v", i, k, m[k])
			}
		}
	}
}

// Negative: default (compact omitted) preserves the full shape so
// existing dashboard / dogfood / risk-aware consumers don't regress.
func TestHandleTrace_Default_PreservesFullShape(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-tr-full", "/tmp/p-tr-full", "trfull")
	srv.sessionID = "p-tr-full"
	srv.sessionRoot = "/tmp/p-tr-full"

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "a.go::pkg.A#Function", ProjectID: "p-tr-full",
			FilePath: "a.go", Name: "A", QualifiedName: "pkg.A",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		{ID: "b.go::pkg.B#Function", ProjectID: "p-tr-full",
			FilePath: "b.go", Name: "B", QualifiedName: "pkg.B",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
	})
	mustUpsertEdges(t, store, "p-tr-full", []db.Edge{{
		FromID: "a.go::pkg.A#Function",
		ToID:   "b.go::pkg.B#Function", Kind: "CALLS", Confidence: 1.0,
	}})

	res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":      "A",
		"direction": "outbound",
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, res)
	// risk_summary present
	if _, ok := body["risk_summary"]; !ok {
		t.Errorf("default response must include risk_summary; got %v", mapKeys(body))
	}
	hops, _ := body["hops"].([]any)
	if len(hops) == 0 {
		t.Fatalf("expected at least one hop block")
	}
	nodes, _ := hops[0].(map[string]any)["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatalf("expected at least one node")
	}
	m, _ := nodes[0].(map[string]any)
	// kind + via + risk present
	for _, k := range []string{"kind", "via", "risk"} {
		if _, ok := m[k]; !ok {
			t.Errorf("default node missing %q; got keys %v", k, mapKeys(m))
		}
	}
}

// Cross-check: include_fixtures defaults to false. A fixture-path
// hop is filtered out by default even when include_tests=true.
// Pre-#1225 include_tests=true unlocked both; post-#1225 it unlocks
// tests only.
func TestHandleTrace_IncludeFixtures_DefaultFalse_FiltersFixturesEvenWhenIncludeTestsTrue(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-tr-fix", "/tmp/p-tr-fix", "trfix")
	srv.sessionID = "p-tr-fix"
	srv.sessionRoot = "/tmp/p-tr-fix"

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "a.go::pkg.A#Function", ProjectID: "p-tr-fix",
			FilePath: "a.go", Name: "A", QualifiedName: "pkg.A",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		// Fixture-path callee — under testdata/__fixtures__/ which
		// isTestFixturePath matches.
		{ID: "testdata/__fixtures__/x.go::pkg.FixtureCallee#Function",
			ProjectID: "p-tr-fix",
			FilePath:  "testdata/__fixtures__/x.go",
			Name:      "FixtureCallee", QualifiedName: "pkg.FixtureCallee",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
	})
	mustUpsertEdges(t, store, "p-tr-fix", []db.Edge{{
		FromID: "a.go::pkg.A#Function",
		ToID:   "testdata/__fixtures__/x.go::pkg.FixtureCallee#Function",
		Kind:   "CALLS", Confidence: 1.0,
	}})

	// include_tests=true but include_fixtures left at default (false).
	// Pre-#1225 this unlocked the fixture callee; post-#1225 it's
	// still filtered.
	res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":          "A",
		"direction":     "outbound",
		"include_tests": true,
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, res)
	hops, _ := body["hops"].([]any)
	for _, h := range hops {
		nodes, _ := h.(map[string]any)["nodes"].([]any)
		for _, n := range nodes {
			fp, _ := n.(map[string]any)["file_path"].(string)
			if strings.Contains(fp, "testdata/__fixtures__/") {
				t.Errorf("post-#1225, include_tests=true alone must NOT unlock fixture paths; got file_path=%q", fp)
			}
		}
	}
}

// Cross-check: include_fixtures=true unlocks the fixture callee
// (the opt-in path).
func TestHandleTrace_IncludeFixtures_True_UnlocksFixturePaths(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-tr-fix2", "/tmp/p-tr-fix2", "trfix2")
	srv.sessionID = "p-tr-fix2"
	srv.sessionRoot = "/tmp/p-tr-fix2"

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "a.go::pkg.A#Function", ProjectID: "p-tr-fix2",
			FilePath: "a.go", Name: "A", QualifiedName: "pkg.A",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		{ID: "testdata/__fixtures__/x.go::pkg.FixtureCallee#Function",
			ProjectID: "p-tr-fix2",
			FilePath:  "testdata/__fixtures__/x.go",
			Name:      "FixtureCallee", QualifiedName: "pkg.FixtureCallee",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
	})
	mustUpsertEdges(t, store, "p-tr-fix2", []db.Edge{{
		FromID: "a.go::pkg.A#Function",
		ToID:   "testdata/__fixtures__/x.go::pkg.FixtureCallee#Function",
		Kind:   "CALLS", Confidence: 1.0,
	}})

	res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":             "A",
		"direction":        "outbound",
		"include_fixtures": true,
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, res)
	hops, _ := body["hops"].([]any)
	sawFixture := false
	for _, h := range hops {
		nodes, _ := h.(map[string]any)["nodes"].([]any)
		for _, n := range nodes {
			fp, _ := n.(map[string]any)["file_path"].(string)
			if strings.Contains(fp, "testdata/__fixtures__/") {
				sawFixture = true
			}
		}
	}
	if !sawFixture {
		t.Errorf("include_fixtures=true must surface fixture-path hops; got hops=%v", hops)
	}
}

// Control: default behavior (both flags unset) unchanged from pre-#1225.
// Real test files are still filtered, fixtures are still filtered.
func TestHandleTrace_BothFlagsDefault_StillFiltersTestsAndFixtures(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "p-tr-def", "/tmp/p-tr-def", "trdef")
	srv.sessionID = "p-tr-def"
	srv.sessionRoot = "/tmp/p-tr-def"

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "a.go::pkg.A#Function", ProjectID: "p-tr-def",
			FilePath: "a.go", Name: "A", QualifiedName: "pkg.A",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		{ID: "a_test.go::pkg.TestA#Function", ProjectID: "p-tr-def",
			FilePath: "a_test.go", Name: "TestA", QualifiedName: "pkg.TestA",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		{ID: "testdata/__fixtures__/x.go::pkg.Fix#Function",
			ProjectID: "p-tr-def",
			FilePath:  "testdata/__fixtures__/x.go",
			Name:      "Fix", QualifiedName: "pkg.Fix",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
	})
	mustUpsertEdges(t, store, "p-tr-def", []db.Edge{
		{FromID: "a_test.go::pkg.TestA#Function",
			ToID: "a.go::pkg.A#Function", Kind: "CALLS", Confidence: 1.0},
		{FromID: "testdata/__fixtures__/x.go::pkg.Fix#Function",
			ToID: "a.go::pkg.A#Function", Kind: "CALLS", Confidence: 1.0},
	})

	res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":      "A",
		"direction": "inbound",
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, res)
	hops, _ := body["hops"].([]any)
	for _, h := range hops {
		nodes, _ := h.(map[string]any)["nodes"].([]any)
		for _, n := range nodes {
			fp, _ := n.(map[string]any)["file_path"].(string)
			if strings.HasSuffix(fp, "_test.go") {
				t.Errorf("default-flags response must filter test file; got %q", fp)
			}
			if strings.Contains(fp, "testdata/__fixtures__/") {
				t.Errorf("default-flags response must filter fixture path; got %q", fp)
			}
		}
	}
}

