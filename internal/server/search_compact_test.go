package server

import (
	"context"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1226 (thin-client umbrella PR 2): search compact=true drops per-hit
// extraction_confidence + language + snippet plus the top-level
// confidence_distribution. Skips the snippet disk read entirely on
// compact, which is the real perf win on bulk searches.

func seedSearchCorpus(t *testing.T, store *db.Store, projID string) {
	t.Helper()
	mustUpsertProject(t, store, projID, "/tmp/"+projID, projID)
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "a.go::pkg.Alpha#Function", ProjectID: projID,
			FilePath: "a.go", Name: "Alpha", QualifiedName: "pkg.Alpha",
			Kind: "Function", Language: "Go",
			Signature: "func Alpha() error", ExtractionConfidence: 1.0},
		{ID: "b.go::pkg.AlphaTwo#Function", ProjectID: projID,
			FilePath: "b.go", Name: "AlphaTwo", QualifiedName: "pkg.AlphaTwo",
			Kind: "Function", Language: "Go",
			Signature: "func AlphaTwo() error", ExtractionConfidence: 1.0},
	})
}

// Positive: compact=true response carries no per-hit
// extraction_confidence / language / snippet, no top-level
// confidence_distribution. The required fields stay.
func TestHandleSearch_Compact_DropsConfidenceLanguageSnippetDistribution(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	seedSearchCorpus(t, store, "p-sc")
	srv.sessionID = "p-sc"

	res, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   "Alpha*",
		"compact": true,
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if _, present := meta["confidence_distribution"]; present {
		t.Errorf("compact=true must drop top-level confidence_distribution from _meta; got %v", meta["confidence_distribution"])
	}
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("expected at least one search hit; got 0")
	}
	for i, r := range results {
		m, _ := r.(map[string]any)
		for _, k := range []string{"id", "name", "kind", "file_path", "signature", "score"} {
			if _, ok := m[k]; !ok {
				t.Errorf("hit %d compact response missing required field %q; got keys %v", i, k, mapKeysSearch(m))
			}
		}
		for _, k := range []string{"extraction_confidence", "language", "snippet"} {
			if _, present := m[k]; present {
				t.Errorf("hit %d compact response must NOT carry %q; got %v", i, k, m[k])
			}
		}
	}
}

// Negative: default (compact omitted) preserves the full shape — no
// regression for dashboard / dogfood / quality-aware consumers that
// rely on extraction_confidence or snippet.
func TestHandleSearch_Default_PreservesFullShape(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	seedSearchCorpus(t, store, "p-scf")
	srv.sessionID = "p-scf"

	res, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": "Alpha*",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if _, ok := meta["confidence_distribution"]; !ok {
		t.Errorf("default response must include confidence_distribution; got meta keys %v", mapKeysSearch(meta))
	}
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("expected at least one search hit; got 0")
	}
	m, _ := results[0].(map[string]any)
	for _, k := range []string{"extraction_confidence", "language", "snippet"} {
		if _, ok := m[k]; !ok {
			t.Errorf("default hit missing %q; got keys %v", k, mapKeysSearch(m))
		}
	}
}

// Cross-check: the per-hit snippet field is OMITTED from the response
// map on compact, not merely set to empty string. Pre-fix (or a buggy
// shipped fix) might keep "snippet": "" — wasting bytes. Verify the
// key is absent.
func TestHandleSearch_Compact_SnippetKeyAbsentNotEmpty(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	seedSearchCorpus(t, store, "p-snip")
	srv.sessionID = "p-snip"

	res, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   "Alpha*",
		"compact": true,
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, res)
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("expected at least one hit")
	}
	for i, r := range results {
		m, _ := r.(map[string]any)
		if v, present := m["snippet"]; present {
			t.Errorf("hit %d: snippet key must be absent on compact (saves bytes); got %q present with value %q", i, "snippet", v)
		}
	}
}

// Control: compact=true still respects the existing fields= projection
// where applicable. A caller asking for explicit fields including
// `extraction_confidence` while ALSO passing compact=true is a
// contradiction — verify compact wins (compact dropped extraction_
// confidence from allFields, so projection can't bring it back from
// nothing). This pins the precedence so a future refactor that
// stuffs extraction_confidence back into allFields-pre-projection
// breaks loudly.
func TestHandleSearch_Compact_TakesPrecedenceOverFieldsProjection(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	seedSearchCorpus(t, store, "p-prec")
	srv.sessionID = "p-prec"

	res, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   "Alpha*",
		"compact": true,
		"fields":  "id,name,extraction_confidence,snippet",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, res)
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("expected at least one hit")
	}
	for i, r := range results {
		m, _ := r.(map[string]any)
		// extraction_confidence was projected-in by fields= but
		// compact suppressed its emit-into-allFields — the projected
		// value should be nil (not present, or present as nil).
		if v, present := m["extraction_confidence"]; present && v != nil {
			t.Errorf("hit %d: compact must override fields= for extraction_confidence; got %v", i, v)
		}
		if v, present := m["snippet"]; present && v != nil {
			t.Errorf("hit %d: compact must override fields= for snippet; got %v", i, v)
		}
	}
}

func mapKeysSearch(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
