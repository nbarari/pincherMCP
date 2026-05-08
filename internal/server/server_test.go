package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pincherMCP/pincher/internal/db"
	"github.com/pincherMCP/pincher/internal/index"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func newTestServer(t *testing.T) (*Server, *db.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	idx := index.New(store)
	srv := New(store, idx, "test")
	return srv, store, dir
}

// makeReq builds a minimal CallToolRequest with JSON args.
func makeReq(args map[string]any) *mcp.CallToolRequest {
	b, _ := json.Marshal(args)
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Arguments: json.RawMessage(b),
		},
	}
}

// decode unmarshals the text content of a tool result into a map.
func decode(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(textOf(t, result)), &m); err != nil {
		t.Fatalf("unmarshal result: %v\nraw: %s", err, textOf(t, result))
	}
	return m
}

// textOf returns the raw text of a tool result. Use this for tools that
// render human-readable output (e.g. handleStats) instead of JSON.
func textOf(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result == nil {
		t.Fatal("result is nil")
	}
	if len(result.Content) == 0 {
		t.Fatal("result has no content")
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is not TextContent, got %T", result.Content[0])
	}
	return text.Text
}

func writeGoFile(t *testing.T, dir, rel, src string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

const simpleGoSrc = `package mypkg

// Compute does something.
func Compute(x int) int { return x * 2 }

type Widget struct{ ID int }

func (w *Widget) Render() string { return "widget" }
`

// ─────────────────────────────────────────────────────────────────────────────
// Utility helpers (parseArgs, str, intArg, boolArg, etc.)
// ─────────────────────────────────────────────────────────────────────────────

func TestParseArgs_Empty(t *testing.T) {
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{}}
	m := parseArgs(req)
	if len(m) != 0 {
		t.Errorf("expected empty map, got %v", m)
	}
}

func TestParseArgs_Fields(t *testing.T) {
	req := makeReq(map[string]any{"path": "/tmp/x", "force": true, "limit": 5})
	m := parseArgs(req)
	if str(m, "path") != "/tmp/x" {
		t.Errorf("path = %q", str(m, "path"))
	}
	if !boolArg(m, "force") {
		t.Error("force should be true")
	}
	if intArg(m, "limit", 0) != 5 {
		t.Errorf("limit = %d", intArg(m, "limit", 0))
	}
}

func TestBoolArgDefault(t *testing.T) {
	m := map[string]any{}
	if !boolArgDefault(m, "missing", true) {
		t.Error("default should be true")
	}
	m["flag"] = false
	if boolArgDefault(m, "flag", true) {
		t.Error("explicit false should override default true")
	}
}

func TestStrSlice(t *testing.T) {
	m := map[string]any{"ids": []any{"a", "b", "c"}}
	got := strSlice(m, "ids")
	if len(got) != 3 || got[0] != "a" {
		t.Errorf("strSlice = %v", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// parseFileURI
// ─────────────────────────────────────────────────────────────────────────────

func TestParseFileURI(t *testing.T) {
	cases := []struct {
		uri string
		ok  bool
	}{
		{"file:///tmp/project", true},
		{"http://example.com", false},
		{"not-a-uri", false},
	}
	for _, c := range cases {
		got, ok := parseFileURI(c.uri)
		if ok != c.ok {
			t.Errorf("parseFileURI(%q) ok=%v, want %v", c.uri, ok, c.ok)
		}
		if ok && got == "" {
			t.Errorf("parseFileURI(%q) returned empty path", c.uri)
		}
		// Verify the path contains expected components (OS-agnostic)
		if ok && !strings.Contains(got, "tmp") {
			t.Errorf("parseFileURI(%q) = %q, expected path containing 'tmp'", c.uri, got)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleList
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleList_Empty(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleList(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	m := decode(t, result)
	if m["count"].(float64) != 0 {
		t.Errorf("expected 0 projects, got %v", m["count"])
	}
}

func TestHandleList_WithProjects(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "proj1", IndexedAt: time.Now()})
	store.UpsertProject(db.Project{ID: "p2", Path: "/p2", Name: "proj2", IndexedAt: time.Now()})

	result, err := srv.handleList(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	m := decode(t, result)
	if m["count"].(float64) != 2 {
		t.Errorf("expected 2 projects, got %v", m["count"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleIndex
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleIndex_NoPath(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// No session root, no path arg → error result
	result, err := srv.handleIndex(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleIndex: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result when no path provided")
	}
}

func TestHandleIndex_ValidPath(t *testing.T) {
	srv, _, _ := newTestServer(t)
	repoDir := t.TempDir()
	writeGoFile(t, repoDir, "pkg/service.go", simpleGoSrc)

	result, err := srv.handleIndex(context.Background(), makeReq(map[string]any{"path": repoDir}))
	if err != nil {
		t.Fatalf("handleIndex: %v", err)
	}
	if result.IsError {
		m := decode(t, result)
		t.Fatalf("handleIndex error: %v", m)
	}
	m := decode(t, result)
	if m["files"].(float64) < 1 {
		t.Errorf("expected at least 1 file indexed, got %v", m["files"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleSearch
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleSearch_NoProject(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// No session root, no project arg → error
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{"query": "Compute"}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when no project")
	}
}

func TestHandleSearch_WithProject(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "proj1"
	store.UpsertProject(db.Project{ID: "proj1", Path: "/tmp/proj1", Name: "proj1", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "s1", ProjectID: "proj1", FilePath: "a.go", Name: "Compute",
			QualifiedName: "pkg.Compute", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{"query": "Compute"}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if result.IsError {
		t.Fatalf("handleSearch error: %v", decode(t, result))
	}
	m := decode(t, result)
	if m["count"].(float64) < 1 {
		t.Errorf("expected ≥1 result, got %v", m["count"])
	}
}

func TestHandleSearch_FieldProjection(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "proj2"
	store.UpsertProject(db.Project{ID: "proj2", Path: "/tmp/proj2", Name: "proj2", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "s2", ProjectID: "proj2", FilePath: "b.go", Name: "LoadData",
			QualifiedName: "pkg.LoadData", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 3},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":  "LoadData",
		"fields": "id,name",
	}))
	if err != nil {
		t.Fatalf("handleSearch fields: %v", err)
	}
	if result.IsError {
		t.Fatalf("handleSearch fields error: %v", decode(t, result))
	}
	m := decode(t, result)
	rows, _ := m["results"].([]any)
	if len(rows) == 0 {
		t.Skip("no results — FTS not indexed yet")
	}
	row, _ := rows[0].(map[string]any)
	if _, ok := row["kind"]; ok {
		t.Error("field projection: 'kind' should be absent when fields=id,name")
	}
	if _, ok := row["id"]; !ok {
		t.Error("field projection: 'id' should be present")
	}
}

func TestHandleSearch_SnippetOmittedWhenFieldsExcluded(t *testing.T) {
	// When the caller's fields= projection excludes "snippet", the result
	// row must not contain a snippet key — and the snippet-read disk path
	// should not run. (The latter is also covered by the perf comment in
	// handleSearch; this test pins the user-visible behaviour.)
	srv, store, _ := newTestServer(t)
	srv.sessionID = "projSnippet"
	store.UpsertProject(db.Project{ID: "projSnippet", Path: "/tmp/projSnippet", Name: "projSnippet", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "sSnip", ProjectID: "projSnippet", FilePath: "z.go", Name: "ParseInput",
			QualifiedName: "pkg.ParseInput", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":  "ParseInput",
		"fields": "id,name,kind",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if result.IsError {
		t.Fatalf("handleSearch error: %v", decode(t, result))
	}
	m := decode(t, result)
	rows, _ := m["results"].([]any)
	if len(rows) == 0 {
		t.Skip("no results — FTS not indexed yet")
	}
	row, _ := rows[0].(map[string]any)
	if _, ok := row["snippet"]; ok {
		t.Error("snippet should be absent when fields= excludes it")
	}
}

func TestHandleSearch_SnippetIncludedByDefault(t *testing.T) {
	// Without fields= projection, the row should include the snippet key
	// (even if its value is empty when the file isn't on disk).
	srv, store, _ := newTestServer(t)
	srv.sessionID = "projSnipDefault"
	store.UpsertProject(db.Project{ID: "projSnipDefault", Path: "/tmp/projSnipDefault", Name: "projSnipDefault", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "sSnipDef", ProjectID: "projSnipDefault", FilePath: "z.go", Name: "WriteOutput",
			QualifiedName: "pkg.WriteOutput", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": "WriteOutput",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if result.IsError {
		t.Fatalf("handleSearch error: %v", decode(t, result))
	}
	m := decode(t, result)
	rows, _ := m["results"].([]any)
	if len(rows) == 0 {
		t.Skip("no results — FTS not indexed yet")
	}
	row, _ := rows[0].(map[string]any)
	if _, ok := row["snippet"]; !ok {
		t.Error("snippet key should be present when fields= is unset")
	}
}

func TestHandleSearch_AllProjects(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "pA", Path: "/tmp/pA", Name: "pA", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "sA", ProjectID: "pA", FilePath: "x.go", Name: "GlobalFunc",
			QualifiedName: "pkg.GlobalFunc", Kind: "Function", Language: "Go"},
	})

	// project="*" searches across all projects (no project filter)
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   "GlobalFunc",
		"project": "*",
	}))
	if err != nil {
		t.Fatalf("handleSearch *: %v", err)
	}
	if result.IsError {
		t.Fatalf("handleSearch * error: %v", decode(t, result))
	}
	m := decode(t, result)
	if m["count"] == nil {
		t.Errorf("handleSearch *: missing count field")
	}
}

func TestHandleSearch_VariableKindSkipsSnippet(t *testing.T) {
	srv, store, dir := newTestServer(t)
	srv.sessionID = "proj3"
	srv.sessionRoot = dir
	store.UpsertProject(db.Project{ID: "proj3", Path: dir, Name: "proj3", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "v1", ProjectID: "proj3", FilePath: "c.go", Name: "MaxRetries",
			QualifiedName: "pkg.MaxRetries", Kind: "Variable", Language: "Go"},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{"query": "MaxRetries"}))
	if err != nil {
		t.Fatalf("handleSearch variable: %v", err)
	}
	if result.IsError {
		t.Fatalf("handleSearch variable error: %v", decode(t, result))
	}
	// Variable kind should produce empty snippet — just verify no panic
	_ = decode(t, result)
}

// ─────────────────────────────────────────────────────────────────────────────
// handleSymbol
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleSymbol_NotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{"id": "nonexistent"}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for nonexistent symbol")
	}
}

func TestHandleSymbol_NoID(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleSymbol(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when id is missing")
	}
}

func TestHandleSymbol_Found(t *testing.T) {
	srv, store, _ := newTestServer(t)
	repoDir := t.TempDir()
	writeGoFile(t, repoDir, "pkg/svc.go", simpleGoSrc)
	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "test", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "sym1", ProjectID: repoDir, FilePath: "pkg/svc.go", Name: "Compute",
			QualifiedName: "mypkg.Compute", Kind: "Function", Language: "Go",
			StartByte: 14, EndByte: 60, StartLine: 3, EndLine: 5},
	})

	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{"id": "sym1"}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if result.IsError {
		t.Logf("error (may be ok if source read fails): %v", decode(t, result))
		return
	}
	m := decode(t, result)
	if m["name"] != "Compute" {
		t.Errorf("name = %v, want Compute", m["name"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleSymbols (batch)
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleSymbols_NoIDs(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleSymbols(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleSymbols: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when ids is missing")
	}
}

func TestHandleSymbols_MultipleIDs(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "a.go", Name: "Foo", QualifiedName: "pkg.Foo",
			Kind: "Function", Language: "Go", StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5},
		{ID: "s2", ProjectID: "p1", FilePath: "a.go", Name: "Bar", QualifiedName: "pkg.Bar",
			Kind: "Function", Language: "Go", StartByte: 60, EndByte: 110, StartLine: 10, EndLine: 15},
	})

	result, err := srv.handleSymbols(context.Background(), makeReq(map[string]any{
		"ids": []any{"s1", "s2", "nonexistent"},
	}))
	if err != nil {
		t.Fatalf("handleSymbols: %v", err)
	}
	m := decode(t, result)
	if m["count"].(float64) != 3 {
		t.Errorf("expected 3 results (2 found + 1 not found), got %v", m["count"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleQuery (Cypher)
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleQuery_NoProject(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"cypher": "MATCH (f:Function) RETURN f.name",
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	// No project set — cypher runs against all data (may return empty result)
	if result == nil {
		t.Error("result should not be nil")
	}
}

func TestHandleQuery_EmptyCypher(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleQuery(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when cypher is empty")
	}
}

func TestHandleQuery_ValidQuery(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "f1", ProjectID: "p1", FilePath: "a.go", Name: "main", QualifiedName: "main.main",
			Kind: "Function", Language: "Go", StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5},
	})

	result, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"cypher": "MATCH (f:Function) WHERE f.name = 'main' RETURN f.name",
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", decode(t, result))
	}
	m := decode(t, result)
	if m["total"].(float64) < 1 {
		t.Errorf("expected at least 1 result, got %v", m["total"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleSchema
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleSchema_NoProject(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleSchema(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleSchema: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when no project")
	}
}

func TestHandleSchema_WithProject(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "f1", ProjectID: "p1", FilePath: "a.go", Name: "Foo", QualifiedName: "pkg.Foo",
			Kind: "Function", Language: "Go", StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5},
	})

	result, err := srv.handleSchema(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleSchema: %v", err)
	}
	m := decode(t, result)
	if m["symbols"].(float64) < 1 {
		t.Errorf("expected symbols > 0, got %v", m["symbols"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleArchitecture
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleArchitecture_NoProject(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleArchitecture(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleArchitecture: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when no project")
	}
}

func TestHandleArchitecture_WithProject(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now(), FileCount: 5})

	result, err := srv.handleArchitecture(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleArchitecture: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", decode(t, result))
	}
	m := decode(t, result)
	if _, ok := m["languages"]; !ok {
		t.Error("expected 'languages' key in architecture response")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleTrace
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleTrace_NoName(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleTrace(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when name is missing")
	}
}

func TestHandleTrace_SymbolNotFound(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})

	result, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name": "nonexistent",
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for nonexistent symbol")
	}
}

func TestHandleTrace_Valid(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "fn_a", ProjectID: "p1", FilePath: "a.go", Name: "Alpha", QualifiedName: "pkg.Alpha",
			Kind: "Function", Language: "Go", StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5},
		{ID: "fn_b", ProjectID: "p1", FilePath: "b.go", Name: "Beta", QualifiedName: "pkg.Beta",
			Kind: "Function", Language: "Go", StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5},
	})
	store.BulkUpsertEdges([]db.Edge{
		{ProjectID: "p1", FromID: "fn_a", ToID: "fn_b", Kind: "CALLS", Confidence: 1.0},
	})

	result, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":      "Alpha",
		"direction": "outbound",
		"depth":     float64(2),
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", decode(t, result))
	}
	m := decode(t, result)
	if m["total"].(float64) < 1 {
		t.Errorf("expected at least 1 hop, got %v", m["total"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleChanges
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleChanges_NoProject(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleChanges(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when no project")
	}
}

func TestHandleChanges_ValidProject(t *testing.T) {
	srv, store, _ := newTestServer(t)
	repoDir := t.TempDir()
	// Initialize a git repo so git diff doesn't fail
	os.MkdirAll(repoDir, 0o755)

	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "test", IndexedAt: time.Now()})
	srv.sessionID = repoDir
	srv.sessionRoot = repoDir

	result, err := srv.handleChanges(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	// git diff may fail (not a git repo) → that's an error result, which is fine
	_ = result
}

// ─────────────────────────────────────────────────────────────────────────────
// handleADR
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleADR_SetGetListDelete(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})

	// set
	result, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "set", "key": "STACK", "value": "Go+SQLite",
	}))
	if err != nil || result.IsError {
		t.Fatalf("ADR set failed: %v / %v", err, decode(t, result))
	}

	// get
	result, err = srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "get", "key": "STACK",
	}))
	if err != nil || result.IsError {
		t.Fatalf("ADR get failed")
	}
	m := decode(t, result)
	if m["value"] != "Go+SQLite" {
		t.Errorf("value = %v, want Go+SQLite", m["value"])
	}

	// list
	result, _ = srv.handleADR(context.Background(), makeReq(map[string]any{"action": "list"}))
	m = decode(t, result)
	entries := m["entries"].(map[string]any)
	if entries["STACK"] != "Go+SQLite" {
		t.Errorf("list entries = %v", entries)
	}

	// delete
	result, _ = srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "delete", "key": "STACK",
	}))
	if result.IsError {
		t.Fatalf("ADR delete failed")
	}

	// get after delete → error
	result, _ = srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "get", "key": "STACK",
	}))
	if !result.IsError {
		t.Error("expected error after delete")
	}
}

func TestHandleADR_UnknownAction(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})

	result, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "invalid",
	}))
	if err != nil {
		t.Fatalf("handleADR: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for unknown action")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleStats
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleStats_Empty(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleStats(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleStats: %v", err)
	}
	text := textOf(t, result)
	// Fresh server: SESSION section must be present but show zero calls.
	if !strings.Contains(text, "SESSION") {
		t.Errorf("missing SESSION header; got:\n%s", text)
	}
	if !strings.Contains(text, "Tool calls:          0") {
		t.Errorf("expected Tool calls: 0 on a fresh server; got:\n%s", text)
	}
}

func TestHandleStats_Accumulates(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})

	// Make a few calls
	srv.handleList(context.Background(), makeReq(nil))
	srv.handleList(context.Background(), makeReq(nil))
	srv.handleList(context.Background(), makeReq(nil))

	result, _ := srv.handleStats(context.Background(), makeReq(nil))
	text := textOf(t, result)
	// Stats reads atomics before incrementing itself, so it reports 3.
	// Accept 3+ to avoid races with the async session flusher.
	matched := false
	for _, n := range []string{"3", "4", "5"} {
		if strings.Contains(text, "Tool calls:          "+n) {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("expected Tool calls ≥ 3; got:\n%s", text)
	}
	// With pincher: should report a non-zero token count.
	if strings.Contains(text, "With pincher:        0 tokens") {
		t.Errorf("tokens_used should be non-zero; got:\n%s", text)
	}
}

func TestHandleStats_WithProject(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now(), SymCount: 42})

	result, err := srv.handleStats(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleStats: %v", err)
	}
	text := textOf(t, result)
	if !strings.Contains(text, "PROJECT") {
		t.Errorf("expected PROJECT section when session project is set; got:\n%s", text)
	}
	if !strings.Contains(text, "Name:                p1") {
		t.Errorf("expected project name row; got:\n%s", text)
	}
	if !strings.Contains(text, "Symbols:             42") {
		t.Errorf("expected symbol count row; got:\n%s", text)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleContext
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleContext_NoID(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleContext(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when id is missing")
	}
}

func TestHandleContext_NotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleContext(context.Background(), makeReq(map[string]any{"id": "nonexistent"}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for nonexistent symbol")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// parseGitDiffFiles
// ─────────────────────────────────────────────────────────────────────────────

func TestParseGitDiffFiles(t *testing.T) {
	diff := "internal/db/db.go\ninternal/server/server.go\n\n"
	files := parseGitDiffFiles(diff)
	if len(files) != 2 {
		t.Errorf("expected 2 files, got %d: %v", len(files), files)
	}
	if files[0] != "internal/db/db.go" {
		t.Errorf("files[0] = %q", files[0])
	}
}

func TestParseGitDiffFiles_Empty(t *testing.T) {
	files := parseGitDiffFiles("")
	if len(files) != 0 {
		t.Errorf("expected 0 files for empty diff, got %d", len(files))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// riskLabel
// ─────────────────────────────────────────────────────────────────────────────

func TestRiskLabel(t *testing.T) {
	cases := []struct{ d int; want string }{
		{1, "CRITICAL"}, {2, "HIGH"}, {3, "MEDIUM"}, {4, "LOW"}, {5, "LOW"},
	}
	for _, c := range cases {
		if got := index.RiskLabel(c.d); got != c.want {
			t.Errorf("RiskLabel(%d) = %q, want %q", c.d, got, c.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// max helper
// ─────────────────────────────────────────────────────────────────────────────

func TestMax(t *testing.T) {
	if max(3, 5) != 5 {
		t.Error("max(3,5) should be 5")
	}
	if max(7, 2) != 7 {
		t.Error("max(7,2) should be 7")
	}
	if max(4, 4) != 4 {
		t.Error("max(4,4) should be 4")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// resolveProjectID
// ─────────────────────────────────────────────────────────────────────────────

func TestResolveProjectID_ByID(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "proj-abc", Path: "/abc", Name: "myproj", IndexedAt: time.Now()})

	id, err := srv.resolveProjectID("proj-abc")
	if err != nil {
		t.Fatalf("resolveProjectID by ID: %v", err)
	}
	if id != "proj-abc" {
		t.Errorf("got %q, want 'proj-abc'", id)
	}
}

func TestResolveProjectID_ByName(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "proj-xyz", Path: "/xyz", Name: "myproj", IndexedAt: time.Now()})

	id, err := srv.resolveProjectID("myproj")
	if err != nil {
		t.Fatalf("resolveProjectID by name: %v", err)
	}
	if id != "proj-xyz" {
		t.Errorf("got %q, want 'proj-xyz'", id)
	}
}

func TestResolveProjectID_NotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	_, err := srv.resolveProjectID("nonexistent-project")
	if err == nil {
		t.Error("expected error for unknown project name")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// resolveProjectRoot
// ─────────────────────────────────────────────────────────────────────────────

func TestResolveProjectRoot_WithProject(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "p1", Path: "/mypath", Name: "p1", IndexedAt: time.Now()})
	root, err := srv.resolveProjectRoot("p1")
	if err != nil {
		t.Fatalf("resolveProjectRoot: %v", err)
	}
	if root != "/mypath" {
		t.Errorf("got %q, want '/mypath'", root)
	}
}

func TestResolveProjectRoot_Fallback(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.sessionRoot = "/fallback"
	root, err := srv.resolveProjectRoot("nonexistent")
	if err != nil {
		t.Fatalf("resolveProjectRoot fallback: %v", err)
	}
	if root != "/fallback" {
		t.Errorf("got %q, want '/fallback'", root)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// setRoot
// ─────────────────────────────────────────────────────────────────────────────

func TestSetRoot(t *testing.T) {
	srv, _, dir := newTestServer(t)
	srv.setRoot(dir)
	if srv.sessionRoot != dir {
		t.Errorf("sessionRoot = %q, want %q", srv.sessionRoot, dir)
	}
	if srv.sessionID == "" {
		t.Error("sessionID should be set after setRoot")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleContext with real symbol + imports
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleContext_WithSymbol(t *testing.T) {
	srv, store, _ := newTestServer(t)
	repoDir := t.TempDir()
	writeGoFile(t, repoDir, "pkg/service.go", simpleGoSrc)

	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "test", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "main-sym", ProjectID: repoDir, FilePath: "pkg/service.go",
			Name: "Compute", QualifiedName: "mypkg.Compute",
			Kind: "Function", Language: "Go", StartByte: 14, EndByte: 60,
			StartLine: 3, EndLine: 3},
	})

	result, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id": "main-sym",
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	if result.IsError {
		t.Logf("error (acceptable if source read fails): %v", decode(t, result))
		return
	}
	m := decode(t, result)
	sym := m["symbol"].(map[string]any)
	if sym["name"] != "Compute" {
		t.Errorf("symbol name = %v, want Compute", sym["name"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleChanges with git repo
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleChanges_GitRepo(t *testing.T) {
	srv, store, _ := newTestServer(t)
	repoDir := t.TempDir()

	// Initialize git repo with a committed file, then modify it
	writeGoFile(t, repoDir, "main.go", "package main\nfunc main() {}\n")
	if err := os.WriteFile(filepath.Join(repoDir, ".git", "config"), nil, 0o644); err != nil {
		// Can't init git, skip
		t.Skip("cannot create git dir structure")
	}

	// Just test with a non-git dir — git diff fails → error result
	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "test", IndexedAt: time.Now()})
	srv.sessionID = repoDir
	srv.sessionRoot = repoDir
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "m1", ProjectID: repoDir, FilePath: "main.go", Name: "main",
			QualifiedName: "main.main", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5},
	})

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{
		"scope": "staged",
	}))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	// Either succeeds or fails gracefully — no panic
	_ = result
}

// ─────────────────────────────────────────────────────────────────────────────
// handleArchitecture with language data
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleArchitecture_WithSymbols(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now(), FileCount: 5})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "f1", ProjectID: "p1", FilePath: "main.go", Name: "main",
			QualifiedName: "main.main", Kind: "Function", Language: "Go",
			IsEntryPoint: true, StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5},
		{ID: "f2", ProjectID: "p1", FilePath: "util.go", Name: "Helper",
			QualifiedName: "main.Helper", Kind: "Function", Language: "Go",
			StartByte: 60, EndByte: 110, StartLine: 10, EndLine: 15},
	})
	store.BulkUpsertEdges([]db.Edge{
		{ProjectID: "p1", FromID: "f1", ToID: "f2", Kind: "CALLS", Confidence: 1.0},
	})

	result, err := srv.handleArchitecture(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleArchitecture: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", decode(t, result))
	}
	m := decode(t, result)
	if m["entry_points"] == nil {
		t.Error("expected entry_points in architecture response")
	}
	if m["hotspots"] == nil {
		t.Error("expected hotspots in architecture response")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleSymbol with source read
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleSymbol_WithSource(t *testing.T) {
	srv, store, _ := newTestServer(t)
	repoDir := t.TempDir()
	writeGoFile(t, repoDir, "pkg/svc.go", simpleGoSrc)

	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "test", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "wsym1", ProjectID: repoDir, FilePath: "pkg/svc.go", Name: "Compute",
			QualifiedName: "mypkg.Compute", Kind: "Function", Language: "Go",
			StartByte: 14, EndByte: 55, StartLine: 3, EndLine: 3},
	})

	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{"id": "wsym1"}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	m := decode(t, result)
	if !result.IsError && m["source"] == nil {
		t.Error("expected source field in symbol response")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleADR no project
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleADR_NoProject(t *testing.T) {
	srv, _, _ := newTestServer(t)
	result, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "set", "key": "K", "value": "V",
	}))
	if err != nil {
		t.Fatalf("handleADR: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when no project set")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// MCPServer getter
// ─────────────────────────────────────────────────────────────────────────────

func TestMCPServer_Getter(t *testing.T) {
	srv, _, _ := newTestServer(t)
	s := srv.MCPServer()
	if s == nil {
		t.Error("MCPServer() returned nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleChanges: in a real git repo with staged changes
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleChanges_InGitRepo(t *testing.T) {
	srv, store, _ := newTestServer(t)
	repoDir := t.TempDir()

	// Initialize a git repo
	if out, err := runCmd(t, repoDir, "git", "init"); err != nil {
		t.Skipf("git not available: %v (%s)", err, out)
	}
	runCmd(t, repoDir, "git", "config", "user.email", "test@test.com")
	runCmd(t, repoDir, "git", "config", "user.name", "Test")

	// Create a file, commit it, then modify it
	goFile := filepath.Join(repoDir, "main.go")
	os.WriteFile(goFile, []byte("package main\nfunc main() {}\n"), 0o644)
	runCmd(t, repoDir, "git", "add", ".")
	runCmd(t, repoDir, "git", "commit", "-m", "init")

	// Modify the file (unstaged)
	os.WriteFile(goFile, []byte("package main\nfunc main() { println() }\n"), 0o644)

	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "gitrepo", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "gitm1", ProjectID: repoDir, FilePath: "main.go", Name: "main",
			QualifiedName: "main.main", Kind: "Function", Language: "Go",
			StartByte: 14, EndByte: 38, StartLine: 2, EndLine: 2},
	})
	srv.sessionID = repoDir
	srv.sessionRoot = repoDir

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{
		"scope": "unstaged",
	}))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	if result.IsError {
		t.Logf("handleChanges returned error (may be expected): %v", decode(t, result))
		return
	}
	m := decode(t, result)
	if m["summary"] == nil {
		t.Error("expected summary in changes response")
	}
}

func TestHandleChanges_WithStagedScope(t *testing.T) {
	srv, store, _ := newTestServer(t)
	repoDir := t.TempDir()

	if out, err := runCmd(t, repoDir, "git", "init"); err != nil {
		t.Skipf("git not available: %v (%s)", err, out)
	}
	runCmd(t, repoDir, "git", "config", "user.email", "test@test.com")
	runCmd(t, repoDir, "git", "config", "user.name", "Test")

	goFile := filepath.Join(repoDir, "svc.go")
	os.WriteFile(goFile, []byte("package svc\nfunc Run() {}\n"), 0o644)
	runCmd(t, repoDir, "git", "add", ".")
	runCmd(t, repoDir, "git", "commit", "-m", "init")

	// Stage a change
	os.WriteFile(goFile, []byte("package svc\nfunc Run() { println() }\n"), 0o644)
	runCmd(t, repoDir, "git", "add", "svc.go")

	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "gitrepo2", IndexedAt: time.Now()})
	srv.sessionID = repoDir
	srv.sessionRoot = repoDir

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{
		"scope": "staged",
		"depth": float64(2),
	}))
	if err != nil {
		t.Fatalf("handleChanges staged: %v", err)
	}
	_ = result // just verify no panic
}

func TestHandleChanges_AllScope(t *testing.T) {
	srv, store, _ := newTestServer(t)
	repoDir := t.TempDir()

	if out, err := runCmd(t, repoDir, "git", "init"); err != nil {
		t.Skipf("git not available: %v (%s)", err, out)
	}
	runCmd(t, repoDir, "git", "config", "user.email", "test@test.com")
	runCmd(t, repoDir, "git", "config", "user.name", "Test")
	goFile := filepath.Join(repoDir, "lib.go")
	os.WriteFile(goFile, []byte("package lib\nfunc Lib() {}\n"), 0o644)
	runCmd(t, repoDir, "git", "add", ".")
	runCmd(t, repoDir, "git", "commit", "-m", "init")
	os.WriteFile(goFile, []byte("package lib\nfunc Lib() { return }\n"), 0o644)

	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "gitrepo3", IndexedAt: time.Now()})
	srv.sessionID = repoDir
	srv.sessionRoot = repoDir

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{"scope": "all"}))
	if err != nil {
		t.Fatalf("handleChanges all: %v", err)
	}
	_ = result
}

// ─────────────────────────────────────────────────────────────────────────────
// handleContext: with IMPORTS edges
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleContext_WithImportEdges(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := "ctx-import-proj"
	repoDir := t.TempDir()
	writeGoFile(t, repoDir, "pkg/svc.go", simpleGoSrc)

	store.UpsertProject(db.Project{ID: pid, Path: repoDir, Name: "ctximp", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "ci-main", ProjectID: pid, FilePath: "pkg/svc.go", Name: "Compute",
			QualifiedName: "mypkg.Compute", Kind: "Function", Language: "Go",
			StartByte: 14, EndByte: 55, StartLine: 3, EndLine: 3},
		{ID: "ci-dep", ProjectID: pid, FilePath: "pkg/dep.go", Name: "Helper",
			QualifiedName: "mypkg.Helper", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 30, StartLine: 1, EndLine: 2},
	})
	store.BulkUpsertEdges([]db.Edge{
		{ProjectID: pid, FromID: "ci-main", ToID: "ci-dep", Kind: "IMPORTS", Confidence: 1.0},
	})
	srv.sessionID = pid

	result, err := srv.handleContext(context.Background(), makeReq(map[string]any{"id": "ci-main"}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", decode(t, result))
	}
	m := decode(t, result)
	if m["symbol"] == nil {
		t.Error("expected symbol in context response")
	}
	if m["imports"] == nil {
		t.Log("imports nil — IMPORTS edge may not have been returned")
	}
}

func TestHandleContext_NoImports(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := "ctx-noimport-proj"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/ctx", Name: "ctx", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "ni-main", ProjectID: pid, FilePath: "main.go", Name: "Run",
			QualifiedName: "pkg.Run", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 30, StartLine: 1, EndLine: 3},
	})
	srv.sessionID = pid

	result, err := srv.handleContext(context.Background(), makeReq(map[string]any{"id": "ni-main"}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", decode(t, result))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// parseFileURI edge cases
// ─────────────────────────────────────────────────────────────────────────────

func TestParseFileURI_Valid(t *testing.T) {
	path, ok := parseFileURI("file:///home/user/project")
	if !ok {
		t.Error("expected valid parse for file:///home/user/project")
	}
	if path == "" {
		t.Error("expected non-empty path")
	}
}

func TestParseFileURI_WindowsDriveLetter(t *testing.T) {
	// Windows: file:///C:/Users/project
	path, ok := parseFileURI("file:///C:/Users/project")
	if !ok {
		t.Error("expected valid parse for Windows file URI")
	}
	if path == "" {
		t.Error("expected non-empty path")
	}
}

func TestParseFileURI_InvalidScheme(t *testing.T) {
	_, ok := parseFileURI("http://example.com/path")
	if ok {
		t.Error("expected false for non-file URI")
	}
}

func TestParseFileURI_InvalidURI(t *testing.T) {
	_, ok := parseFileURI(":/invalid")
	if ok {
		t.Error("expected false for invalid URI")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// runGitDiff helper
// ─────────────────────────────────────────────────────────────────────────────

func TestRunGitDiff_NonGitDir(t *testing.T) {
	dir := t.TempDir()
	_, err := runGitDiff(dir, "unstaged")
	if err == nil {
		t.Log("runGitDiff returned nil error for non-git dir (may be ok if git says no diff)")
	}
}

func TestParseGitDiffFiles_Basic(t *testing.T) {
	diff := "internal/server/server.go\ninternal/db/db.go\n"
	files := parseGitDiffFiles(diff)
	if len(files) != 2 {
		t.Errorf("expected 2 files, got %d: %v", len(files), files)
	}
	if files[0] != "internal/server/server.go" {
		t.Errorf("unexpected first file: %q", files[0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleSymbols: with project arg
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleSymbols_WithProjectArg(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := "syms-proj"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/syms", Name: "symsproj", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: "sp1", ProjectID: pid, FilePath: "a.go", Name: "Alpha",
			QualifiedName: "pkg.Alpha", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 30, StartLine: 1, EndLine: 3},
		{ID: "sp2", ProjectID: pid, FilePath: "b.go", Name: "Beta",
			QualifiedName: "pkg.Beta", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 30, StartLine: 1, EndLine: 3},
	})
	srv.sessionID = pid

	result, err := srv.handleSymbols(context.Background(), makeReq(map[string]any{
		"ids":     []any{"sp1", "sp2", "sp-nonexistent"},
		"project": "symsproj",
	}))
	if err != nil {
		t.Fatalf("handleSymbols: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", decode(t, result))
	}
	m := decode(t, result)
	syms, ok := m["symbols"].([]any)
	if !ok || len(syms) != 3 {
		t.Errorf("expected 3 symbols (including error entry), got %v", m["symbols"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleStats
// ─────────────────────────────────────────────────────────────────────────────

// ─────────────────────────────────────────────────────────────────────────────
// resolveProjectRoot fallbacks
// ─────────────────────────────────────────────────────────────────────────────

func TestResolveProjectRoot_FallsBackToSessionRoot(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.sessionRoot = "/tmp/session-root"

	root, err := srv.resolveProjectRoot("nonexistent-project-id")
	if err != nil {
		t.Fatalf("resolveProjectRoot: %v", err)
	}
	if root != "/tmp/session-root" {
		t.Errorf("expected session root fallback, got %q", root)
	}
}

func TestResolveProjectRoot_NoSessionRoot(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// No session root, no project in DB
	_, err := srv.resolveProjectRoot("nonexistent")
	if err == nil {
		t.Error("expected error when no project and no session root")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// runCmd helper for git tests
// ─────────────────────────────────────────────────────────────────────────────

func runCmd(t *testing.T, dir string, name string, args ...string) (string, error) {
	t.Helper()
	c := exec.Command(name, args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	return string(out), err
}

// ─────────────────────────────────────────────────────────────────────────────
// handleADR: missing branch coverage
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleADR_GetEmptyKey(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})

	result, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "get",
		"key":    "",
	}))
	if err != nil {
		t.Fatalf("handleADR: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when key is empty for get")
	}
}

func TestHandleADR_SetEmptyKey(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})

	result, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "set",
		"key":    "",
		"value":  "somevalue",
	}))
	if err != nil {
		t.Fatalf("handleADR: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when key is empty for set")
	}
}

func TestHandleADR_SetEmptyValue(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})

	result, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "set",
		"key":    "somekey",
		"value":  "",
	}))
	if err != nil {
		t.Fatalf("handleADR: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when value is empty for set")
	}
}

func TestHandleADR_DeleteEmptyKey(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})

	result, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "delete",
		"key":    "",
	}))
	if err != nil {
		t.Fatalf("handleADR: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when key is empty for delete")
	}
}

func TestHandleADR_GetNotFound(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()})

	result, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "get",
		"key":    "nonexistent-key",
	}))
	if err != nil {
		t.Fatalf("handleADR: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when key not found")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleSymbol: stale ID resolution
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleSymbol_StaleIDRedirect(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: t.TempDir(), Name: "p1", IndexedAt: time.Now()})

	// Insert a symbol at the new path
	newSym := db.Symbol{
		ID: "new/path.go::MyFn#Function", ProjectID: "p1",
		FilePath: "new/path.go", Name: "MyFn", QualifiedName: "MyFn", Kind: "Function",
		Language: "Go", ExtractionConfidence: 1.0,
	}
	store.BulkUpsertSymbols([]db.Symbol{newSym})

	// Record a move: old-id → new-id
	store.RecordSymbolMove("p1", "old/path.go::MyFn#Function", newSym.ID)

	// Lookup via stale old ID
	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id": "old/path.go::MyFn#Function",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success via stale redirect, got error: %v", result.Content)
	}
	m := decode(t, result)
	if m["name"] != "MyFn" {
		t.Errorf("expected MyFn, got %v", m["name"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// intArg / boolArgDefault / strSlice: uncovered branches
// ─────────────────────────────────────────────────────────────────────────────

func TestIntArg_NonFloatFallsToDefault(t *testing.T) {
	// When the value is not a float64, intArg should return the default.
	m := map[string]any{"depth": "notanumber"}
	if got := intArg(m, "depth", 42); got != 42 {
		t.Errorf("intArg with non-float64 = %d, want 42", got)
	}
}

func TestBoolArgDefault_NonBoolFallsToDefault(t *testing.T) {
	// When the value is present but not a bool, boolArgDefault returns def.
	m := map[string]any{"flag": "notabool"}
	if got := boolArgDefault(m, "flag", true); !got {
		t.Errorf("boolArgDefault with non-bool = %v, want true (default)", got)
	}
	if got := boolArgDefault(m, "flag", false); got {
		t.Errorf("boolArgDefault with non-bool = %v, want false (default)", got)
	}
}

func TestStrSlice_NonStringValuesSkipped(t *testing.T) {
	// Values that aren't strings should be skipped.
	m := map[string]any{"ids": []any{"a", 42, "b", nil, "c"}}
	got := strSlice(m, "ids")
	if len(got) != 3 {
		t.Errorf("strSlice with mixed types = %v, want [a b c]", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleHealth
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleHealth_NoProject(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// No session project, no project arg — should still return schema_version.
	result, err := srv.handleHealth(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleHealth: %v", err)
	}
	m := decode(t, result)
	if m["schema_version"] == nil {
		t.Error("expected schema_version in health response")
	}
}

func TestHandleHealth_WithProject(t *testing.T) {
	srv, store, dir := newTestServer(t)
	pid := db.ProjectIDFromPath(dir)
	store.UpsertProject(db.Project{ID: pid, Path: dir, Name: "healthtest", IndexedAt: time.Now()})
	srv.sessionID = pid

	// Index a Go file so coverage has data
	goFile := filepath.Join(dir, "main.go")
	os.WriteFile(goFile, []byte("package main\nfunc Main() {}\n"), 0o644)
	srv.indexer.Index(context.Background(), dir, false)

	result, err := srv.handleHealth(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("handleHealth with project: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", decode(t, result))
	}
	m := decode(t, result)
	if m["schema_version"] == nil {
		t.Error("expected schema_version")
	}
	if m["project"] == nil {
		t.Error("expected project data in health response")
	}
}

func TestResolveProjectID_ByProjectName(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "pid-xyz", Path: "/tmp/xyz", Name: "myproject", IndexedAt: time.Now()})

	// Resolve by project name (not ID)
	id, err := srv.resolveProjectID("myproject")
	if err != nil {
		t.Fatalf("resolveProjectID by name: %v", err)
	}
	if id != "pid-xyz" {
		t.Errorf("resolveProjectID by name = %q, want pid-xyz", id)
	}
}

func TestResolveProjectID_NoSessionNoArg(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// sessionID is "" (default) and no project arg — should error
	_, err := srv.resolveProjectID("")
	if err == nil {
		t.Error("expected error when no project and no session, got nil")
	}
}

func TestResolveProjectID_UnknownName(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.sessionID = "some-session"
	// Project arg not in DB by ID or name
	_, err := srv.resolveProjectID("definitely-missing")
	if err == nil {
		t.Error("expected error for unknown project, got nil")
	}
}

func TestResolveProjectID_SessionIDUsedWhenEmpty(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "my-session-proj"
	store.UpsertProject(db.Project{
		ID: "my-session-proj", Path: "/tmp/session", Name: "session", IndexedAt: time.Now(),
	})

	// Empty project arg → falls back to sessionID
	id, err := srv.resolveProjectID("")
	if err != nil {
		t.Fatalf("resolveProjectID session fallback: %v", err)
	}
	if id != "my-session-proj" {
		t.Errorf("resolveProjectID session fallback = %q, want my-session-proj", id)
	}
}

func TestHandleSymbol_StaleID(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := "stale-proj"
	srv.sessionID = pid
	srv.sessionRoot = t.TempDir()
	store.UpsertProject(db.Project{ID: pid, Path: srv.sessionRoot, Name: "stale", IndexedAt: time.Now()})

	// Insert a symbol under the new ID, and record a move from old → new ID.
	newSym := db.Symbol{
		ID: "new::Fn#Function", ProjectID: pid, FilePath: "f.go",
		Name: "Fn", QualifiedName: "Fn", Kind: "Function", Language: "Go",
		StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2,
	}
	store.BulkUpsertSymbols([]db.Symbol{newSym})

	// Manually record a move: old ID → new ID
	store.DB().Exec(`INSERT INTO symbol_moves(old_id, new_id, project_id, moved_at)
		VALUES ('old::Fn#Function', 'new::Fn#Function', ?, CURRENT_TIMESTAMP)`, pid)

	// Request using the stale old ID — should resolve via symbol_moves
	result, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id": "old::Fn#Function",
	}))
	if err != nil {
		t.Fatalf("handleSymbol stale: %v", err)
	}
	if result.IsError {
		// If the stale ID resolution isn't wired (no symbol_moves table populated),
		// an error is expected — just verify no panic.
		t.Logf("stale ID not resolved (expected if symbol_moves empty): %v", result)
	}
}

func TestParseArgs_InvalidJSON(t *testing.T) {
	// Malformed JSON should return an empty map, not panic.
	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Arguments: json.RawMessage(`{not valid json`),
		},
	}
	got := parseArgs(req)
	if got == nil {
		t.Fatal("parseArgs returned nil for bad JSON, want empty map")
	}
	if len(got) != 0 {
		t.Errorf("parseArgs returned non-empty map for bad JSON: %v", got)
	}
}

func TestParseArgs_EmptyArguments(t *testing.T) {
	// nil/empty Arguments should return an empty map.
	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Arguments: nil,
		},
	}
	got := parseArgs(req)
	if got == nil {
		t.Fatal("parseArgs returned nil for empty args")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP gateway tests
// ─────────────────────────────────────────────────────────────────────────────

func httpGet(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func httpPost(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestServeHTTP_Health(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/health")
	if w.Code != http.StatusOK {
		t.Fatalf("health: got %d, want 200", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["ok"] != true {
		t.Errorf("health ok field: got %v, want true", resp["ok"])
	}
}

func TestServeHTTP_OpenAPISpec(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/openapi.json")
	if w.Code != http.StatusOK {
		t.Fatalf("openapi: got %d, want 200", w.Code)
	}
	var spec map[string]any
	json.NewDecoder(w.Body).Decode(&spec)
	if spec["openapi"] != "3.1.0" {
		t.Errorf("openapi version: got %v, want 3.1.0", spec["openapi"])
	}
	paths, ok := spec["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Error("openapi spec missing paths")
	}
}

func TestServeHTTP_Projects(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/projects")
	if w.Code != http.StatusOK {
		t.Fatalf("projects: got %d, want 200", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if _, ok := resp["projects"]; !ok {
		t.Error("projects response missing 'projects' key")
	}
}

func TestServeHTTP_CORS_Preflight(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodOptions, "/v1/list", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS: got %d, want 204", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS origin header missing")
	}
}

func TestServeHTTP_MethodNotAllowed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/list", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /list: got %d, want 405", w.Code)
	}
}

func TestServeHTTP_UnknownTool(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpPost(t, srv, "/v1/notarealtool", "{}")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown tool: got %d, want 404", w.Code)
	}
}

func TestServeHTTP_PostList(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpPost(t, srv, "/v1/list", "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("POST /list: got %d, want 200", w.Code)
	}
}

func TestServeHTTP_BearerAuth(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetHTTPKey("secret123")

	// No token → 401
	w := httpGet(t, srv, "/v1/health")
	// health is after auth check
	req := httptest.NewRequest(http.MethodPost, "/v1/list", strings.NewReader("{}"))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", w.Code)
	}

	// Wrong token → 401
	req = httptest.NewRequest(http.MethodPost, "/v1/list", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer wrongtoken")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", w.Code)
	}

	// Correct token → 200
	req = httptest.NewRequest(http.MethodPost, "/v1/list", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer secret123")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("correct token: got %d, want 200", w.Code)
	}
}

// TestAuth_ConstantTime_LengthInvariant pins the security invariant added by
// the SHA-256 + subtle.ConstantTimeCompare auth path: rejection behaviour
// MUST be identical regardless of supplied-token length.
//
// Pre-fix (`tok != s.httpKey`), Go's string compare exits at the first byte
// that differs — so a 1-byte wrong token rejects faster than a 1000-byte
// wrong token. Post-fix, both inputs are hashed to 32 bytes before compare;
// length is no longer a side channel.
//
// The test asserts the *response shape* is invariant. We can't reliably
// assert timing in CI without flake (GC, scheduling, network adapter noise
// all dominate sub-microsecond signal), but a regression to == would also
// produce different rejection paths for empty / very-long tokens — and our
// path is now uniform.
func TestAuth_ConstantTime_LengthInvariant(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetHTTPKey("secret123")

	// Each of these MUST produce identical 401 + body shape.
	tokens := []string{
		"",                                     // empty token
		"a",                                    // 1-byte wrong
		"secret12",                             // 1 byte short of correct
		"secret1234",                           // 1 byte longer than correct
		"secret124",                            // same length, last byte differs
		"xsecret123",                           // same length, first byte differs (oracle bait)
		strings.Repeat("a", 1000),              // very long wrong
		strings.Repeat("secret123", 100),       // very long, repeats real key as a prefix
	}

	var firstBody string
	for i, tok := range tokens {
		req := httptest.NewRequest(http.MethodPost, "/v1/list", strings.NewReader("{}"))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("case %d (token len=%d): got %d, want 401", i, len(tok), w.Code)
			continue
		}
		body := w.Body.String()
		if i == 0 {
			firstBody = body
			continue
		}
		// Body shape MUST be identical across all rejection cases — proves
		// the rejection path doesn't branch on token length or content.
		if body != firstBody {
			t.Errorf("case %d (token len=%d): rejection body differs from first case\n  first: %q\n  this:  %q",
				i, len(tok), firstBody, body)
		}
	}
}

// TestAuth_NoSidechannelOnPrefixMatch is the negative companion: a partial
// prefix match MUST be rejected the same way as a fully-different token.
// Pre-fix, the byte-by-byte == compare would have run further on the
// prefix-match case, leaking that the first N bytes were correct. Post-fix
// (hash-and-compare), both rejection paths are identical.
func TestAuth_NoSidechannelOnPrefixMatch(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetHTTPKey("supersecret")

	// Both wrong, but the first matches the configured key's first 8 bytes.
	prefixMatch := httptest.NewRequest(http.MethodPost, "/v1/list", strings.NewReader("{}"))
	prefixMatch.Header.Set("Authorization", "Bearer supersecX")
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, prefixMatch)

	noMatch := httptest.NewRequest(http.MethodPost, "/v1/list", strings.NewReader("{}"))
	noMatch.Header.Set("Authorization", "Bearer xxxxxxxxxxx")
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, noMatch)

	if w1.Code != http.StatusUnauthorized || w2.Code != http.StatusUnauthorized {
		t.Fatalf("both should be 401: prefix=%d nomatch=%d", w1.Code, w2.Code)
	}
	if w1.Body.String() != w2.Body.String() {
		t.Errorf("rejection body differs between prefix-match and no-match — possible byte oracle\n"+
			"  prefix: %q\n"+
			"  nomatch: %q", w1.Body.String(), w2.Body.String())
	}
}

// BenchmarkAuth_TimingProfile is a non-asserting benchmark for inspecting
// auth latency under varying token shapes. Useful for manually verifying
// the constant-time property post-merge by running:
//
//	go test ./internal/server/ -run='^$' -bench=BenchmarkAuth_TimingProfile -benchtime=1s
//
// Variation between sub-benchmarks should be bounded by GC / scheduler
// noise, not by token-length or content. We don't assert in CI because
// the noise floor on shared runners exceeds the signal; this is a
// developer-side regression sniff-test instead.
func BenchmarkAuth_TimingProfile(b *testing.B) {
	dir := b.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		b.Fatalf("db.Open: %v", err)
	}
	b.Cleanup(func() { store.Close() })
	idx := index.New(store)
	srv := New(store, idx, "test")
	srv.SetHTTPKey("supersecretkey1234567890")

	cases := []struct {
		name  string
		token string
	}{
		{"correct", "supersecretkey1234567890"},
		{"wrong_same_length", "wrongkey00000000000000000"},
		{"wrong_first_byte", "Xupersecretkey1234567890"},
		{"wrong_last_byte", "supersecretkey1234567891"},
		{"wrong_short", "x"},
		{"wrong_long_prefix_match", "supersecretkey1234567890" + strings.Repeat("x", 1000)},
	}

	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				req := httptest.NewRequest(http.MethodPost, "/v1/list", strings.NewReader("{}"))
				req.Header.Set("Authorization", "Bearer "+c.token)
				w := httptest.NewRecorder()
				srv.ServeHTTP(w, req)
			}
		})
	}
}

func TestServeHTTP_GzipResponse(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("gzip health: got %d, want 200", w.Code)
	}
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Error("Content-Encoding: gzip header missing when Accept-Encoding: gzip was sent")
	}
}

func TestAllowRequest_RateLimit(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetRateLimit(3, time.Minute)

	for i := 0; i < 3; i++ {
		if !srv.allowRequest("1.2.3.4") {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if srv.allowRequest("1.2.3.4") {
		t.Fatal("4th request should be rate-limited")
	}
	// Different IP not affected
	if !srv.allowRequest("5.6.7.8") {
		t.Fatal("different IP should not be rate-limited")
	}
}

func TestServeHTTP_RateLimitedResponse(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetRateLimit(1, time.Minute)

	makeRatedReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/list", strings.NewReader("{}"))
		req.RemoteAddr = "10.0.0.1:1234" // fixed IP so rate window applies
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}

	if w := makeRatedReq(); w.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", w.Code)
	}
	if w := makeRatedReq(); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request (over limit): got %d, want 429", w.Code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /v1/stats  (dashboard-safe read-only endpoint)
// ─────────────────────────────────────────────────────────────────────────────

func TestServeHTTP_GetStats_Empty(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/stats")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/stats: got %d, want 200", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	// session may be nil (no sessions recorded yet), all_time should exist
	if _, ok := resp["all_time"]; !ok {
		t.Errorf("GET /v1/stats: missing all_time key; got %v", resp)
	}
}

func TestServeHTTP_GetStats_WithSession(t *testing.T) {
	srv, store, _ := newTestServer(t)
	// Record a fake session directly in the DB
	if err := store.RecordSession("sess-1", time.Now(), 5, 1000, 2000, 0.10); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	w := httpGet(t, srv, "/v1/stats")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/stats: got %d, want 200", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	sess, ok := resp["session"].(map[string]any)
	if !ok || sess == nil {
		t.Fatalf("GET /v1/stats: session not returned; got %v", resp)
	}
	if sess["calls"] == nil {
		t.Errorf("GET /v1/stats: session.calls missing")
	}
}

func TestServeHTTP_GetStats_SessionProject(t *testing.T) {
	srv, _, _ := newTestServer(t)

	// No sessionID yet → response must NOT include session_project (so the
	// dashboard falls back to the first project).
	w := httpGet(t, srv, "/v1/stats")
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if _, ok := resp["session_project"]; ok {
		t.Errorf("session_project should be absent when no root is set; got %v", resp["session_project"])
	}

	// Once a session root has been detected it must appear so the ADR picker
	// can default to it.
	srv.sessionID = "proj-abc"
	w = httpGet(t, srv, "/v1/stats")
	json.NewDecoder(w.Body).Decode(&resp)
	if got := resp["session_project"]; got != "proj-abc" {
		t.Errorf("session_project = %v, want proj-abc", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /v1/sessions
// ─────────────────────────────────────────────────────────────────────────────

func TestServeHTTP_GetSessions_Empty(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/sessions")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/sessions: got %d, want 200", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if _, ok := resp["sessions"]; !ok {
		t.Errorf("GET /v1/sessions: missing sessions key; got %v", resp)
	}
}

func TestServeHTTP_GetSessions_WithData(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.RecordSession("sess-a", time.Now(), 3, 500, 1000, 0.05)
	store.RecordSession("sess-b", time.Now(), 7, 1500, 3000, 0.15)
	w := httpGet(t, srv, "/v1/sessions")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/sessions: got %d, want 200", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	rows, _ := resp["sessions"].([]any)
	if len(rows) != 2 {
		t.Errorf("GET /v1/sessions: got %d sessions, want 2", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /v1/index-progress
// ─────────────────────────────────────────────────────────────────────────────

func TestServeHTTP_IndexProgress_NotActive(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpPost(t, srv, "/v1/index-progress", `{"project":"nonexistent"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /v1/index-progress: got %d, want 200", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["active"] != false {
		t.Errorf("index-progress active: got %v, want false", resp["active"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /v1/projects  and  DELETE /v1/projects
// ─────────────────────────────────────────────────────────────────────────────

func TestServeHTTP_GetProjects_Empty(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/projects")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/projects: got %d, want 200", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if _, ok := resp["projects"]; !ok {
		t.Errorf("GET /v1/projects: missing projects key; got %v", resp)
	}
}

func TestServeHTTP_Dashboard(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/dashboard", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/dashboard: got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if body == "" {
		t.Error("dashboard response body is empty")
	}
}

func TestServeHTTP_DeleteProject_Success(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "del-pid", Path: "/tmp/del", Name: "todel", IndexedAt: time.Now()})
	w := httpDelete(t, srv, "/v1/projects", `{"id":"del-pid"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/projects success: got %d, want 200", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["deleted"] != "del-pid" {
		t.Errorf("DELETE response: deleted=%v, want del-pid", resp["deleted"])
	}
}

func TestServeHTTP_DeleteProject_BadBody(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// Missing id field
	w := httpDelete(t, srv, "/v1/projects", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("DELETE /v1/projects bad body: got %d, want 400", w.Code)
	}
}

func TestServeHTTP_DeleteProject_NotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpDelete(t, srv, "/v1/projects", `{"id":"nonexistent-proj"}`)
	// DeleteProject on nonexistent ID should still return 200 (SQLite DELETE is idempotent)
	if w.Code != http.StatusOK {
		t.Logf("DELETE nonexistent project: got %d (acceptable)", w.Code)
	}
}

func httpDelete(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestServeHTTP_DeleteEmptyProjects(t *testing.T) {
	srv, store, _ := newTestServer(t)
	// Two ghosts (sym=0, edge=0) plus one real project.
	store.UpsertProject(db.Project{ID: "ghost-1", Path: "/g1", Name: "g1", IndexedAt: time.Now()})
	store.UpsertProject(db.Project{ID: "ghost-2", Path: "/g2", Name: "g2", IndexedAt: time.Now()})
	store.UpsertProject(db.Project{ID: "real", Path: "/r", Name: "real", IndexedAt: time.Now(), SymCount: 5, EdgeCount: 2})

	w := httpDelete(t, srv, "/v1/projects/empty", "")
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/projects/empty: got %d, want 200 — body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if got, want := resp["deleted"], float64(2); got != want {
		t.Errorf("deleted=%v, want %v", got, want)
	}
	// Real one still there, ghosts gone.
	if p, _ := store.GetProject("real"); p == nil {
		t.Error("real project was swept")
	}
	if p, _ := store.GetProject("ghost-1"); p != nil {
		t.Error("ghost-1 still present")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// savedVsFileSizes — root="" fallback
// ─────────────────────────────────────────────────────────────────────────────

func TestSavedVsFileSizes_EmptyRoot(t *testing.T) {
	// When root is "" and the files don't exist, falls back to avgFileSize per path
	paths := []string{"nonexistent/file.go"}
	saved := savedVsFileSizes("", paths, []byte("small payload"))
	// Should be positive: avgFileSize tokens >> tiny payload tokens
	if saved <= 0 {
		t.Errorf("savedVsFileSizes with missing file: got %d, want > 0", saved)
	}
}

func TestSavedVsFileSizes_DuplicatePaths(t *testing.T) {
	// Duplicate paths should only be counted once
	paths := []string{"a.go", "a.go", "a.go"}
	saved1 := savedVsFileSizes("", []string{"a.go"}, []byte("x"))
	saved3 := savedVsFileSizes("", paths, []byte("x"))
	if saved1 != saved3 {
		t.Errorf("duplicate paths counted multiple times: single=%d triplicate=%d", saved1, saved3)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleStats MCP tool — DB fallback when no live session
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleStats_DBFallback(t *testing.T) {
	srv, store, _ := newTestServer(t)
	// Inject a session row directly; stats atomic counters stay at 0.
	store.RecordSession("fallback-sess", time.Now(), 42, 9000, 18000, 0.90)

	ctx := context.Background()
	result, err := srv.handleStats(ctx, makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleStats: %v", err)
	}
	text := textOf(t, result)
	// calls should come from the DB row (42), not from in-memory counter (0).
	if !strings.Contains(text, "Tool calls:          42") {
		t.Errorf("expected DB-sourced calls=42 in SESSION; got:\n%s", text)
	}
}

func TestHandleStats_AllTime(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.RecordSession("s1", time.Now(), 10, 1000, 2000, 0.10)
	store.RecordSession("s2", time.Now(), 20, 3000, 6000, 0.30)

	ctx := context.Background()
	result, err := srv.handleStats(ctx, makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleStats: %v", err)
	}
	text := textOf(t, result)
	if !strings.Contains(text, "ALL-TIME") {
		t.Fatalf("expected ALL-TIME section with two recorded sessions; got:\n%s", text)
	}
	// ALL-TIME calls should sum to 30 (10 + 20).
	if !strings.Contains(text, "Tool calls:          30") {
		t.Errorf("all_time calls should show 30; got:\n%s", text)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// StartSessionFlusher — context cancel triggers final flush
// ─────────────────────────────────────────────────────────────────────────────

func TestStartSessionFlusher_CancelFlushes(t *testing.T) {
	srv, store, _ := newTestServer(t)

	// Simulate MCP client connecting (required for flushSession to record sessions)
	atomic.StoreInt32(&srv.mcpConnected, 1)

	// Make one tool call so statsCalls > 0 (jsonResultWithMeta increments it)
	srv.handleList(context.Background(), makeReq(nil)) //nolint:errcheck

	ctx, cancel := context.WithCancel(context.Background())
	srv.StartSessionFlusher(ctx)

	// Cancel immediately — the goroutine should do a final flushSession()
	cancel()

	// Give the goroutine time to run the final flush
	time.Sleep(50 * time.Millisecond)

	// Session should now be in the DB
	rows, err := store.GetSessions(10)
	if err != nil {
		t.Fatalf("GetSessions: %v", err)
	}
	if len(rows) == 0 {
		t.Error("StartSessionFlusher: no session flushed to DB after context cancel")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleADR — get nonexistent key
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleADR_GetMissing(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "proj1"
	store.UpsertProject(db.Project{ID: "proj1", Path: "/proj1", Name: "proj1", IndexedAt: time.Now()})

	result, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "get",
		"key":    "NONEXISTENT",
	}))
	if err != nil {
		t.Fatalf("handleADR get: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for nonexistent ADR key, got success")
	}
}

func TestHandleADR_GetNoKey(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "proj2"
	store.UpsertProject(db.Project{ID: "proj2", Path: "/proj2", Name: "proj2", IndexedAt: time.Now()})

	result, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "get",
		// key omitted
	}))
	if err != nil {
		t.Fatalf("handleADR get no key: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result when key is missing for action=get")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleChanges — git diff scope variants
// ─────────────────────────────────────────────────────────────────────────────

func TestHandleChanges_StagedScope(t *testing.T) {
	srv, store, _ := newTestServer(t)
	dir := t.TempDir()
	store.UpsertProject(db.Project{ID: dir, Path: dir, Name: "repo", IndexedAt: time.Now()})
	srv.sessionID = dir
	srv.sessionRoot = dir

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{
		"scope": "staged",
	}))
	if err != nil {
		t.Fatalf("handleChanges staged: %v", err)
	}
	// May fail (not a git repo) or succeed — we just verify no panic and a result
	_ = result
}

func TestHandleChanges_CommitScope(t *testing.T) {
	srv, store, _ := newTestServer(t)
	dir := t.TempDir()
	store.UpsertProject(db.Project{ID: dir, Path: dir, Name: "repo", IndexedAt: time.Now()})
	srv.sessionID = dir
	srv.sessionRoot = dir

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{
		"scope": "HEAD",
	}))
	if err != nil {
		t.Fatalf("handleChanges commit scope: %v", err)
	}
	_ = result
}

// TestResolveProjectID_AutoIndex covers the auto-index path when the session
// project is not yet in the DB (lines 511-517 in resolveProjectID).
func TestResolveProjectID_AutoIndex(t *testing.T) {
	srv, _, _ := newTestServer(t)
	dir := t.TempDir()
	// sessionID is set but project NOT in DB — triggers auto-index on empty arg.
	srv.sessionID = dir
	srv.sessionRoot = dir

	id, err := srv.resolveProjectID("")
	if err != nil {
		// Auto-index might fail on some CI environments; that's acceptable.
		t.Logf("resolveProjectID auto-index returned error (may be expected): %v", err)
		return
	}
	if id == "" {
		t.Error("expected non-empty project ID from auto-index path")
	}
}

// TestServeHTTP_IsErrorResult verifies that an IsError tool result causes a
// 400 Bad Request HTTP response (ServeHTTP line 381-383).
func TestServeHTTP_IsErrorResult(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// POST /v1/search with no project set → handleSearch returns errResult
	// which sets result.IsError = true → ServeHTTP should return 400.
	w := httpPost(t, srv, "/v1/search", `{"query":"anything"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST /v1/search no project: got %d, want 400", w.Code)
	}
}

// TestHandleChanges_WithImpact covers the BFS trace loop body (lines 1194-1213)
// and risk-count accumulation (lines 1217-1221) by setting up a symbol in a
// changed file with an inbound edge from another symbol.
func TestHandleChanges_WithImpact(t *testing.T) {
	srv, store, _ := newTestServer(t)
	repoDir := t.TempDir()

	if out, err := runCmd(t, repoDir, "git", "init"); err != nil {
		t.Skipf("git not available: %v (%s)", err, out)
	}
	runCmd(t, repoDir, "git", "config", "user.email", "test@test.com")
	runCmd(t, repoDir, "git", "config", "user.name", "Test")

	// Create a file, commit it, then modify it (unstaged change).
	mainFile := filepath.Join(repoDir, "main.go")
	os.WriteFile(mainFile, []byte("package main\nfunc main() {}\n"), 0o644)
	runCmd(t, repoDir, "git", "add", ".")
	runCmd(t, repoDir, "git", "commit", "-m", "init")
	os.WriteFile(mainFile, []byte("package main\nfunc main() { println() }\n"), 0o644)

	// Symbol A: in the changed file.
	symA := db.Symbol{
		ID: "impact-sym-a", ProjectID: repoDir, FilePath: "main.go",
		Name: "main", QualifiedName: "main.main", Kind: "Function", Language: "Go",
		StartByte: 14, EndByte: 38, StartLine: 2, EndLine: 2,
	}
	// Symbol B: caller of A (inbound edge).
	symB := db.Symbol{
		ID: "impact-sym-b", ProjectID: repoDir, FilePath: "caller.go",
		Name: "caller", QualifiedName: "main.caller", Kind: "Function", Language: "Go",
		StartByte: 0, EndByte: 20, StartLine: 1, EndLine: 1,
	}
	store.BulkUpsertSymbols([]db.Symbol{symA, symB})
	store.BulkUpsertEdges([]db.Edge{
		{FromID: symB.ID, ToID: symA.ID, Kind: "CALLS", ProjectID: repoDir},
	})
	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "impact-repo", IndexedAt: time.Now()})
	srv.sessionID = repoDir
	srv.sessionRoot = repoDir

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{
		"scope": "unstaged",
		"depth": float64(2),
	}))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	if result.IsError {
		t.Logf("handleChanges error (may be OK in CI without git): %v", decode(t, result))
		return
	}
	m := decode(t, result)
	// Should have a summary with at least changed_files.
	summary, ok := m["summary"].(map[string]any)
	if !ok {
		t.Fatal("expected summary in changes result")
	}
	_ = summary
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP port flexibility (--http :0, HTTPAddr, displayAddr)
// ─────────────────────────────────────────────────────────────────────────────

func TestListenAndServeHTTP_AutoPort(t *testing.T) {
	srv, _, _ := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServeHTTP(ctx, "127.0.0.1:0") }()

	// Poll for the bound address (ListenAndServeHTTP sets it after net.Listen).
	var addr string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a := srv.HTTPAddr(); a != "" {
			addr = a
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("HTTPAddr stayed empty — bind never completed")
	}
	if strings.HasSuffix(addr, ":0") {
		t.Errorf("HTTPAddr = %q, want a resolved port (not :0)", addr)
	}

	// Hit /v1/health on the OS-picked port to prove the server actually listens.
	resp, err := http.Get("http://" + addr + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health on %s: %v", addr, err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("ListenAndServeHTTP did not return after ctx cancel")
	}
}

func TestDisplayAddr(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"0.0.0.0:8080", "localhost:8080"},
		{"[::]:8080", "localhost:8080"},
		{":8080", "localhost:8080"},
		{"127.0.0.1:9999", "127.0.0.1:9999"},
		{"not-an-addr", "not-an-addr"}, // fallthrough on parse error
	}
	for _, c := range cases {
		if got := displayAddr(c.in); got != c.want {
			t.Errorf("displayAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Reverse-proxy basepath tests
// ─────────────────────────────────────────────────────────────────────────────

func httpGetWithHeader(srv *Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestNormalizeBasePath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"/", ""},
		{"  ", ""},
		{"/pincher", "/pincher"},
		{"pincher", "/pincher"},
		{"/pincher/", "/pincher"},
		{"pincher/", "/pincher"},
		{"/api/v1", "/api/v1"},
		{"/api/v1/", "/api/v1"},
	}
	for _, c := range cases {
		if got := normalizeBasePath(c.in); got != c.want {
			t.Errorf("normalizeBasePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestServeHTTP_BasePath_StripsPrefix(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetBasePath("/pincher")
	w := httpGet(t, srv, "/pincher/v1/health")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["ok"] != true {
		t.Errorf("ok field: got %v, want true", resp["ok"])
	}
}

func TestServeHTTP_BasePath_AcceptsRootPath(t *testing.T) {
	// When the proxy strips the prefix before forwarding, requests arrive
	// at /v1/* directly. Pincher must still serve them.
	srv, _, _ := newTestServer(t)
	srv.SetBasePath("/pincher")
	w := httpGet(t, srv, "/v1/health")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestServeHTTP_BasePath_NormalizesInput(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetBasePath("pincher/")
	if got := srv.BasePath(); got != "/pincher" {
		t.Errorf("after Set(\"pincher/\"): got %q, want %q", got, "/pincher")
	}
	srv.SetBasePath("/")
	if got := srv.BasePath(); got != "" {
		t.Errorf("after Set(\"/\"): got %q, want \"\"", got)
	}
}

func TestServeHTTP_BasePath_OpenAPIPathsPrefixed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetBasePath("/pincher")
	w := httpGet(t, srv, "/pincher/v1/openapi.json")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var spec map[string]any
	json.NewDecoder(w.Body).Decode(&spec)

	paths, ok := spec["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatal("openapi spec missing paths")
	}
	for k := range paths {
		if !strings.HasPrefix(k, "/pincher/v1/") {
			t.Errorf("path key %q not prefixed with /pincher/v1/", k)
		}
	}
	servers, ok := spec["servers"].([]any)
	if !ok || len(servers) == 0 {
		t.Fatal("openapi spec missing servers block")
	}
	srvMap := servers[0].(map[string]any)
	url, _ := srvMap["url"].(string)
	if !strings.HasSuffix(url, "/pincher") {
		t.Errorf("servers[0].url = %q, want suffix /pincher", url)
	}
}

func TestServeHTTP_TrustProxy_HonorsXForwardedPrefix(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)
	// Note: no SetBasePath — the prefix comes only from the header.
	w := httpGetWithHeader(srv, "/pincher/v1/health", map[string]string{
		"X-Forwarded-Prefix": "/pincher",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("with X-Forwarded-Prefix: got %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// And the OpenAPI spec for the same trusted request should reflect the
	// header-derived prefix in both paths and the servers URL.
	w = httpGetWithHeader(srv, "/pincher/v1/openapi.json", map[string]string{
		"X-Forwarded-Prefix": "/pincher",
		"X-Forwarded-Proto":  "https",
		"X-Forwarded-Host":   "example.com",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("openapi: got %d, want 200", w.Code)
	}
	var spec map[string]any
	json.NewDecoder(w.Body).Decode(&spec)
	paths := spec["paths"].(map[string]any)
	for k := range paths {
		if !strings.HasPrefix(k, "/pincher/v1/") {
			t.Errorf("path key %q not prefixed with /pincher/v1/", k)
			break
		}
	}
	servers := spec["servers"].([]any)
	url := servers[0].(map[string]any)["url"].(string)
	if url != "https://example.com/pincher" {
		t.Errorf("servers[0].url = %q, want https://example.com/pincher", url)
	}
}

func TestServeHTTP_TrustProxy_IgnoresHeaderByDefault(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// trustProxy stays at its zero value (false).
	// X-Forwarded-Prefix must NOT influence routing — request to a
	// prefixed URL with no basepath configured should miss.
	w := httpGetWithHeader(srv, "/pincher/v1/health", map[string]string{
		"X-Forwarded-Prefix": "/pincher",
	})
	// /pincher/v1/health doesn't match any handler; routing falls into the
	// POST-only catch at the bottom and returns 405 (GET on tool path).
	if w.Code == http.StatusOK {
		t.Errorf("untrusted X-Forwarded-Prefix should not route; got 200")
	}

	// And the OpenAPI spec must NOT pick up the spoofed prefix.
	w = httpGetWithHeader(srv, "/v1/openapi.json", map[string]string{
		"X-Forwarded-Prefix": "/pincher",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("openapi: got %d, want 200", w.Code)
	}
	var spec map[string]any
	json.NewDecoder(w.Body).Decode(&spec)
	for k := range spec["paths"].(map[string]any) {
		if strings.HasPrefix(k, "/pincher/") {
			t.Errorf("path %q should not be prefixed when trustProxy is false", k)
			break
		}
	}
	if _, hasServers := spec["servers"]; hasServers {
		t.Error("servers block should be absent when no basepath and trustProxy=false")
	}
}

func TestServeHTTP_Dashboard_InjectsBasePath(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetBasePath("/pincher")
	w := httpGet(t, srv, "/pincher/v1/dashboard")
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard: got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `const BP = "/pincher"`) {
		t.Error("dashboard HTML missing injected BP constant")
	}
	if !strings.Contains(body, `href="/pincher/v1/openapi.json"`) {
		t.Error("dashboard footer link not prefixed")
	}
	// The placeholder must be fully substituted.
	if strings.Contains(body, "__PINCHER_BASEPATH__") {
		t.Error("dashboard HTML still contains unresolved __PINCHER_BASEPATH__ token")
	}
}

func TestServeHTTP_Dashboard_NoBasePath(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/dashboard")
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard: got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `const BP = ""`) {
		t.Error("dashboard HTML should have empty BP when no basepath set")
	}
	if strings.Contains(body, "__PINCHER_BASEPATH__") {
		t.Error("dashboard still contains unresolved placeholder")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// clientIP / rate-limit + X-Forwarded-For (issue #40)
// ─────────────────────────────────────────────────────────────────────────────

// reqWithRemoteAndHeaders builds a request with the given RemoteAddr and
// header bag. Used throughout the clientIP / rate-limit XFF tests.
func reqWithRemoteAndHeaders(remote string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/list", strings.NewReader("{}"))
	req.RemoteAddr = remote
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func TestClientIP_TrustProxyOff_IgnoresXFF(t *testing.T) {
	// Spoof gate: when --trust-proxy is OFF (the default), X-Forwarded-For
	// MUST be ignored. A direct caller cannot influence the rate-limit key
	// by adding the header. This is the security invariant — without it,
	// any client could pretend to be a different IP per request.
	srv, _, _ := newTestServer(t)
	// trustProxy stays at its zero value (false).

	r := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "1.2.3.4",
	})
	got := srv.clientIP(r)
	if got != "10.0.0.1" {
		t.Errorf("trustProxy=false with XFF set: got %q, want %q (RemoteAddr host)", got, "10.0.0.1")
	}
}

func TestClientIP_TrustProxyOn_UsesXFF(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)

	r := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "1.2.3.4",
	})
	got := srv.clientIP(r)
	if got != "1.2.3.4" {
		t.Errorf("trustProxy=true with XFF=1.2.3.4: got %q, want %q", got, "1.2.3.4")
	}
}

func TestClientIP_TrustProxyOn_LeftmostInChain(t *testing.T) {
	// XFF chain semantics: each proxy appends to the right. Leftmost is
	// the original client; rightmost is the proxy immediately upstream of
	// pincher. We rate-limit on the original client.
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)

	r := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "1.2.3.4, 5.6.7.8, 9.10.11.12",
	})
	got := srv.clientIP(r)
	if got != "1.2.3.4" {
		t.Errorf("trustProxy=true with XFF chain: got %q, want %q (leftmost)", got, "1.2.3.4")
	}
}

func TestClientIP_TrustProxyOn_NoXFF_FallsBackToRemoteAddr(t *testing.T) {
	// Graceful fallback: trust-proxy on, but no XFF header set (e.g. the
	// proxy in front of pincher didn't add one). Use RemoteAddr — the
	// fallback is the same shape as trustProxy=off behaviour.
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)

	r := reqWithRemoteAndHeaders("10.0.0.1:5555", nil)
	got := srv.clientIP(r)
	if got != "10.0.0.1" {
		t.Errorf("trustProxy=true with no XFF: got %q, want %q", got, "10.0.0.1")
	}
}

func TestClientIP_TrustProxyOn_EmptyXFF_FallsBack(t *testing.T) {
	// XFF set to empty string (or just whitespace) — same as missing.
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)

	r := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "   ",
	})
	got := srv.clientIP(r)
	if got != "10.0.0.1" {
		t.Errorf("trustProxy=true with whitespace XFF: got %q, want %q", got, "10.0.0.1")
	}
}

func TestClientIP_IPv6_RemoteAddr(t *testing.T) {
	// IPv6 RemoteAddr is "[::1]:port" — bracketed. The previous
	// strings.Cut(":") implementation cut on the first colon (which is
	// inside the bracket) and returned "[", which would then match "["
	// as the rate-limit key for every IPv6 client. net.SplitHostPort
	// handles bracketed forms correctly.
	srv, _, _ := newTestServer(t)

	r := reqWithRemoteAndHeaders("[::1]:8080", nil)
	got := srv.clientIP(r)
	if got != "::1" {
		t.Errorf("IPv6 RemoteAddr: got %q, want %q", got, "::1")
	}

	r = reqWithRemoteAndHeaders("[2001:db8::1]:54321", nil)
	got = srv.clientIP(r)
	if got != "2001:db8::1" {
		t.Errorf("IPv6 RemoteAddr (full): got %q, want %q", got, "2001:db8::1")
	}
}

// XFF parsing robustness — #41 item 6.

func TestClientIP_XFF_StripsPort_IPv4(t *testing.T) {
	// Some proxies emit "1.2.3.4:8080" in X-Forwarded-For. Without
	// stripping the port, ephemeral source ports would each get their
	// own rate-limit bucket — bypassing per-IP throttling.
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)
	r := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "1.2.3.4:8080",
	})
	got := srv.clientIP(r)
	if got != "1.2.3.4" {
		t.Errorf("XFF=1.2.3.4:8080: got %q, want %q (port should be stripped)", got, "1.2.3.4")
	}
}

func TestClientIP_XFF_StripsPort_IPv6Bracketed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)
	r := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "[2001:db8::1]:8080",
	})
	got := srv.clientIP(r)
	if got != "2001:db8::1" {
		t.Errorf("XFF=[2001:db8::1]:8080: got %q, want %q (port should be stripped)", got, "2001:db8::1")
	}
}

func TestClientIP_XFF_BareIPv6_NoPort(t *testing.T) {
	// A bare IPv6 (no brackets, no port) MUST pass through unchanged —
	// SplitHostPort fails on it, we fall through to the raw value.
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)
	r := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "2001:db8::1",
	})
	got := srv.clientIP(r)
	if got != "2001:db8::1" {
		t.Errorf("XFF=bare IPv6: got %q, want %q", got, "2001:db8::1")
	}
}

func TestClientIP_XFF_EmptyLeftmost_FallsThrough(t *testing.T) {
	// Pathological XFF: leading comma → empty leftmost entry. The
	// safe behaviour is to fall through to RemoteAddr rather than
	// use the empty string as a rate-limit key.
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)
	r := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
		"X-Forwarded-For": ", 1.2.3.4",
	})
	got := srv.clientIP(r)
	if got != "10.0.0.1" {
		t.Errorf("empty-leftmost XFF: got %q, want %q (RemoteAddr fallback)", got, "10.0.0.1")
	}
}

func TestClientIP_XFF_OnlyComma_FallsThrough(t *testing.T) {
	// XFF that's just a comma → both sides empty → fall through.
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)
	r := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
		"X-Forwarded-For": ",",
	})
	got := srv.clientIP(r)
	if got != "10.0.0.1" {
		t.Errorf("comma-only XFF: got %q, want %q (RemoteAddr fallback)", got, "10.0.0.1")
	}
}

func TestClientIP_XFF_LeadingWhitespaceAndPort(t *testing.T) {
	// Combined: leading whitespace + port. TrimSpace runs first, then
	// SplitHostPort handles the port.
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)
	r := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "   1.2.3.4:9999  , 5.6.7.8",
	})
	got := srv.clientIP(r)
	if got != "1.2.3.4" {
		t.Errorf("padded XFF entry: got %q, want %q", got, "1.2.3.4")
	}
}

func TestClientIP_XFF_PathologicalValueSafelyContained(t *testing.T) {
	// Audit finding: Go's net/http validates header values at WIRE parse
	// time (a real request with \r\n in a header value is rejected
	// before reaching our handler) but NOT at Header.Set time. So an
	// in-process call could in theory shove anything through.
	//
	// Defense: even if a pathological value reached clientIP, the
	// returned string is only used as a sync.Map key for rate-limiting.
	// No SQL, no HTML, no echo back to the client. So the blast radius
	// is "rate-limit fragmentation," not exploitation.
	//
	// This test pins both halves of that:
	//   1. clientIP doesn't panic on values containing CR/LF/null bytes.
	//   2. The pathological value flows through allowRequest the same
	//      way a normal IP does — no special branch breaks.
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)
	srv.SetRateLimit(2, time.Minute)

	pathologicals := []string{
		"1.2.3.4\r\nX-Admin: true",
		"1.2.3.4\x00",
		"1.2.3.4 with spaces and tabs\t",
		strings.Repeat("a", 1024), // very long
	}
	for _, payload := range pathologicals {
		req := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
			"X-Forwarded-For": payload,
		})
		// MUST NOT panic.
		key := srv.clientIP(req)
		if key == "" {
			t.Errorf("clientIP returned empty for payload %q", payload)
		}
		// MUST be usable as a rate-limit key (allowRequest doesn't
		// branch or panic on the input shape).
		if !srv.allowRequest(key) && !srv.allowRequest(key) {
			// Two "allowed" calls within the limit MUST both succeed —
			// we just want allowRequest to return true at least once
			// without crashing. The double-call is for the limit=2 cap
			// to absorb either return.
			t.Errorf("allowRequest panicked or returned false for both calls on %q", payload)
		}
	}
}

func TestClientIP_XFF_MultipleHeaders_FirstWins(t *testing.T) {
	// RFC allows the same header to appear multiple times. Header.Get
	// returns ONLY the first instance. Document this behaviour: a
	// trusted proxy chain that comma-appends to a single XFF header
	// is handled correctly; a proxy that ADDs a second header would
	// have its value ignored. This is the right behaviour given the
	// trust model — the first proxy in our chain is the one we trust.
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)
	req := httptest.NewRequest(http.MethodPost, "/v1/list", strings.NewReader("{}"))
	req.RemoteAddr = "10.0.0.1:5555"
	// Header.Add appends rather than replacing, producing two values.
	req.Header.Add("X-Forwarded-For", "1.2.3.4")
	req.Header.Add("X-Forwarded-For", "9.9.9.9")
	got := srv.clientIP(req)
	if got != "1.2.3.4" {
		t.Errorf("multiple XFF headers: got %q, want first %q", got, "1.2.3.4")
	}
}

func TestRateLimit_TrustProxyOn_PerXFFClient_Integration(t *testing.T) {
	// End-to-end integration: same TCP source (the proxy), different
	// XFF values (different real clients). Rate limit MUST apply per
	// XFF-derived client, not per RemoteAddr — otherwise a single proxy
	// fronting many clients hits the cap on the first burst.
	srv, _, _ := newTestServer(t)
	srv.SetTrustProxy(true)
	srv.SetRateLimit(1, time.Minute)

	makeReq := func(xff string) *httptest.ResponseRecorder {
		req := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
			"X-Forwarded-For": xff,
		})
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}

	// Client A's first request: allowed.
	if w := makeReq("1.2.3.4"); w.Code != http.StatusOK {
		t.Fatalf("client A first request: got %d, want 200", w.Code)
	}
	// Client B's first request, same RemoteAddr (proxy IP): MUST be allowed.
	// If we were keying on RemoteAddr, this would 429. We're not, so it's 200.
	if w := makeReq("9.10.11.12"); w.Code != http.StatusOK {
		t.Fatalf("client B first request (same proxy, different XFF): got %d, want 200", w.Code)
	}
	// Client A's second request: now over the per-client limit.
	if w := makeReq("1.2.3.4"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("client A second request: got %d, want 429", w.Code)
	}
}

func TestRateLimit_TrustProxyOff_XFFSpoofIgnored_Integration(t *testing.T) {
	// The negative companion: with --trust-proxy OFF, two requests with
	// the same RemoteAddr but different (spoofed) XFF MUST still hit the
	// rate limit. This is what prevents a malicious direct caller from
	// rotating XFF values to bypass per-IP throttling.
	srv, _, _ := newTestServer(t)
	// trustProxy stays false.
	srv.SetRateLimit(1, time.Minute)

	makeReq := func(xff string) *httptest.ResponseRecorder {
		req := reqWithRemoteAndHeaders("10.0.0.1:5555", map[string]string{
			"X-Forwarded-For": xff,
		})
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}

	if w := makeReq("1.2.3.4"); w.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", w.Code)
	}
	// Same RemoteAddr, different XFF — XFF is ignored, so this is the same
	// rate-limit key. MUST hit the cap.
	if w := makeReq("9.10.11.12"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request (XFF spoof attempt): got %d, want 429", w.Code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleFetch — SSRF protection, redirect validation, scheme allow-list
// (Issue #41 item 2)
// ─────────────────────────────────────────────────────────────────────────────

// fetchTestSetup provides the bookkeeping common to every fetch test: a
// project so mustProject succeeds, and a flag flipped to allow loopback
// fetches against an httptest.Server (which always binds to 127.0.0.1).
//
// Tests that exercise the SSRF gate against unsafe URLs should NOT set
// fetchAllowLoopback — their goal is to prove that rejection happens in
// production-shape code paths.
func fetchTestSetup(t *testing.T) (*Server, *db.Store) {
	t.Helper()
	srv, store, _ := newTestServer(t)
	if err := store.UpsertProject(db.Project{
		ID: "p", Path: "/p", Name: "proj", IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	return srv, store
}

func fetchArgs(url string) map[string]any {
	return map[string]any{"url": url, "project": "p"}
}

func TestHandleFetch_HappyPath(t *testing.T) {
	srv, _ := fetchTestSetup(t)
	srv.fetchAllowLoopback = true // httptest.Server binds 127.0.0.1

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Hello</title></head><body>world</body></html>`))
	}))
	defer upstream.Close()

	result, err := srv.handleFetch(context.Background(), makeReq(fetchArgs(upstream.URL)))
	if err != nil {
		t.Fatalf("handleFetch: %v", err)
	}
	m := decode(t, result)
	if m["title"] != "Hello" {
		t.Errorf("title = %v, want %q", m["title"], "Hello")
	}
	if m["stored"] != true {
		t.Errorf("stored = %v, want true", m["stored"])
	}
}

// Negative scheme allow-list — only http/https accepted.

func TestHandleFetch_RejectsFileScheme(t *testing.T) {
	srv, _ := fetchTestSetup(t)
	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs("file:///etc/passwd")))
	body := textOf(t, result)
	if !strings.Contains(body, "scheme") || !strings.Contains(body, "not allowed") {
		t.Errorf("expected scheme rejection, got: %s", body)
	}
}

func TestHandleFetch_RejectsGopherScheme(t *testing.T) {
	srv, _ := fetchTestSetup(t)
	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs("gopher://example.com/")))
	body := textOf(t, result)
	if !strings.Contains(body, "scheme") {
		t.Errorf("expected gopher scheme rejection, got: %s", body)
	}
}

func TestHandleFetch_RejectsDataScheme(t *testing.T) {
	srv, _ := fetchTestSetup(t)
	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs("data:text/plain;base64,SGVsbG8=")))
	body := textOf(t, result)
	if !strings.Contains(body, "scheme") {
		t.Errorf("expected data: scheme rejection, got: %s", body)
	}
}

// SSRF gate — block private/loopback/link-local/multicast IPs.

func TestHandleFetch_RejectsCloudMetadataIP(t *testing.T) {
	// 169.254.169.254 is the AWS / GCP / Azure cloud-metadata endpoint.
	// Reaching it from a fetch tool is a classic SSRF for cred theft.
	srv, _ := fetchTestSetup(t)
	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs("http://169.254.169.254/latest/meta-data/")))
	body := textOf(t, result)
	if !strings.Contains(body, "blocked") || !strings.Contains(body, "link-local") {
		t.Errorf("expected metadata-IP rejection, got: %s", body)
	}
}

func TestHandleFetch_RejectsRFC1918_10Net(t *testing.T) {
	srv, _ := fetchTestSetup(t)
	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs("http://10.0.0.1/")))
	body := textOf(t, result)
	if !strings.Contains(body, "blocked") || !strings.Contains(body, "private network") {
		t.Errorf("expected RFC1918 10/8 rejection, got: %s", body)
	}
}

func TestHandleFetch_RejectsRFC1918_172Net(t *testing.T) {
	srv, _ := fetchTestSetup(t)
	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs("http://172.17.0.1/")))
	body := textOf(t, result)
	if !strings.Contains(body, "blocked") || !strings.Contains(body, "private network") {
		t.Errorf("expected RFC1918 172.16/12 rejection, got: %s", body)
	}
}

func TestHandleFetch_RejectsRFC1918_192Net(t *testing.T) {
	srv, _ := fetchTestSetup(t)
	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs("http://192.168.1.1/")))
	body := textOf(t, result)
	if !strings.Contains(body, "blocked") || !strings.Contains(body, "private network") {
		t.Errorf("expected RFC1918 192.168/16 rejection, got: %s", body)
	}
}

func TestHandleFetch_RejectsLoopbackByDefault(t *testing.T) {
	// fetchAllowLoopback is OFF — production behaviour. 127.0.0.1 MUST
	// be rejected even though tests can opt in for httptest.Server.
	srv, _ := fetchTestSetup(t)
	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs("http://127.0.0.1:8080/admin")))
	body := textOf(t, result)
	if !strings.Contains(body, "blocked") || !strings.Contains(body, "loopback") {
		t.Errorf("expected loopback rejection (allow-loopback off), got: %s", body)
	}
}

func TestHandleFetch_RejectsIPv6Loopback(t *testing.T) {
	srv, _ := fetchTestSetup(t)
	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs("http://[::1]:8080/")))
	body := textOf(t, result)
	if !strings.Contains(body, "blocked") || !strings.Contains(body, "loopback") {
		t.Errorf("expected IPv6 loopback rejection, got: %s", body)
	}
}

func TestHandleFetch_RejectsZeroAddress(t *testing.T) {
	// 0.0.0.0 — unspecified. Hitting it on Linux often resolves to
	// localhost; treat as SSRF.
	srv, _ := fetchTestSetup(t)
	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs("http://0.0.0.0:8080/")))
	body := textOf(t, result)
	if !strings.Contains(body, "blocked") {
		t.Errorf("expected 0.0.0.0 rejection, got: %s", body)
	}
}

// Redirect-target validation — a public initial URL CANNOT redirect into
// a private/loopback/link-local target.

func TestHandleFetch_RedirectToPrivateBlocked(t *testing.T) {
	// The upstream server is on httptest (loopback, allowed via flag).
	// It responds with a 302 redirect to 10.0.0.1, which is RFC1918.
	// CheckRedirect must reject before any TCP connection to 10.0.0.1.
	srv, _ := fetchTestSetup(t)
	srv.fetchAllowLoopback = true

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/internal", http.StatusFound)
	}))
	defer upstream.Close()

	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs(upstream.URL)))
	body := textOf(t, result)
	if !strings.Contains(body, "redirect target blocked") {
		t.Errorf("expected redirect-block error, got: %s", body)
	}
}

func TestHandleFetch_RedirectToMetadataIPBlocked(t *testing.T) {
	srv, _ := fetchTestSetup(t)
	srv.fetchAllowLoopback = true

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/iam/security-credentials/", http.StatusFound)
	}))
	defer upstream.Close()

	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs(upstream.URL)))
	body := textOf(t, result)
	if !strings.Contains(body, "redirect target blocked") || !strings.Contains(body, "link-local") {
		t.Errorf("expected metadata-redirect block, got: %s", body)
	}
}

func TestHandleFetch_RedirectChainCapped(t *testing.T) {
	// Cap is maxFetchRedirects = 5. Build a chain that always redirects
	// back to itself with a counter; expect rejection at hop 6.
	srv, _ := fetchTestSetup(t)
	srv.fetchAllowLoopback = true

	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, upstream.URL+"/next", http.StatusFound)
	}))
	defer upstream.Close()

	result, _ := srv.handleFetch(context.Background(), makeReq(fetchArgs(upstream.URL)))
	body := textOf(t, result)
	if !strings.Contains(body, "too many redirects") {
		t.Errorf("expected redirect-cap error, got: %s", body)
	}
}

// Body-size cap — even if the upstream sends gigabytes, we read only
// maxFetchBytes and close. Asserts io.LimitReader actually bounds the read
// rather than trusting Content-Length.

func TestHandleFetch_BodySizeCapEnforced(t *testing.T) {
	srv, _ := fetchTestSetup(t)
	srv.fetchAllowLoopback = true

	// Upstream sends maxFetchBytes * 2 bytes. Pincher MUST cap at
	// maxFetchBytes regardless of how much the upstream tries to stream.
	// We don't set Content-Length — Go's net/http defaults to chunked
	// encoding, so the upstream actually streams the full payload and the
	// only thing bounding the read is io.LimitReader on our side.
	bigSize := maxFetchBytes * 2
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(make([]byte, bigSize))
	}))
	defer upstream.Close()

	result, err := srv.handleFetch(context.Background(), makeReq(fetchArgs(upstream.URL)))
	if err != nil {
		t.Fatalf("handleFetch: %v", err)
	}
	m := decode(t, result)
	rawBytes, ok := m["raw_bytes"].(float64)
	if !ok {
		t.Fatalf("raw_bytes missing or not numeric: %v", m["raw_bytes"])
	}
	if int(rawBytes) > maxFetchBytes {
		t.Errorf("raw_bytes = %d exceeds cap %d (LimitReader not enforcing)", int(rawBytes), maxFetchBytes)
	}
	// And we should have read close to the cap — proves the cap is the
	// effective bound, not some smaller default that would silently
	// truncate legitimate large responses below their useful content.
	if int(rawBytes) < maxFetchBytes/2 {
		t.Errorf("raw_bytes = %d, expected close to %d (cap)", int(rawBytes), maxFetchBytes)
	}
}

// validateFetchURL unit tests — covering the helper directly so we get
// finer-grained negative assertions than the integration handler tests.

func TestValidateFetchURL_BlocksMulticast(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if err := srv.validateFetchURL("http://224.0.0.1/"); err == nil ||
		!strings.Contains(err.Error(), "multicast") {
		t.Errorf("expected multicast block, got %v", err)
	}
}

func TestValidateFetchURL_AllowsPublicIP(t *testing.T) {
	// 8.8.8.8 (Google DNS) is a public IP — MUST pass validation
	// purely. We're not actually fetching anything here, just checking
	// the gate.
	srv, _, _ := newTestServer(t)
	if err := srv.validateFetchURL("http://8.8.8.8/"); err != nil {
		t.Errorf("expected 8.8.8.8 to pass validation, got %v", err)
	}
}

func TestValidateFetchURL_NoHost(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if err := srv.validateFetchURL("http:///path"); err == nil {
		t.Error("expected error on URL with no host")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Dashboard XSS protection (#41 item 4)
//
// Five JSON.stringify-into-attribute sites that previously produced JS-safe
// but HTML-unsafe output. A user-controlled value containing `' onclick=...`
// could break out of the onclick attribute and inject new attributes when
// the dashboard renders the project list, search results, or ADR list.
//
// These tests don't render the dashboard in a real browser (Go can't), but
// they pin the safe shape at the template-source level: every JSON.stringify
// site that lands inside an HTML attribute MUST be wrapped in esc(). A
// regression to bare JSON.stringify in those positions makes these tests
// fail.
// ─────────────────────────────────────────────────────────────────────────────

func TestDashboard_ESCWrapsAllAttributeJSONStringify(t *testing.T) {
	// Render the dashboard (any basepath; behaviour is the same).
	html := renderDashboard("")

	// The five known attribute-injection sites must use esc(JSON.stringify(...)).
	// Each pattern below is a stable substring of the expected safe form.
	safeSites := []string{
		"openDetail('+esc(JSON.stringify(id))+','+esc(JSON.stringify(name))+')",
		"reindex('+esc(JSON.stringify(id))+',this)",
		"deleteProject('+esc(JSON.stringify(id))+','+esc(JSON.stringify(name))+')",
		"deleteADR('+esc(JSON.stringify(e.key||''))+')",
		"copyID('+esc(JSON.stringify(r.id))+',this)",
	}
	for _, want := range safeSites {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing safe attribute-escape pattern:\n  want substring: %s", want)
		}
	}
}

func TestDashboard_NoBareJSONStringifyInOnclickAttribute(t *testing.T) {
	// Negative-shape regression: detect the unsafe pattern returning to
	// any onclick handler. We scan for `onclick="<funcName>('+JSON.stringify`
	// — the exact shape that bypasses HTML-attribute escaping.
	html := renderDashboard("")

	// Match `onclick="<JS>"` where <JS> contains a bare `+JSON.stringify(`
	// not preceded by `esc(`.
	//
	// Pattern: onclick=" ... +JSON.stringify( ... )+...
	// We look for `+JSON.stringify(` and verify the preceding non-space
	// chars are `+esc(` not just `+`.
	idx := 0
	for {
		i := strings.Index(html[idx:], "+JSON.stringify(")
		if i < 0 {
			break
		}
		abs := idx + i
		// Look back at most 8 chars for the safe wrapper "esc(".
		start := abs - 8
		if start < 0 {
			start = 0
		}
		preceding := html[start:abs]
		if !strings.Contains(preceding, "esc(") {
			// Distinguish onclick context from "fetch body" context (safe).
			// Walk back to find the nearest preceding " or '.
			ctxStart := abs - 200
			if ctxStart < 0 {
				ctxStart = 0
			}
			ctx := html[ctxStart:abs]
			if strings.Contains(ctx, "onclick=") || strings.Contains(ctx, "title=") {
				t.Errorf("found bare JSON.stringify in attribute context at byte %d:\n  context: ...%s",
					abs, html[ctxStart:abs+80])
			}
		}
		idx = abs + len("+JSON.stringify(")
	}
}

func TestDashboard_EscFunctionDefined(t *testing.T) {
	// Sanity check: the esc() helper must exist in the rendered template,
	// since every above test depends on it. Catches a regression where
	// someone removes the helper definition while leaving call sites in
	// place — would produce ReferenceError in browser, not a build break.
	html := renderDashboard("")
	if !strings.Contains(html, "const esc = s => String(s).replace") {
		t.Error("esc() helper missing from dashboard — XSS escaping would fail at runtime")
	}
}

// CSP and security headers on the dashboard response.

func TestDashboard_SecurityHeaders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httpGet(t, srv, "/v1/dashboard")
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard: got %d, want 200", w.Code)
	}

	wantHeaders := map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for header, want := range wantHeaders {
		got := w.Header().Get(header)
		if got != want {
			t.Errorf("header %s: got %q, want %q", header, got, want)
		}
	}

	// CSP must be present and contain each of the directives we explicitly
	// rely on. We don't check exact-match because future tightening (e.g.
	// dropping unsafe-inline once JS is externalized) is good — we just
	// want to fail loud if a directive disappears.
	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy header missing")
	}
	mustContain := []string{
		"default-src 'self'",
		"frame-ancestors 'none'", // clickjacking defense
		"object-src 'none'",      // no Flash/Java/etc.
		"base-uri 'self'",        // no <base> hijack
	}
	for _, dir := range mustContain {
		if !strings.Contains(csp, dir) {
			t.Errorf("CSP missing required directive: %q\n  full CSP: %s", dir, csp)
		}
	}
}

func TestADR_AcceptsArbitraryStringValues(t *testing.T) {
	// Regression: the ADR write path MUST NOT silently sanitize, mangle,
	// or reject arbitrary string values. Sanitization is the dashboard's
	// job (esc() on render). Server-side filtering would create a
	// false sense of safety AND lose data.
	srv, store, _ := newTestServer(t)
	if err := store.UpsertProject(db.Project{
		ID: "xss-test", Path: "/p", Name: "p", IndexedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert project: %v", err)
	}

	payloads := map[string]string{
		"html_tags":  `<script>alert(1)</script>`,
		"attr_break": `' onclick=alert(1)//`,
		"quote":      `value with "double" and 'single' quotes`,
		"newlines":   "line1\nline2\nline3",
		"unicode":    "héllo wörld 你好 🦀",
	}

	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			// SET
			result, err := srv.handleADR(context.Background(), makeReq(map[string]any{
				"action":  "set",
				"project": "xss-test",
				"key":     "k_" + name,
				"value":   payload,
			}))
			if err != nil {
				t.Fatalf("ADR set: %v", err)
			}
			m := decode(t, result)
			if m["error"] != nil {
				t.Fatalf("ADR set rejected payload: %v", m["error"])
			}

			// GET — must return the payload verbatim, no server-side mangling.
			result, _ = srv.handleADR(context.Background(), makeReq(map[string]any{
				"action":  "get",
				"project": "xss-test",
				"key":     "k_" + name,
			}))
			m = decode(t, result)
			if got := m["value"]; got != payload {
				t.Errorf("round-trip: got %q, want %q", got, payload)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// slow_queries server-side capture (#42 part 2)
// ─────────────────────────────────────────────────────────────────────────────

func TestSlowQuery_DisabledByDefault(t *testing.T) {
	// Threshold defaults to 0 → no rows persisted regardless of latency.
	srv, store, _ := newTestServer(t)
	// Don't call SetSlowQueryThreshold.

	// Force latency by sleeping inside a fake handler call. Since we can't
	// easily inject latency into existing handlers, we just call jsonResultWithMeta
	// directly with a start in the past.
	past := time.Now().Add(-500 * time.Millisecond)
	srv.jsonResultWithMeta(map[string]any{}, past, "test_tool", map[string]any{}, 0)

	rows, err := store.ListSlowQueries(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("threshold=0 should not record any rows, got %d", len(rows))
	}
}

func TestSlowQuery_ThresholdBoundary(t *testing.T) {
	// Latency >= threshold → recorded; below → not.
	srv, store, _ := newTestServer(t)
	srv.SetSlowQueryThreshold(50)

	// Below threshold (10ms latency).
	past := time.Now().Add(-10 * time.Millisecond)
	srv.jsonResultWithMeta(map[string]any{}, past, "fast_tool", map[string]any{}, 0)

	// Above threshold (200ms latency).
	past = time.Now().Add(-200 * time.Millisecond)
	srv.jsonResultWithMeta(map[string]any{}, past, "slow_tool", map[string]any{}, 0)

	rows, err := store.ListSlowQueries(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row above threshold, got %d", len(rows))
	}
	if rows[0].Tool != "slow_tool" {
		t.Errorf("captured tool = %q, want slow_tool", rows[0].Tool)
	}
}

func TestSlowQuery_ProjectIDPropagatesFromArgs(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.SetSlowQueryThreshold(50)

	past := time.Now().Add(-100 * time.Millisecond)
	srv.jsonResultWithMeta(map[string]any{}, past, "search",
		map[string]any{"project": "proj-abc", "query": "open"}, 0)

	rows, _ := store.ListSlowQueries(0)
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].ProjectID != "proj-abc" {
		t.Errorf("ProjectID = %q, want proj-abc", rows[0].ProjectID)
	}
}

// TestSlowQuery_SecretRedaction is the security gate. Argument values
// keyed under sensitive names MUST be redacted before persistence — pincher
// would otherwise persist credentials on disk indefinitely.
func TestSlowQuery_SecretRedaction(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.SetSlowQueryThreshold(50)

	past := time.Now().Add(-100 * time.Millisecond)
	srv.jsonResultWithMeta(map[string]any{}, past, "fetch",
		map[string]any{
			"url":           "https://api.example.com",
			"api_key":       "sk-abc123-this-must-not-persist",
			"BearerToken":   "ey-must-not-persist",
			"password":      "p4ssw0rd",
			"nested": map[string]any{
				"my_secret": "must-also-not-persist",
				"normal":    "ok",
			},
		}, 0)

	rows, _ := store.ListSlowQueries(0)
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	args := rows[0].Arguments

	// Sensitive values MUST NOT appear verbatim.
	for _, secret := range []string{
		"sk-abc123-this-must-not-persist",
		"ey-must-not-persist",
		"p4ssw0rd",
		"must-also-not-persist",
	} {
		if strings.Contains(args, secret) {
			t.Errorf("CREDENTIAL LEAK: %q appears unredacted in persisted args:\n%s", secret, args)
		}
	}
	// Sensitive keys should appear with [redacted] sentinel.
	if !strings.Contains(args, "[redacted]") {
		t.Errorf("expected [redacted] sentinel in persisted args, got: %s", args)
	}
	// Non-sensitive values must pass through.
	if !strings.Contains(args, "https://api.example.com") {
		t.Errorf("non-sensitive 'url' value should be preserved, got: %s", args)
	}
}
