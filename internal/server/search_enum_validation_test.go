package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #953: search with a typo'd kind/language returned 0 rows + a
// diagnosis that recommended "drop the filter" — implying the value
// was valid but selective. The value was actually not a known enum
// member at all. Surface a _meta.warnings entry naming the unknown
// value and the canonical set. Same #473-family silent-quality-loss.

func TestHandleSearch_TypoKindEmitsWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p953a"
	store.UpsertProject(db.Project{ID: "p953a", Path: "/tmp/p953a", Name: "p953a", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "s1", ProjectID: "p953a", FilePath: "a.go", Name: "handleSearch",
			QualifiedName: "pkg.handleSearch", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5,
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": "handleSearch",
		"kind":  "FunctionTypoKind",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	warnings, _ := meta["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatal("expected _meta.warnings for typo'd kind; got none")
	}
	found := false
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "FunctionTypoKind") && strings.Contains(s, "not a known symbol kind") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("warning should name FunctionTypoKind as unknown kind; got %v", warnings)
	}
}

func TestHandleSearch_TypoLanguageEmitsWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p953b"
	store.UpsertProject(db.Project{ID: "p953b", Path: "/tmp/p953b", Name: "p953b", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "s1", ProjectID: "p953b", FilePath: "a.go", Name: "handleSearch",
			QualifiedName: "pkg.handleSearch", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5,
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":    "handleSearch",
		"language": "PythonTypo",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	warnings, _ := meta["warnings"].([]any)
	found := false
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "PythonTypo") && strings.Contains(s, "not a known language") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("warning should name PythonTypo as unknown language; got %v", warnings)
	}
}

// Valid kind / language must NOT trigger the warning.
func TestHandleSearch_ValidKindLanguageNoWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p953c"
	store.UpsertProject(db.Project{ID: "p953c", Path: "/tmp/p953c", Name: "p953c", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "s1", ProjectID: "p953c", FilePath: "a.go", Name: "handleSearch",
			QualifiedName: "pkg.handleSearch", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5,
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":    "handleSearch",
		"kind":     "Function",
		"language": "Go",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return
	}
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "not a known symbol kind") ||
			strings.Contains(s, "not a known language") {
			t.Errorf("valid kind/language must not emit unknown-enum warning; got %q", s)
		}
	}
}

// Case-mismatch ("function" lowercase) must NOT trigger the unknown-
// enum warning — the canonical-case probe in canonicalKindCase
// recognises it as a typo'd CASE, not an unknown enum. The existing
// #902/#910 path handles case-mismatch separately.
func TestHandleSearch_CaseMismatchKind_NoUnknownEnumWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p953d"
	store.UpsertProject(db.Project{ID: "p953d", Path: "/tmp/p953d", Name: "p953d", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "s1", ProjectID: "p953d", FilePath: "a.go", Name: "handleSearch",
			QualifiedName: "pkg.handleSearch", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5,
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": "handleSearch",
		"kind":  "function", // lowercase — known kind, wrong case
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		return
	}
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "not a known symbol kind") {
			t.Errorf("case-mismatched kind must not emit unknown-enum warning (existing case-fix path covers it); got %q", s)
		}
	}
}
