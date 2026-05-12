package server

import (
	"context"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #622: in production every MCP invocation populates req.Params.Name.
// On the success path (no warnings, no diagnosis, no same-tool
// pagination entry), pedagogy-shape next_steps are stripped from the
// _meta envelope. verbose=true opts back into the full envelope.
//
// These tests pin the production behavior by setting req.Params.Name
// explicitly. Other handler tests deliberately leave it empty so they
// can keep asserting next_steps shape — the strip is gated on
// non-empty tool name to preserve that ergonomic.

func TestMeta_NextStepsStrippedOnSuccessPath(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1"})
	store.BulkUpsertSymbols([]db.Symbol{{
		ID: "p1::pkg.Foo#Function", ProjectID: "p1", FilePath: "f.go",
		Name: "Foo", QualifiedName: "pkg.Foo", Kind: "Function",
		Language: "Go", Signature: "func Foo()", ExtractionConfidence: 1.0,
	}})

	req := makeReq(map[string]any{"query": "Foo", "project": "p1"})
	req.Params.Name = "search" // matches production routing
	result, err := srv.handleSearch(context.Background(), req)
	if err != nil || result.IsError {
		t.Fatalf("handleSearch: err=%v isErr=%v body=%v", err, result.IsError, decode(t, result))
	}

	m := decode(t, result)
	meta, _ := m["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	if _, present := meta["next_steps"]; present {
		t.Errorf("next_steps should be stripped on successful search; got %v", meta["next_steps"])
	}
}

func TestMeta_NextStepsKeptWhenVerboseTrue(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1"})
	store.BulkUpsertSymbols([]db.Symbol{{
		ID: "p1::pkg.Foo#Function", ProjectID: "p1", FilePath: "f.go",
		Name: "Foo", QualifiedName: "pkg.Foo", Kind: "Function",
		Language: "Go", Signature: "func Foo()", ExtractionConfidence: 1.0,
	}})

	req := makeReq(map[string]any{"query": "Foo", "project": "p1", "verbose": true})
	req.Params.Name = "search"
	result, err := srv.handleSearch(context.Background(), req)
	if err != nil || result.IsError {
		t.Fatalf("handleSearch: err=%v isErr=%v", err, result.IsError)
	}
	m := decode(t, result)
	meta, _ := m["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	if _, present := meta["next_steps"]; !present {
		t.Errorf("next_steps should be present when verbose=true; got %v", meta)
	}
}

func TestMeta_NextStepsKeptOnEmptyResult(t *testing.T) {
	// Empty result fires diagnosis + remediation steps — high-value
	// pedagogy that must survive even in non-verbose mode.
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1"})

	req := makeReq(map[string]any{"query": "definitely-no-such-symbol", "project": "p1"})
	req.Params.Name = "search"
	result, err := srv.handleSearch(context.Background(), req)
	if err != nil || result.IsError {
		t.Fatalf("handleSearch: err=%v isErr=%v", err, result.IsError)
	}
	m := decode(t, result)
	meta, _ := m["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	if _, present := meta["next_steps"]; !present {
		t.Errorf("next_steps must survive on zero-result responses (pedagogy); got %v", meta)
	}
	if _, present := meta["diagnosis"]; !present {
		t.Errorf("diagnosis missing on empty search; got %v", meta)
	}
}

func TestMeta_NextStepsKeptForPagination(t *testing.T) {
	// Pagination next_steps point at the same tool (continuation, not
	// pedagogy). Must survive on success in non-verbose mode.
	srv, store, _ := newTestServer(t)
	for i := 0; i < 100; i++ {
		store.UpsertProject(db.Project{
			ID: "pag-" + itoa(i), Path: "/tmp/pag-" + itoa(i), Name: "pag-" + itoa(i),
		})
	}
	req := makeReq(map[string]any{})
	req.Params.Name = "list"
	result, err := srv.handleList(context.Background(), req)
	if err != nil || result.IsError {
		t.Fatalf("handleList: err=%v isErr=%v", err, result.IsError)
	}
	m := decode(t, result)
	meta, _ := m["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	steps, ok := meta["next_steps"].([]any)
	if !ok || len(steps) == 0 {
		t.Fatalf("pagination next_steps must survive on success; got %v", meta)
	}
	first, _ := steps[0].(map[string]any)
	if first["tool"] != "list" {
		t.Errorf("pagination next_steps[0].tool = %v, want list", first["tool"])
	}
}

func TestMeta_VerboseUnknownArgIsAllowed(t *testing.T) {
	// `verbose` is the universal meta-arg added by #622. It must be
	// accepted by every tool without firing the unknown-arg warning
	// (#499). Verifies the special-case in unknownArgs.
	srv, _, _ := newTestServer(t)
	w := srv.unknownArgs("symbol", map[string]any{"verbose": true})
	if len(w) != 0 {
		t.Errorf("verbose=true must not be flagged as unknown on any tool; got %v", w)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
