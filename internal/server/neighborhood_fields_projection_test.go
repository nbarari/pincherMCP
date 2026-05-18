package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1442 v0.72: neighborhood lacked a `fields` projection. Every
// call shipped 11+ keys per neighbor (id, qualified_name, kind,
// signature, start/end_byte/line, is_exported, extraction_
// confidence). For "in-file outline" workflows the only needed
// fields are id/name/kind/start_line — dropping the rest is a
// 3-4× payload reduction. Mirrors search/symbol/symbols which
// already accept fields= projection.
//
// Tests cover positive (fields filter applies), negative (unknown
// fields are dropped silently), control (no fields = all fields),
// and cross-check (include_source still gates source disk read
// even when fields= includes "source").

func setupFieldsFixture(t *testing.T) (*Server, string, db.Symbol) {
	t.Helper()
	srv, store, _ := newTestServer(t)
	pid := "p1"
	if err := store.UpsertProject(db.Project{ID: pid, Path: "/tmp/p1", Name: "p1"}); err != nil {
		t.Fatal(err)
	}
	syms := []db.Symbol{
		{ID: "f.go::pkg.Seed#Function", ProjectID: pid, FilePath: "f.go", Name: "Seed",
			QualifiedName: "pkg.Seed", Kind: "Function", Language: "Go",
			Signature: "func Seed() int", StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5,
			IsExported: true, ExtractionConfidence: 1.0},
		{ID: "f.go::pkg.Sibling#Function", ProjectID: pid, FilePath: "f.go", Name: "Sibling",
			QualifiedName: "pkg.Sibling", Kind: "Function", Language: "Go",
			Signature: "func Sibling(x int) string", StartByte: 51, EndByte: 120, StartLine: 7, EndLine: 12,
			IsExported: true, ExtractionConfidence: 1.0},
	}
	if err := store.BulkUpsertSymbols(syms); err != nil {
		t.Fatal(err)
	}
	srv.sessionID = pid
	return srv, pid, syms[0]
}

func TestHandleNeighborhood_FieldsProjection_OnlyRequestedKeysReturned(t *testing.T) {
	t.Parallel()
	srv, _, seed := setupFieldsFixture(t)

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":     seed.ID,
		"fields": "id,name,kind,start_line",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success; got %s", textOf(t, res))
	}
	body := decode(t, res)
	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) == 0 {
		t.Fatalf("expected at least one neighbor; got 0")
	}
	want := map[string]bool{"id": true, "name": true, "kind": true, "start_line": true}
	for i, n := range neighbors {
		entry, _ := n.(map[string]any)
		for k := range entry {
			if !want[k] {
				t.Errorf("neighbor[%d] surfaced unrequested field %q (only id/name/kind/start_line were requested)", i, k)
			}
		}
		for k := range want {
			if _, ok := entry[k]; !ok {
				t.Errorf("neighbor[%d] missing requested field %q (entry=%v)", i, k, entry)
			}
		}
	}
}

// Control — omitting `fields` keeps the full shape (no regression
// on the default neighborhood envelope).
func TestHandleNeighborhood_FieldsOmitted_FullShape(t *testing.T) {
	t.Parallel()
	srv, _, seed := setupFieldsFixture(t)

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": seed.ID,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success; got %s", textOf(t, res))
	}
	body := decode(t, res)
	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) == 0 {
		t.Fatalf("expected at least one neighbor; got 0")
	}
	entry, _ := neighbors[0].(map[string]any)
	// Spot-check a handful of fields that the default shape carries.
	for _, k := range []string{"id", "name", "qualified_name", "kind", "signature", "start_line", "end_line", "is_exported", "extraction_confidence"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("default-shape neighbor missing %q (entry=%v)", k, entry)
		}
	}
}

// Negative — unknown field names are silently dropped (not
// rejected). Matches search's permissive behaviour so a typo'd
// field name doesn't error a call that's otherwise valid.
func TestHandleNeighborhood_FieldsProjection_UnknownFieldDropped(t *testing.T) {
	t.Parallel()
	srv, _, seed := setupFieldsFixture(t)

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":     seed.ID,
		"fields": "id,name,nonexistent_field,another_typo",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success despite unknown fields; got %s", textOf(t, res))
	}
	body := decode(t, res)
	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) == 0 {
		t.Fatalf("expected at least one neighbor; got 0")
	}
	entry, _ := neighbors[0].(map[string]any)
	if _, ok := entry["nonexistent_field"]; ok {
		t.Errorf("unknown field surfaced in response: %v", entry)
	}
	if _, ok := entry["id"]; !ok {
		t.Errorf("known field id missing: %v", entry)
	}
}

// Cross-check — fields=id,source DOES NOT trigger source disk
// read unless include_source=true. Source-byte reads are
// materially more expensive than in-memory field projection;
// callers passing fields=id,source without include_source
// shouldn't trigger the disk read by accident (and source would
// stay empty regardless since the read path is gated).
func TestHandleNeighborhood_FieldsIncludesSource_RequiresIncludeSource(t *testing.T) {
	t.Parallel()
	srv, _, seed := setupFieldsFixture(t)

	// Without include_source — source should be absent.
	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":     seed.ID,
		"fields": "id,source",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, res)
	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) == 0 {
		t.Fatalf("expected at least one neighbor; got 0")
	}
	entry, _ := neighbors[0].(map[string]any)
	if src, ok := entry["source"]; ok && src != nil && src != "" {
		t.Errorf("fields=source without include_source=true should NOT trigger disk read; got source=%v", src)
	}
}

// Cross-check — whitespace-tolerant parsing (search's fields
// arg ignores spaces around commas; mirror that here).
func TestHandleNeighborhood_FieldsProjection_TolerantOfWhitespace(t *testing.T) {
	t.Parallel()
	srv, _, seed := setupFieldsFixture(t)

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":     seed.ID,
		"fields": "id , name ,  kind",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, res)
	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) == 0 {
		t.Fatalf("expected at least one neighbor; got 0")
	}
	entry, _ := neighbors[0].(map[string]any)
	for _, k := range []string{"id", "name", "kind"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("whitespace-tolerant field %q missing: %v", k, entry)
		}
	}
	// signature shouldn't be present since user didn't ask for it.
	if _, ok := entry["signature"]; ok {
		t.Errorf("signature unexpectedly present in projected entry: %v", entry)
	}
}

// Suppress unused-import lint when this is the only test in the file.
var _ = strings.TrimSpace
