package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// Tests for #247 #5 query-aware confidence default. The 0.71 baseline
// makes sense for wide keyword searches that can BM25-match doc-section
// titles, but for an exact identifier query like `registerTools` the
// floor silently drops valid results since no doc symbol can share
// the name. Helper picks 0.0 for identifier-shaped queries, 0.71
// otherwise.

func TestDefaultMinConfidenceFor_QueryShapes(t *testing.T) {
	cases := []struct {
		query string
		want  float64
		why   string
	}{
		// Identifier-shaped → 0.0 (the bug fix).
		{"registerTools", 0.0, "single camelCase identifier"},
		{"snake_case_name", 0.0, "snake_case identifier"},
		{"ALLCAPS", 0.0, "all-caps identifier"},
		{"_leadingUnderscore", 0.0, "leading underscore is valid Go ident char"},
		{"name123", 0.0, "trailing digits"},
		{"a", 0.0, "single character"},
		// Phrase / wildcard / multi-word → 0.71 (the original default).
		{`"quoted phrase"`, 0.71, "quoted phrase needs the floor"},
		{"foo*", 0.71, "wildcard query needs the floor"},
		{"foo bar", 0.71, "multi-word query"},
		{"foo OR bar", 0.71, "FTS5 boolean operator query"},
		{"foo\tbar", 0.71, "tab-separated multi-word"},
		// Edge cases.
		{"", 0.71, "empty query falls back to baseline"},
		{"foo-bar", 0.71, "hyphen is not an identifier char (FTS5 will treat as two terms)"},
		{"foo.bar", 0.71, "dot is not an identifier char"},
		{"foo:bar", 0.71, "FTS5 column filter"},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got := defaultMinConfidenceFor(tc.query)
			if got != tc.want {
				t.Errorf("defaultMinConfidenceFor(%q) = %v, want %v (%s)",
					tc.query, got, tc.want, tc.why)
			}
		})
	}
}

// Integration: the friction this fix addresses. Pre-fix, searching for
// an exact identifier name with a confidence-floor symbol matching
// returned 0 results until the caller manually passed min_confidence=0.
// Post-fix, the identifier query auto-defaults to 0.0 and surfaces the
// match.
func TestHandleSearch_IdentifierQueryDefaultsToZeroConfidence(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "qaconf"
	store.UpsertProject(db.Project{
		ID: "qaconf", Path: "/tmp/qaconf", Name: "qaconf",
		IndexedAt: time.Now(),
	})

	// Seed a symbol with confidence 0.65 — below the legacy 0.71 default.
	// Pre-fix, an identifier-shaped search would zero-result silently.
	if err := store.BulkUpsertSymbols([]db.Symbol{{
		ID: "qaconf::pkg.LowConfidenceSymbol#Function", ProjectID: "qaconf",
		FilePath: "x.go", Name: "LowConfidenceSymbol",
		QualifiedName: "pkg.LowConfidenceSymbol",
		Kind:          "Function",
		Language:      "Go",
		ExtractionConfidence: 0.65,
	}}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   "LowConfidenceSymbol",
		"project": "qaconf",
		// NO min_confidence — should auto-default to 0.0 because the query
		// is identifier-shaped.
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	count, _ := body["count"].(float64)
	if count == 0 {
		t.Errorf("identifier query zero-resulted on a 0.65-confidence symbol; the auto-default should have used 0.0:\n  %v", body)
	}
}

// Phrase queries still get the 0.71 floor — needed to filter
// doc-section noise that BM25-matches wide queries. A 0.65-confidence
// symbol whose name contains the phrase should NOT surface.
func TestHandleSearch_PhraseQueryRespectsConfidenceFloor(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "qaconf-phrase"
	store.UpsertProject(db.Project{
		ID: "qaconf-phrase", Path: "/tmp/qaconf-phrase", Name: "qaconf-phrase",
		IndexedAt: time.Now(),
	})

	// Two symbols whose names contain a multi-word phrase. The high-
	// confidence one should surface; the low-confidence one filtered.
	if err := store.BulkUpsertSymbols([]db.Symbol{
		{
			ID: "qaconf-phrase::pkg.HighConfPhrase#Function", ProjectID: "qaconf-phrase",
			FilePath: "high.go", Name: "HighConfPhrase",
			QualifiedName: "pkg.HighConfPhrase", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0,
			Signature:            "high confidence phrase",
		},
		{
			ID: "qaconf-phrase::pkg.LowConfPhrase#Function", ProjectID: "qaconf-phrase",
			FilePath: "low.go", Name: "LowConfPhrase",
			QualifiedName: "pkg.LowConfPhrase", Kind: "Function", Language: "Go",
			ExtractionConfidence: 0.65,
			Signature:            "low confidence phrase",
		},
	}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   `"confidence phrase"`,
		"project": "qaconf-phrase",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	rows, _ := body["results"].([]any)
	for _, r := range rows {
		row, _ := r.(map[string]any)
		conf, _ := row["extraction_confidence"].(float64)
		if conf < 0.71 {
			t.Errorf("phrase query surfaced a symbol below the 0.71 floor (conf=%v); the floor should still apply for non-identifier queries:\n%v", conf, row)
		}
	}
}

// Caller-provided min_confidence wins over either default. Pin so a
// future refactor doesn't accidentally let the heuristic override an
// explicit value.
func TestHandleSearch_ExplicitMinConfidenceOverridesDefault(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "qaconf-explicit"
	store.UpsertProject(db.Project{
		ID: "qaconf-explicit", Path: "/tmp/qaconf-explicit", Name: "qaconf-explicit",
		IndexedAt: time.Now(),
	})

	if err := store.BulkUpsertSymbols([]db.Symbol{{
		ID: "qaconf-explicit::pkg.MidConf#Function", ProjectID: "qaconf-explicit",
		FilePath: "x.go", Name: "MidConf",
		QualifiedName: "pkg.MidConf", Kind: "Function", Language: "Go",
		ExtractionConfidence: 0.80,
	}}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	// Explicit min_confidence=0.95 must win over the identifier-shaped
	// query's 0.0 default — the symbol scores 0.80, so it should be
	// filtered out.
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":          "MidConf",
		"project":        "qaconf-explicit",
		"min_confidence": 0.95,
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	count, _ := body["count"].(float64)
	if count != 0 {
		t.Errorf("explicit min_confidence=0.95 was overridden by the auto-default; got count=%v:\n%v",
			count, body)
	}
}

// The min_confidence schema description must reflect the dynamic default
// so MCP clients (and humans reading the schema) know about the
// query-aware behavior. Otherwise callers passing 0.0 explicitly think
// they're changing behavior when they're not.
func TestSearchToolSchema_DocumentsQueryAwareDefault(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tool, ok := srv.tools["search"]
	if !ok {
		t.Fatal("search tool not registered")
	}
	// InputSchema is `any` (carries either json.RawMessage or another
	// JSON-marshallable shape depending on construction site). Re-marshal
	// to canonical bytes so the assertion works regardless of the
	// underlying type.
	schemaBytes, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	schema := string(schemaBytes)
	if !strings.Contains(schema, "query-aware") {
		t.Errorf("search tool's min_confidence description must mention query-aware default; got:\n%s", schema)
	}
}
