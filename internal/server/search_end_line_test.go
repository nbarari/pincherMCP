package server

import (
	"context"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #947: search dropped end_line / start_byte / end_byte from every
// result. Agents using search results to make size-aware decisions
// ("is this a 5-line helper or a 200-line monster?") were forced into
// a follow-up `symbol` call. neighborhood / symbol / symbols all
// surface the span fields; search now matches.

func TestHandleSearch_DefaultResult_IncludesSpanFields(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p947a"
	store.UpsertProject(db.Project{ID: "p947a", Path: "/tmp/p947a", Name: "p947a", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "s1", ProjectID: "p947a", FilePath: "a.go", Name: "ProcessOrder",
			QualifiedName: "pkg.ProcessOrder", Kind: "Function", Language: "Go",
			StartByte: 100, EndByte: 500, StartLine: 10, EndLine: 42,
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": "ProcessOrder",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	row, _ := results[0].(map[string]any)
	for _, key := range []string{"start_line", "end_line", "start_byte", "end_byte"} {
		v, ok := row[key]
		if !ok {
			t.Errorf("search result missing %q (silent drop)", key)
			continue
		}
		if v == nil {
			t.Errorf("search result %q is nil; want numeric value", key)
		}
	}
	// Spot-check the values round-trip from the seed.
	if got, want := toInt(row["end_line"]), 42; got != want {
		t.Errorf("end_line = %d, want %d", got, want)
	}
	if got, want := toInt(row["start_byte"]), 100; got != want {
		t.Errorf("start_byte = %d, want %d", got, want)
	}
}

// Explicit fields= projection requesting end_line must return the value,
// not nil. Pre-fix the projection found no allFields["end_line"] key and
// silently substituted nil — the silent-confidently-wrong shape.
func TestHandleSearch_ExplicitEndLineField_NotNil(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p947b"
	store.UpsertProject(db.Project{ID: "p947b", Path: "/tmp/p947b", Name: "p947b", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "s1", ProjectID: "p947b", FilePath: "a.go", Name: "ProcessOrder",
			QualifiedName: "pkg.ProcessOrder", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 200, StartLine: 1, EndLine: 17,
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":  "ProcessOrder",
		"fields": "id,start_line,end_line",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := decode(t, result)
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	row, _ := results[0].(map[string]any)
	if row["end_line"] == nil {
		t.Fatalf("explicit fields=end_line returned nil; got row=%v", row)
	}
	if got, want := toInt(row["end_line"]), 17; got != want {
		t.Errorf("end_line = %d, want %d", got, want)
	}
}

// toInt accepts either float64 (JSON unmarshal default) or int.
func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return -1
}
