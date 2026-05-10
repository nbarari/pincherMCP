package server

import (
	"context"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #350: when a kind filter excludes the only exact-name match, search
// returns a non-empty BM25-partial-match result with no signal that
// the agent is being misled. The fix surfaces a `_meta.exact_match_in_other_kind`
// warning and a relax-the-kind next_step.

func TestHandleSearch_ExactMatchInOtherKind_SurfacesHint(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := t.TempDir()
	store.UpsertProject(db.Project{ID: pid, Path: pid, Name: "exact-match", IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = pid

	// Seed: an actual handleIndex Method (the user wants this), plus a
	// test Function whose tokens will BM25-match "handleIndex" but whose
	// name is different.
	mustUpsertSymbols(t, store, []db.Symbol{
		{
			ID: "p::server.*Server.handleIndex#Method", ProjectID: pid,
			FilePath: "internal/server/server.go", Name: "handleIndex",
			QualifiedName: "server.*Server.handleIndex", Kind: "Method", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5,
			ExtractionConfidence: 1.0,
		},
		{
			// Use `_` separator so the unicode61 tokenizer splits this name
			// into ["test", "handleindex", "loop"] — query "handleIndex"
			// (lowercased to "handleindex") matches via BM25 even though
			// the name is NOT exactly "handleIndex".
			ID: "p::server.Test_handleIndex_loop#Function", ProjectID: pid,
			FilePath: "internal/server/server_test.go", Name: "Test_handleIndex_loop",
			QualifiedName: "server.Test_handleIndex_loop", Kind: "Function", Language: "Go",
			StartByte: 100, EndByte: 200, StartLine: 6, EndLine: 10,
			IsTest: true, ExtractionConfidence: 1.0,
		},
	})

	// Search with kind=Function. The Method (real handleIndex) is excluded;
	// the test Function with overlapping tokens will be the BM25 match.
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   "handleIndex",
		"kind":    "Function",
		"project": pid,
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing from search response")
	}

	hint, ok := meta["exact_match_in_other_kind"].(map[string]any)
	if !ok {
		t.Fatalf("expected _meta.exact_match_in_other_kind when kind filter excludes exact-name match; got meta=%v", meta)
	}
	if got, _ := hint["kind"].(string); got != "Method" {
		t.Errorf("hint.kind = %q, want Method", got)
	}
	if got, _ := hint["id"].(string); got != "p::server.*Server.handleIndex#Method" {
		t.Errorf("hint.id = %q, want the Method's ID", got)
	}

	// First next_step should be the kind-relax retry.
	steps, _ := meta["next_steps"].([]any)
	if len(steps) == 0 {
		t.Fatal("expected at least one next_step")
	}
	first, _ := steps[0].(map[string]any)
	if first["tool"] != "search" {
		t.Errorf("first next_step tool = %v, want search (the relax-kind retry)", first["tool"])
	}
}

// #350: when the exact-name match IS in the result set (no kind filter
// or kind filter that includes it), no hint should be emitted.
func TestHandleSearch_ExactMatchInResults_NoHint(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := t.TempDir()
	store.UpsertProject(db.Project{ID: pid, Path: pid, Name: "no-hint", IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = pid

	mustUpsertSymbols(t, store, []db.Symbol{
		{
			ID: "p::server.*Server.handleIndex#Method", ProjectID: pid,
			FilePath: "internal/server/server.go", Name: "handleIndex",
			QualifiedName: "server.*Server.handleIndex", Kind: "Method", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5,
			ExtractionConfidence: 1.0,
		},
	})

	// No kind filter; exact match is in results.
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   "handleIndex",
		"project": pid,
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if _, present := meta["exact_match_in_other_kind"]; present {
		t.Errorf("hint should NOT be emitted when exact match is already in results: %v", meta)
	}
}

// #350: phrase / multi-word queries are NOT exact-identifier queries —
// no hint should fire even if a kind filter is set, because BM25
// partial matching is the intended behaviour for phrase queries.
func TestHandleSearch_PhraseQuery_NoHint(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := t.TempDir()
	store.UpsertProject(db.Project{ID: pid, Path: pid, Name: "phrase", IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = pid

	mustUpsertSymbols(t, store, []db.Symbol{
		{
			ID: "p::server.handleSearch#Method", ProjectID: pid,
			FilePath: "internal/server/server.go", Name: "handleSearch",
			QualifiedName: "server.handleSearch", Kind: "Method", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5,
			ExtractionConfidence: 1.0,
		},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   "handle search",
		"kind":    "Function",
		"project": pid,
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if _, present := meta["exact_match_in_other_kind"]; present {
		t.Errorf("hint should NOT fire for phrase queries (BM25 partial match is intended): %v", meta)
	}
}
