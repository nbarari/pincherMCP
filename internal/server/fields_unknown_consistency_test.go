package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #908: symbol / symbols / context must all handle an unknown field
// name identically — drop it from the response and warn. Pre-fix:
//   context  → already warned (via projectFieldsChecked) ✅
//   symbols  → silently dropped (no warning)             ❌
//   symbol   → silently included with null value         ❌ (worst)
// Three handlers, three behaviors. Now they're consistent.

func seedFieldsTestSymbol(t *testing.T) (srv *Server) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p908"
	store.UpsertProject(db.Project{ID: "p908", Path: "/tmp/p908", Name: "p908", IndexedAt: time.Now()})
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p908::pkg.Target#Function", ProjectID: "p908",
			FilePath: "internal/util.go", Name: "Target",
			QualifiedName: "pkg.Target", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})
	return srv
}

func TestHandleSymbol_UnknownField_DroppedAndWarned(t *testing.T) {
	t.Parallel()
	srv := seedFieldsTestSymbol(t)

	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id":     "p908::pkg.Target#Function",
		"fields": "id,name,nonexistent_field",
	}))
	if err != nil {
		t.Fatal(err)
	}
	body := decode(t, result)
	// Unknown field must NOT appear in the response (pre-fix it did, as null).
	if _, present := body["nonexistent_field"]; present {
		t.Errorf("nonexistent_field must not appear in symbol response; got %v", body["nonexistent_field"])
	}
	// Known fields are still present.
	if body["name"] != "Target" {
		t.Errorf("name field must still be returned; got %v", body["name"])
	}
	// Warning must be present.
	if !fieldsHasUnknownWarning(t, body, "nonexistent_field") {
		t.Errorf("symbol response must warn about the unknown field; got _meta=%v", body["_meta"])
	}
}

func TestHandleSymbols_UnknownField_DroppedAndWarned(t *testing.T) {
	t.Parallel()
	srv := seedFieldsTestSymbol(t)

	result, err := srv.handleSymbols(context.Background(), makeReq(map[string]any{
		"ids":    []any{"p908::pkg.Target#Function"},
		"fields": "id,name,nonexistent_field",
	}))
	if err != nil {
		t.Fatal(err)
	}
	body := decode(t, result)
	symbols, _ := body["symbols"].([]any)
	if len(symbols) != 1 {
		t.Fatalf("expected 1 symbol; got %d", len(symbols))
	}
	entry, _ := symbols[0].(map[string]any)
	if _, present := entry["nonexistent_field"]; present {
		t.Errorf("per-entry response must not include the unknown field; got %v", entry["nonexistent_field"])
	}
	// Batch-level warning.
	if !fieldsHasUnknownWarning(t, body, "nonexistent_field") {
		t.Errorf("symbols response must warn about the unknown field; got _meta=%v", body["_meta"])
	}
}

// Control: a valid fields= list produces no unknown-field warning.
func TestHandleSymbol_AllKnownFields_NoWarning(t *testing.T) {
	t.Parallel()
	srv := seedFieldsTestSymbol(t)

	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id":     "p908::pkg.Target#Function",
		"fields": "id,name,kind",
	}))
	if err != nil {
		t.Fatal(err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return
	}
	if w, ok := meta["warnings"].([]any); ok {
		for _, msg := range w {
			if s, _ := msg.(string); strings.Contains(s, "matched no keys") {
				t.Errorf("valid fields must not warn; got %q", s)
			}
		}
	}
}

// When EVERY requested field is unknown, return the full response
// (don't ship an empty body) plus a warning so the call stays useful.
func TestHandleSymbol_AllFieldsUnknown_ReturnsFullResponseWithWarning(t *testing.T) {
	t.Parallel()
	srv := seedFieldsTestSymbol(t)

	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id":     "p908::pkg.Target#Function",
		"fields": "bogus1,bogus2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	body := decode(t, result)
	if body["name"] != "Target" {
		t.Errorf("when every field is unknown, full response must be returned; got name=%v", body["name"])
	}
	if !fieldsHasUnknownWarning(t, body, "bogus1") {
		t.Errorf("response must warn about bogus fields; got _meta=%v", body["_meta"])
	}
}

func fieldsHasUnknownWarning(t *testing.T, body map[string]any, fragment string) bool {
	t.Helper()
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return false
	}
	switch w := meta["warnings"].(type) {
	case []string:
		for _, s := range w {
			if strings.Contains(s, fragment) && strings.Contains(s, "matched no keys") {
				return true
			}
		}
	case []any:
		for _, s := range w {
			if str, _ := s.(string); strings.Contains(str, fragment) && strings.Contains(str, "matched no keys") {
				return true
			}
		}
	}
	return false
}
