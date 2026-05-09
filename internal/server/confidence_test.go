package server

import (
	"context"
	"testing"
	"time"

	"github.com/pincherMCP/pincher/internal/db"
)

// TestConfidenceDistribution_SumsToCount is the critical no-leakage gate
// from #34's negative-tests section: bucket counts MUST sum to len(input).
// Catches off-by-one bucketing and the "1.0 falls off the table" boundary.
func TestConfidenceDistribution_SumsToCount(t *testing.T) {
	cases := [][]float64{
		{},
		{0.5},
		{0.0, 0.5, 0.7, 0.9, 1.0},                       // every boundary
		{0.0, 0.499, 0.5, 0.6, 0.7, 0.85, 0.9, 0.95, 1.0}, // dense mid-range
		{1.0, 1.0, 1.0, 1.0},                              // all top-bucket
		{0.0, 0.0, 0.0},                                   // all bottom-bucket
	}
	for _, c := range cases {
		dist := confidenceDistribution(c)
		sum := 0
		for _, n := range dist {
			sum += n
		}
		if sum != len(c) {
			t.Errorf("distribution(%v) sums to %d, want %d (input len)", c, sum, len(c))
		}
	}
}

// TestConfidenceDistribution_BoundaryInclusion pins the right edges of
// each bucket so refactoring doesn't accidentally double-count or drop
// values at exact threshold values.
func TestConfidenceDistribution_BoundaryInclusion(t *testing.T) {
	cases := []struct {
		conf       float64
		wantBucket string
	}{
		{0.0, "0.0-0.5"},
		{0.4999, "0.0-0.5"},
		{0.5, "0.5-0.7"}, // 0.5 lands in 0.5-0.7 (left-inclusive)
		{0.6999, "0.5-0.7"},
		{0.7, "0.7-0.9"}, // future Phase 4 default — confirm it's its own bucket
		{0.85, "0.7-0.9"},
		{0.8999, "0.7-0.9"},
		{0.9, "0.9-1.0"},
		{1.0, "0.9-1.0"}, // top bucket is closed on the right
	}
	for _, c := range cases {
		got := bucketLabel(c.conf)
		if got != c.wantBucket {
			t.Errorf("bucketLabel(%v) = %q, want %q", c.conf, got, c.wantBucket)
		}
	}
}

// TestConfidenceDistribution_EmptyInputShape — empty input produces a
// non-nil empty-buckets map. Without this, JSON callers see `null` instead
// of the consistent `{...}` shape and downstream histograms that assume a
// dict-shape break.
func TestConfidenceDistribution_EmptyInputShape(t *testing.T) {
	dist := confidenceDistribution(nil)
	if dist == nil {
		t.Fatal("got nil; want non-nil map")
	}
	for _, label := range confidenceBucketLabels {
		if _, ok := dist[label]; !ok {
			t.Errorf("bucket %q missing from empty distribution", label)
		}
		if dist[label] != 0 {
			t.Errorf("bucket %q has count %d on empty input, want 0", label, dist[label])
		}
	}
}

// TestFloatArg_DefaultPath proves the default returns when key is missing.
// Critical for the backward-compat invariant: a search call without
// min_confidence MUST behave identically to one with min_confidence=0.0.
func TestFloatArg_DefaultPath(t *testing.T) {
	args := map[string]any{}
	got := floatArg(args, "min_confidence", 0.0)
	if got != 0.0 {
		t.Errorf("missing key: got %v, want 0.0", got)
	}
}

// TestFloatArg_TypePromotion: JSON decodes numbers as float64, but Go-side
// test code might pass int. floatArg must accept both rather than silently
// fall back to default.
func TestFloatArg_TypePromotion(t *testing.T) {
	cases := []struct {
		v    any
		want float64
	}{
		{0.7, 0.7},
		{1, 1.0},
		{int64(0), 0.0},
		{"not-a-number", 0.0}, // garbage → default
	}
	for _, c := range cases {
		got := floatArg(map[string]any{"k": c.v}, "k", 0.0)
		if got != c.want {
			t.Errorf("floatArg(%v) = %v, want %v", c.v, got, c.want)
		}
	}
}

// TestHandleSearch_MinConfidenceFilter is the threshold-filtering
// correctness gate from #34's negative-tests section. It seeds 4 symbols
// at distinct confidences, runs the same query at multiple thresholds,
// and asserts the boundary inclusion (>= not >) and the no-filter default.
//
// Critical invariants pinned:
//   - min_confidence=0.0 returns ALL matching symbols (default = no filter)
//   - min_confidence=0.7 returns symbols with confidence >= 0.7
//   - A symbol scored EXACTLY at the threshold IS included (>= boundary)
//   - min_confidence=1.0 returns only symbols with confidence == 1.0
func TestHandleSearch_MinConfidenceFilter(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "projMC"
	store.UpsertProject(db.Project{ID: "projMC", Path: "/tmp/projMC", Name: "projMC", IndexedAt: time.Now()})

	// Seed symbols with confidence values at each bucket boundary.
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "low", ProjectID: "projMC", FilePath: "a.go", Name: "MyFuncLow",
			QualifiedName: "pkg.MyFuncLow", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 3,
			ExtractionConfidence: 0.4},
		{ID: "boundary", ProjectID: "projMC", FilePath: "b.go", Name: "MyFuncMid",
			QualifiedName: "pkg.MyFuncMid", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 3,
			ExtractionConfidence: 0.7}, // EXACTLY at threshold — must be included
		{ID: "high", ProjectID: "projMC", FilePath: "c.go", Name: "MyFuncHigh",
			QualifiedName: "pkg.MyFuncHigh", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 3,
			ExtractionConfidence: 0.9},
		{ID: "perfect", ProjectID: "projMC", FilePath: "d.go", Name: "MyFuncTop",
			QualifiedName: "pkg.MyFuncTop", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 3,
			ExtractionConfidence: 1.0},
	})

	cases := []struct {
		name          string
		minConfidence any // any so we can omit it via nil to test default
		wantCount     int
	}{
		// Phase 4 + #112 calibration: default is 0.71 (was 0.7). The symbol
		// at exactly 0.7 (formerly counted) is now filtered, so the default
		// returns 2 (0.9 + 1.0), not 3. Explicit 0.7 still includes the
		// boundary because the comparison is `>=`.
		{"no parameter (default 0.71)", nil, 2},
		// Explicit 0.0 is the escape hatch — surfaces every symbol.
		{"explicit 0.0", 0.0, 4},
		{"0.7 includes the boundary", 0.7, 3},
		{"0.9 excludes 0.7", 0.9, 2},
		{"1.0 returns only perfect-score", 1.0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := map[string]any{"query": "MyFunc*"}
			if c.minConfidence != nil {
				args["min_confidence"] = c.minConfidence
			}
			result, err := srv.handleSearch(context.Background(), makeReq(args))
			if err != nil {
				t.Fatalf("handleSearch: %v", err)
			}
			if result.IsError {
				t.Fatalf("handleSearch error: %v", decode(t, result))
			}
			m := decode(t, result)
			rows, _ := m["results"].([]any)
			if len(rows) != c.wantCount {
				t.Errorf("got %d results, want %d", len(rows), c.wantCount)
			}
			// Verify every returned row meets the threshold (the EFFECTIVE
			// threshold — default is 0.71 since #112 calibration; when the
			// caller omits the param we filter at 0.71).
			minConf := 0.71
			if c.minConfidence != nil {
				minConf, _ = c.minConfidence.(float64)
			}
			for _, r := range rows {
				row, _ := r.(map[string]any)
				conf, _ := row["extraction_confidence"].(float64)
				if conf < minConf {
					t.Errorf("row %v has confidence %v, below threshold %v",
						row["name"], conf, minConf)
				}
			}
		})
	}
}

// TestHandleSearch_DefaultIs0_7 — Phase 4 (#34) flipped the default
// from 0.0 → 0.7. Asserts a low-confidence symbol (0.5) is filtered out
// by default, and that explicit min_confidence=0.0 surfaces it as the
// escape-hatch behavior.
func TestHandleSearch_DefaultIs0_7(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "projEq"
	store.UpsertProject(db.Project{ID: "projEq", Path: "/tmp/projEq", Name: "projEq", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "sLow", ProjectID: "projEq", FilePath: "a.go", Name: "FooLow",
			QualifiedName: "pkg.FooLow", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 3,
			ExtractionConfidence: 0.5},
		{ID: "sHigh", ProjectID: "projEq", FilePath: "b.go", Name: "FooHigh",
			QualifiedName: "pkg.FooHigh", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 3,
			ExtractionConfidence: 0.95},
	})

	// Default — Phase 4 filter at 0.7. Low-confidence symbol must be excluded.
	defResult, err := srv.handleSearch(context.Background(), makeReq(map[string]any{"query": "Foo*"}))
	if err != nil {
		t.Fatalf("handleSearch default: %v", err)
	}
	defM := decode(t, defResult)
	defRows, _ := defM["results"].([]any)
	if len(defRows) != 1 {
		t.Errorf("default search: got %d results, want 1 (low-confidence filtered)", len(defRows))
	}
	for _, r := range defRows {
		row, _ := r.(map[string]any)
		conf, _ := row["extraction_confidence"].(float64)
		if conf < 0.7 {
			t.Errorf("default search returned a symbol below 0.7 threshold: %v (conf=%v)",
				row["name"], conf)
		}
	}

	// Explicit 0.0 — escape hatch surfaces every symbol.
	explResult, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":          "Foo*",
		"min_confidence": 0.0,
	}))
	if err != nil {
		t.Fatalf("handleSearch explicit 0.0: %v", err)
	}
	explM := decode(t, explResult)
	explRows, _ := explM["results"].([]any)
	if len(explRows) != 2 {
		t.Errorf("explicit 0.0: got %d results, want 2 (escape hatch shows all)", len(explRows))
	}
}

// TestHandleSearch_MetaConfidenceDistribution covers the no-leakage gate:
// every search response carries `_meta.confidence_distribution` with bucket
// counts that sum to len(results). Catches the regression where someone
// strips _meta or breaks the merge logic in jsonResultWithMeta.
func TestHandleSearch_MetaConfidenceDistribution(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "projMeta"
	store.UpsertProject(db.Project{ID: "projMeta", Path: "/tmp/projMeta", Name: "projMeta", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "m1", ProjectID: "projMeta", FilePath: "a.go", Name: "MetaA",
			QualifiedName: "pkg.MetaA", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 3,
			ExtractionConfidence: 0.85},
		{ID: "m2", ProjectID: "projMeta", FilePath: "b.go", Name: "MetaB",
			QualifiedName: "pkg.MetaB", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 3,
			ExtractionConfidence: 0.95},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{"query": "Meta*"}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	m := decode(t, result)
	meta, ok := m["_meta"].(map[string]any)
	if !ok {
		t.Fatal("_meta missing or wrong type")
	}
	dist, ok := meta["confidence_distribution"].(map[string]any)
	if !ok {
		t.Fatal("_meta.confidence_distribution missing or wrong type")
	}
	rows, _ := m["results"].([]any)
	sum := 0
	for _, n := range dist {
		// JSON unmarshals counts as float64.
		f, _ := n.(float64)
		sum += int(f)
	}
	if sum != len(rows) {
		t.Errorf("distribution sum %d != result count %d", sum, len(rows))
	}
	// Standard meta fields must also be present (merge invariant).
	for _, k := range []string{"tokens_used", "tokens_saved", "latency_ms", "cost_avoided"} {
		if _, ok := meta[k]; !ok {
			t.Errorf("standard meta field %q missing", k)
		}
	}
}

// TestRowConfidence_Projection covers the Cypher-row code path.
// Pass-through behavior when extraction_confidence isn't projected is part
// of the no-silent-drop invariant: rows that don't have confidence
// information should NOT be filtered out by min_confidence (the user's
// query didn't ask for confidence; we don't fabricate it).
func TestRowConfidence_Projection(t *testing.T) {
	cases := []struct {
		name   string
		row    map[string]any
		wantC  float64
		wantOk bool
	}{
		{"with confidence", map[string]any{"name": "x", "extraction_confidence": 0.85}, 0.85, true},
		{"without confidence", map[string]any{"name": "x"}, 0, false},
		{"wrong type", map[string]any{"extraction_confidence": "0.85"}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := rowConfidence(c.row)
			if got != c.wantC || ok != c.wantOk {
				t.Errorf("rowConfidence: got (%v, %v), want (%v, %v)",
					got, ok, c.wantC, c.wantOk)
			}
		})
	}
}
