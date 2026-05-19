package server

import (
	"context"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1643 v0.86: when a bare-identifier query returns zero results and the
// wildcard form would have hit, retry the wildcard form server-side and
// surface `_meta.fellthrough_to_wildcard=true`. Mirrors the existing
// AND→OR (#453) and corpus (#113) fallthrough patterns.

// Positive: bare-identifier query that returns 0 gets rescued by the
// wildcard form, and the recovery is observable via _meta.
func TestHandleSearch_ExactIdentifierWildcardFallthrough_Rescues(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "wcf-rescues"
	store.UpsertProject(db.Project{ID: "wcf-rescues", Path: "/tmp/wcf-rescues", Name: "wcf-rescues", IndexedAt: time.Now()})
	// One symbol whose name has a suffix the BM25 tokenizer's
	// stemming/split rules can hide on bare-identifier queries.
	// The wildcard form (`*`) always finds prefix-matches regardless.
	store.BulkUpsertSymbols([]db.Symbol{
		{
			ID: "wcf1", ProjectID: "wcf-rescues", FilePath: "main.go",
			Name: "RescueMe", QualifiedName: "pkg.RescueMe",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0,
		},
	})

	// Use a query that BM25 surfaces only via the wildcard form. Most
	// tokenizers will match `RescueMe` against `RescueMe` directly, so
	// to exercise the fallthrough we lean on the underlying SearchSymbols
	// behavior — when the exact identifier matches, no fallthrough needed.
	// Test the OPPOSITE case here: query for a prefix that only matches
	// via wildcard expansion (no symbol named "Rescue" exists, but "Rescue*"
	// matches "RescueMe"). The handler converts "Rescue" → "Rescue*"
	// after the bare-token attempt zero-results.
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": "Rescue", "min_confidence": 0.0,
	}))
	if err != nil || result.IsError {
		t.Fatalf("expected non-error; err=%v isErr=%v body=%v", err, result.IsError, decode(t, result))
	}
	m := decode(t, result)
	count := int(m["count"].(float64))
	if count != 1 {
		t.Fatalf("wildcard fallthrough should surface RescueMe: count=%d, want 1", count)
	}
	meta, _ := m["_meta"].(map[string]any)
	if v, ok := meta["fellthrough_to_wildcard"].(bool); !ok || !v {
		t.Errorf("_meta.fellthrough_to_wildcard=true expected; got meta=%v", meta)
	}
	if v, _ := meta["effective_query"].(string); v != "Rescue*" {
		t.Errorf("_meta.effective_query=%q, want %q", v, "Rescue*")
	}
}

// Negative: a query that already contains a wildcard is NOT eligible for
// the fallthrough — isExactIdentifierQuery rejects it. Zero results stay
// zero, no _meta tag.
func TestHandleSearch_AlreadyWildcard_NoDoubleFallthrough(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "wcf-alreadywc"
	store.UpsertProject(db.Project{ID: "wcf-alreadywc", Path: "/tmp/wcf-alreadywc", Name: "wcf-alreadywc", IndexedAt: time.Now()})
	// Empty project — every search returns zero.
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": "NonExistent*", "min_confidence": 0.0,
	}))
	if err != nil || result.IsError {
		t.Fatalf("non-error expected; err=%v isErr=%v body=%v", err, result.IsError, decode(t, result))
	}
	m := decode(t, result)
	if int(m["count"].(float64)) != 0 {
		t.Errorf("count=%v, want 0 (no symbols in project)", m["count"])
	}
	meta, _ := m["_meta"].(map[string]any)
	if _, has := meta["fellthrough_to_wildcard"]; has {
		t.Errorf("fellthrough_to_wildcard must NOT appear when query already has a wildcard; got meta=%v", meta)
	}
}

// Negative: an exact-identifier query that hits on the first try does
// NOT trigger the fallthrough — no _meta tag.
func TestHandleSearch_DirectHit_NoWildcardFallthrough(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "wcf-directhit"
	store.UpsertProject(db.Project{ID: "wcf-directhit", Path: "/tmp/wcf-directhit", Name: "wcf-directhit", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{
			ID: "dh1", ProjectID: "wcf-directhit", FilePath: "main.go",
			Name: "DirectHit", QualifiedName: "pkg.DirectHit",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0,
		},
	})
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": "DirectHit", "min_confidence": 0.0,
	}))
	if err != nil || result.IsError {
		t.Fatalf("non-error expected; err=%v isErr=%v body=%v", err, result.IsError, decode(t, result))
	}
	m := decode(t, result)
	if int(m["count"].(float64)) != 1 {
		t.Errorf("direct hit: count=%v, want 1", m["count"])
	}
	meta, _ := m["_meta"].(map[string]any)
	if _, has := meta["fellthrough_to_wildcard"]; has {
		t.Errorf("fellthrough_to_wildcard must NOT appear on direct hit; got meta=%v", meta)
	}
}

// Cross-check: kind filter is preserved through the wildcard retry. A
// query that would match a prefix symbol of the wrong kind does NOT
// trigger the fallthrough rescue path.
func TestHandleSearch_WildcardFallthrough_KindFilterPreserved(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "wcf-kindfilter"
	store.UpsertProject(db.Project{ID: "wcf-kindfilter", Path: "/tmp/wcf-kindfilter", Name: "wcf-kindfilter", IndexedAt: time.Now()})
	// One Function symbol whose name starts with "Kept". With kind=Method
	// filter, the wildcard form "Kept*" should still respect the filter
	// and return zero.
	store.BulkUpsertSymbols([]db.Symbol{
		{
			ID: "kf1", ProjectID: "wcf-kindfilter", FilePath: "main.go",
			Name: "KeptFunc", QualifiedName: "pkg.KeptFunc",
			Kind: "Function", Language: "Go", ExtractionConfidence: 1.0,
		},
	})
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": "Kept", "kind": "Method", "min_confidence": 0.0,
	}))
	if err != nil || result.IsError {
		t.Fatalf("non-error expected; err=%v isErr=%v body=%v", err, result.IsError, decode(t, result))
	}
	m := decode(t, result)
	if int(m["count"].(float64)) != 0 {
		t.Errorf("kind=Method filter must apply to wildcard retry too; count=%v, want 0", m["count"])
	}
	meta, _ := m["_meta"].(map[string]any)
	if v, ok := meta["fellthrough_to_wildcard"].(bool); ok && v {
		t.Errorf("fellthrough_to_wildcard must NOT appear when kind filter excludes the wildcard hits; got meta=%v", meta)
	}
}

// Cross-check: corpus fallthrough composes with wildcard fallthrough.
// A docs-only symbol whose name has a prefix the wildcard surfaces gets
// found via the (corpus=""→config→docs) × wildcard composition. The
// _meta records BOTH `fellthrough_to: docs` and `fellthrough_to_wildcard: true`.
func TestHandleSearch_WildcardFallthrough_ComposesWithCorpus(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "wcf-corpus"
	store.UpsertProject(db.Project{ID: "wcf-corpus", Path: "/tmp/wcf-corpus", Name: "wcf-corpus", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{
			ID: "wcc1", ProjectID: "wcf-corpus", FilePath: "README.md",
			Name: "DocsThing", QualifiedName: "Section.DocsThing",
			Kind: "Section", Language: "Markdown", ExtractionConfidence: 0.9,
		},
	})
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": "Docs", "min_confidence": 0.0,
	}))
	if err != nil || result.IsError {
		t.Fatalf("non-error expected; err=%v isErr=%v body=%v", err, result.IsError, decode(t, result))
	}
	m := decode(t, result)
	if int(m["count"].(float64)) != 1 {
		t.Errorf("expected 1 docs hit via wildcard fallthrough; got count=%v", m["count"])
	}
	meta, _ := m["_meta"].(map[string]any)
	if v, ok := meta["fellthrough_to_wildcard"].(bool); !ok || !v {
		t.Errorf("_meta.fellthrough_to_wildcard=true expected; got meta=%v", meta)
	}
	if v, _ := meta["fellthrough_to"].(string); v != "docs" {
		t.Errorf("_meta.fellthrough_to=%q, want %q (composing with corpus fallthrough)", v, "docs")
	}
}

// Direct unit: isExactIdentifierQuery truth table — quick guard that the
// helper the fallthrough relies on matches the documented contract.
func TestIsExactIdentifierQuery_Matrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		want  bool
	}{
		{"foo", true},
		{"FooBar", true},
		{"f_oo_bar123", true},
		{"", false},          // empty
		{"foo bar", false},   // space
		{"foo\tbar", false},  // tab
		{`foo"bar`, false},   // quote
		{"foo*", false},      // wildcard already
		{"os.Stat", false},   // dotted identifier — eligible for sanitizer, not raw wildcard
		{"login-flow", false},
	}
	for _, c := range cases {
		if got := isExactIdentifierQuery(c.query); got != c.want {
			t.Errorf("isExactIdentifierQuery(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}
