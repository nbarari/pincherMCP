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
		query  string
		corpus string
		want   float64
		why    string
	}{
		// Identifier-shaped → 0.0 (the #247 fix).
		{"registerTools", "", 0.0, "single camelCase identifier"},
		{"snake_case_name", "", 0.0, "snake_case identifier"},
		{"ALLCAPS", "", 0.0, "all-caps identifier"},
		{"_leadingUnderscore", "", 0.0, "leading underscore is valid Go ident char"},
		{"name123", "", 0.0, "trailing digits"},
		{"a", "", 0.0, "single character"},
		// Phrase / wildcard / multi-word against code → 0.71 (the original default).
		{`"quoted phrase"`, "", 0.71, "quoted phrase needs the floor"},
		{"foo*", "", 0.71, "wildcard query needs the floor"},
		{"foo bar", "", 0.71, "multi-word query"},
		{"foo OR bar", "", 0.71, "FTS5 boolean operator query"},
		{"foo\tbar", "", 0.71, "tab-separated multi-word"},
		// Edge cases.
		{"", "", 0.71, "empty query falls back to baseline"},
		{"foo-bar", "", 0.71, "hyphen is not an identifier char (FTS5 will treat as two terms)"},
		{"foo.bar", "", 0.71, "dot is not an identifier char"},
		{"foo:bar", "", 0.71, "FTS5 column filter"},
		// corpus=docs always returns 0.0 — Markdown sections extract at
		// 0.7-0.81, so the 0.71 floor would silently zero-result the
		// caller's explicit docs query (#379).
		{`"quoted phrase"`, "docs", 0.0, "phrase against docs corpus skips the floor"},
		{"foo*", "docs", 0.0, "wildcard against docs corpus skips the floor"},
		{"foo bar", "docs", 0.0, "multi-word against docs corpus skips the floor"},
		{"registerTools", "docs", 0.0, "exact identifier against docs corpus stays 0"},
		{"foo.bar", "docs", 0.0, "dotted query against docs corpus skips the floor"},
		// corpus=code keeps the existing defaults.
		{"foo bar", "code", 0.71, "multi-word against explicit code corpus keeps floor"},
		{"registerTools", "code", 0.0, "exact identifier against explicit code corpus stays 0"},
		// corpus=config keeps the existing defaults (Settings extract high-confidence,
		// no need for special casing).
		{"foo bar", "config", 0.71, "multi-word against config corpus keeps floor"},
	}

	for _, tc := range cases {
		name := tc.query
		if tc.corpus != "" {
			name = tc.query + "/" + tc.corpus
		}
		t.Run(name, func(t *testing.T) {
			got := defaultMinConfidenceFor(tc.query, tc.corpus)
			if got != tc.want {
				t.Errorf("defaultMinConfidenceFor(%q, %q) = %v, want %v (%s)",
					tc.query, tc.corpus, got, tc.want, tc.why)
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

// #379: explicit corpus=docs auto-defaults min_confidence to 0.0.
// Markdown sections extract at confidence 0.7-0.81 — exactly the band
// the legacy 0.71 default was designed to filter (it filters them OUT
// of code-corpus searches as noise). When the caller asks for the docs
// corpus directly, that filter is wrong-way-around: the README's
// section headings are precisely what they want.
//
// Pre-#379 repro: searching for the README's "Self-healing connections"
// heading via corpus=docs returned 0 with diagnosis "1 match returned
// by FTS5 but every result scored below min_confidence ≥ 0.71".
// Post-fix the caller's explicit corpus=docs is the noise filter; the
// confidence floor disappears.
func TestHandleSearch_DocsCorpus_DefaultIncludesMarkdownSections(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "qaconf-docs"
	store.UpsertProject(db.Project{
		ID: "qaconf-docs", Path: "/tmp/qaconf-docs", Name: "qaconf-docs",
		IndexedAt: time.Now(),
	})

	// Markdown Section seeded at 0.7 — exactly the bottom-of-band that
	// real Markdown extracts at, and exactly the band the 0.71 floor
	// silently drops.
	if err := store.BulkUpsertSymbols([]db.Symbol{{
		ID: "qaconf-docs::self_healing_connections#Section", ProjectID: "qaconf-docs",
		FilePath: "README.md", Name: "Self-healing connections",
		QualifiedName:        "self_healing_connections",
		Kind:                 "Section",
		Language:             "Markdown",
		ExtractionConfidence: 0.7,
		Signature:            "## Self-healing connections",
	}}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	// Phrase query (multi-token) — pre-fix this would default to 0.71
	// and zero-result. Post-fix, corpus=docs flips the default to 0.0.
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   `"Self-healing"`,
		"corpus":  "docs",
		"project": "qaconf-docs",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	count, _ := body["count"].(float64)
	if count == 0 {
		t.Fatalf("docs-corpus phrase query zero-resulted on a 0.7-confidence Markdown section; corpus=docs should auto-default to 0.0:\n  %v", body)
	}
	rows, _ := body["results"].([]any)
	if len(rows) == 0 {
		t.Fatalf("results array empty despite count > 0: %v", body)
	}
	row, _ := rows[0].(map[string]any)
	if name, _ := row["name"].(string); !strings.Contains(name, "Self-healing") {
		t.Errorf("expected the Markdown section as the top result, got name=%q", name)
	}
}

// Wildcard queries against corpus=docs also benefit. Pre-fix `auth*`
// against the docs corpus would silently filter out heading symbols
// that BM25-match `authentication`, `authorization`, etc. Post-fix
// they all surface.
func TestHandleSearch_DocsCorpus_WildcardSurfacesSections(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "qaconf-docs-wild"
	store.UpsertProject(db.Project{
		ID: "qaconf-docs-wild", Path: "/tmp/qaconf-docs-wild", Name: "qaconf-docs-wild",
		IndexedAt: time.Now(),
	})
	if err := store.BulkUpsertSymbols([]db.Symbol{{
		ID: "qaconf-docs-wild::installation#Section", ProjectID: "qaconf-docs-wild",
		FilePath: "README.md", Name: "Installation",
		QualifiedName:        "installation",
		Kind:                 "Section",
		Language:             "Markdown",
		ExtractionConfidence: 0.75,
		Signature:            "## Installation",
	}}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   "Install*",
		"corpus":  "docs",
		"project": "qaconf-docs-wild",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	if count, _ := body["count"].(float64); count == 0 {
		t.Fatalf("docs-corpus wildcard zero-resulted on a 0.75-confidence Markdown section: %v", body)
	}
}

// Caller-provided min_confidence still wins over the corpus=docs
// default. A future change shouldn't let the docs auto-default
// silently override a deliberate threshold from the caller.
func TestHandleSearch_DocsCorpus_ExplicitMinConfidenceStillWins(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "qaconf-docs-explicit"
	store.UpsertProject(db.Project{
		ID: "qaconf-docs-explicit", Path: "/tmp/qaconf-docs-explicit", Name: "qaconf-docs-explicit",
		IndexedAt: time.Now(),
	})
	if err := store.BulkUpsertSymbols([]db.Symbol{{
		ID: "qaconf-docs-explicit::low#Section", ProjectID: "qaconf-docs-explicit",
		FilePath: "README.md", Name: "LowConfidenceHeading",
		QualifiedName:        "low",
		Kind:                 "Section",
		Language:             "Markdown",
		ExtractionConfidence: 0.7,
		Signature:            "## LowConfidenceHeading",
	}}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	// Explicit min_confidence=0.9 must filter out the 0.7 section even
	// though corpus=docs would otherwise default to 0.0.
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":          "LowConfidenceHeading",
		"corpus":         "docs",
		"project":        "qaconf-docs-explicit",
		"min_confidence": 0.9,
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	if count, _ := body["count"].(float64); count != 0 {
		t.Errorf("explicit min_confidence=0.9 should have filtered the 0.7 section; got count=%v: %v", count, body)
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
