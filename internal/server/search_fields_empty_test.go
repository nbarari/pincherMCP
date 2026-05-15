package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1030: search's fields= projection used inline parsing that kept
// empty-string entries. `fields=",,"` produced per-row {"": null},
// and unknown fields like `fields=id,bogus_field` silently emitted
// {"bogus_field": null} — both confidently-wrong shapes with no
// signal that the projection was malformed. Now: parseFieldsArg
// strips empty entries, unknown fields warn (and are dropped from
// the projection), all-bogus fields fall back to full response.

func setupSearchFieldsProject(t *testing.T) *Server {
	t.Helper()
	srv, store, _ := newTestServer(t)
	pid := "p-search-fields"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::pkg.FieldProbe#Function", ProjectID: pid, FilePath: "f.go",
			Name: "FieldProbe", QualifiedName: "pkg.FieldProbe", Kind: "Function", Language: "Go",
			Signature: "func FieldProbe()", ExtractionConfidence: 1.0},
	})
	return srv
}

func TestHandleSearch_FieldsEmptyEntries_WarnsAndReturnsFullResponse(t *testing.T) {
	t.Parallel()
	srv := setupSearchFieldsProject(t)

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":  "FieldProbe",
		"fields": ",,",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	ws := warningsFromMeta(body)
	if !warningContains(ws, "empty entries") {
		t.Errorf("expected warning about empty fields entries; got warnings=%v", ws)
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 result; got %d", len(results))
	}
	row, _ := results[0].(map[string]any)
	if _, hasEmpty := row[""]; hasEmpty {
		t.Errorf("row must not contain empty-string key (the confidently-wrong artifact); got row=%v", row)
	}
	if _, hasID := row["id"]; !hasID {
		t.Errorf("fallback to full response should include id field; got row=%v", row)
	}
}

func TestHandleSearch_FieldsUnknownName_WarnsAndDrops(t *testing.T) {
	t.Parallel()
	srv := setupSearchFieldsProject(t)

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":  "FieldProbe",
		"fields": "id,bogus_field_name",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	ws := warningsFromMeta(body)
	if !warningContains(ws, "bogus_field_name") || !warningContains(ws, "matched no keys and were dropped") {
		t.Errorf("expected warning naming bogus_field_name as dropped; got warnings=%v", ws)
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 result; got %d", len(results))
	}
	row, _ := results[0].(map[string]any)
	if _, has := row["bogus_field_name"]; has {
		t.Errorf("row must not include the dropped unknown field; got row=%v", row)
	}
	if _, has := row["id"]; !has {
		t.Errorf("row should include the valid `id` field; got row=%v", row)
	}
}

func TestHandleSearch_FieldsAllUnknown_FallsBackToFullResponse(t *testing.T) {
	t.Parallel()
	srv := setupSearchFieldsProject(t)

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":  "FieldProbe",
		"fields": "totally_made_up,also_fake",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	ws := warningsFromMeta(body)
	foundFallback := false
	for _, w := range ws {
		if s, _ := w.(string); strings.Contains(s, "returning full response") {
			foundFallback = true
			break
		}
	}
	if !foundFallback {
		t.Errorf("expected fallback warning when every requested field was unknown; got %v", ws)
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 result; got %d", len(results))
	}
	row, _ := results[0].(map[string]any)
	if _, has := row["id"]; !has {
		t.Errorf("fallback row should include id; got row=%v", row)
	}
}

func TestHandleSearch_FieldsValid_NoWarning(t *testing.T) {
	t.Parallel()
	srv := setupSearchFieldsProject(t)

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":  "FieldProbe",
		"fields": "id,signature",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	ws := warningsFromMeta(body)
	for _, w := range ws {
		s, _ := w.(string)
		if strings.Contains(s, "matched no keys") || strings.Contains(s, "empty entries") {
			t.Errorf("valid fields must not trip a fields warning; got %s", s)
		}
	}
}
