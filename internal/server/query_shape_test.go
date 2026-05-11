package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #338: handleQuery must never emit "rows":null. Same JSON-shape class
// as #328 / #330 / #332 / #334 — bug was in cypher Result.Rows defaulting
// to nil and being passed through directly to the JSON map.

func TestHandleQuery_EmptyRows_RowsIsEmptyArrayNotNull(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := "query-empty"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid

	// MATCH with a filter that won't match anything seeded.
	result, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql": `MATCH (n:Function) WHERE n.name = "definitely_not_a_real_symbol_xyz" RETURN n.id`,
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, result)
	if v, present := body["rows"]; !present {
		t.Fatal("rows key missing from query response")
	} else if v == nil {
		t.Errorf("rows is null; want [] (non-nil empty array)")
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), `"rows":null`) {
		t.Errorf("query JSON contains \"rows\":null; want \"rows\":[]\nfull: %s", raw)
	}
}

// #338: when query rows include an id column, _meta.next_steps should
// suggest a `context` follow-up on the top result. Mirrors the
// next_steps pattern in search/trace/changes/architecture.
func TestHandleQuery_NonEmptyRowsWithID_NextStepsSuggestsContext(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := "query-next"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p::main.Foo#Function", ProjectID: pid, FilePath: "main.go", Name: "Foo",
			QualifiedName: "main.Foo", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 30, StartLine: 1, EndLine: 3, ExtractionConfidence: 1.0},
	})

	result, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql": `MATCH (n:Function) WHERE n.name = "Foo" RETURN n.id, n.name`,
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing from query response")
	}
	steps, _ := meta["next_steps"].([]any)
	if len(steps) == 0 {
		t.Fatalf("expected next_steps with at least one entry; got %v", meta)
	}
	first, _ := steps[0].(map[string]any)
	if first["tool"] != "context" {
		t.Errorf("first next_step tool = %v, want context", first["tool"])
	}
	args, _ := first["args"].(string)
	if !strings.Contains(args, "p::main.Foo#Function") {
		t.Errorf("next_step args should reference the top row's id; got %q", args)
	}
}

// project=* opts in to cross-project pinchQL execution. Same shape
// as search's project=*. Returns rows from every indexed project,
// not just the session/default one.
func TestHandleQuery_ProjectStarReturnsCrossProjectRows(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"

	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()})
	store.UpsertProject(db.Project{ID: "p2", Path: "/tmp/p2", Name: "p2", IndexedAt: time.Now()})
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p1::main.Foo#Function", ProjectID: "p1", FilePath: "main.go", Name: "Foo",
			QualifiedName: "main.Foo", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 30, StartLine: 1, EndLine: 3, ExtractionConfidence: 1.0},
		{ID: "p2::main.Bar#Function", ProjectID: "p2", FilePath: "main.go", Name: "Bar",
			QualifiedName: "main.Bar", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 30, StartLine: 1, EndLine: 3, ExtractionConfidence: 1.0},
	})

	// project=* should surface rows from BOTH p1 (session default)
	// and p2 (other project).
	result, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"project": "*",
		"pinchql": `MATCH (n:Function) RETURN n.name`,
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, result)
	rows, _ := body["rows"].([]any)
	got := map[string]bool{}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if name, ok := row["n.name"].(string); ok {
			got[name] = true
		}
	}
	if !got["Foo"] {
		t.Errorf("Foo (p1) missing from project=* result; got %v", got)
	}
	if !got["Bar"] {
		t.Errorf("Bar (p2) missing from project=* result; got %v", got)
	}

	// Default (no project=*) must still scope to session project — Bar
	// from p2 must NOT appear.
	defaultResult, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql": `MATCH (n:Function) RETURN n.name`,
	}))
	if err != nil {
		t.Fatalf("handleQuery (default): %v", err)
	}
	body = decode(t, defaultResult)
	rows, _ = body["rows"].([]any)
	got = map[string]bool{}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if name, ok := row["n.name"].(string); ok {
			got[name] = true
		}
	}
	if !got["Foo"] {
		t.Errorf("Foo (session p1) missing from default result; got %v", got)
	}
	if got["Bar"] {
		t.Errorf("Bar (p2) leaked into default p1-scoped result; got %v", got)
	}
}

// firstRowID unit-tests: handles `id`, `n.id`, missing, and non-string
// values.
func TestFirstRowID(t *testing.T) {
	cases := []struct {
		name string
		rows []map[string]any
		want string
	}{
		{"empty", nil, ""},
		{"direct id", []map[string]any{{"id": "alpha", "name": "x"}}, "alpha"},
		{"aliased n.id", []map[string]any{{"n.id": "beta", "n.name": "x"}}, "beta"},
		{"aliased f.id", []map[string]any{{"f.id": "gamma"}}, "gamma"},
		{"no id column", []map[string]any{{"name": "x", "kind": "Function"}}, ""},
		{"non-string id", []map[string]any{{"id": 42}}, ""},
	}
	for _, tc := range cases {
		got := firstRowID(tc.rows)
		if got != tc.want {
			t.Errorf("%s: firstRowID = %q, want %q", tc.name, got, tc.want)
		}
	}
}
