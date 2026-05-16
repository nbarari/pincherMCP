package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1212: pinchQL queries returned testdata/ + __fixtures__/ paths in
// audit-shape results ("find functions without docstrings"). dead_code
// and architecture already filter these via isTestFixturePath; pinchQL
// bypassed it. Now: default-on filter with include_fixtures=true escape
// hatch, paralleling architecture's include_tests pattern.

// Positive: a query that projects file_path drops fixture rows by
// default. A warning fires naming the count + a sample path.
func TestHandleQuery_FixtureFilter_DefaultDropsTestdata(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "q-fxt-pos"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::pkg.Real#Function", ProjectID: pid, FilePath: "internal/server/server.go",
			Name: "Real", QualifiedName: "pkg.Real", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0, IsExported: true},
		{ID: pid + "::pkg.Fixture#Function", ProjectID: pid, FilePath: "testdata/corpus/node-monorepo/src/app.js",
			Name: "Fixture", QualifiedName: "pkg.Fixture", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0, IsExported: true},
	})

	res, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql": `MATCH (n:Function) RETURN n.name, n.file_path`,
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, res)
	rows, _ := body["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after fixture filter; got %d: %v", len(rows), body)
	}
	row := rows[0].(map[string]any)
	if got := row["n.file_path"]; got != "internal/server/server.go" {
		t.Errorf("expected non-fixture row to survive; got %v", got)
	}
	ws := metaWarnings(t, body)
	if !warningsContain(ws, "fixture") && !warningsContain(ws, "testdata") {
		t.Errorf("expected fixture-filter warning; got: %v", ws)
	}
	if !warningsContain(ws, "include_fixtures=true") {
		t.Errorf("warning should name the opt-out parameter; got: %v", ws)
	}
}

// Negative (escape hatch): include_fixtures=true preserves fixture rows
// and emits no fixture warning.
func TestHandleQuery_FixtureFilter_IncludeFixturesTrue_PreservesRows(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "q-fxt-neg"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::pkg.Real#Function", ProjectID: pid, FilePath: "internal/server/server.go",
			Name: "Real", QualifiedName: "pkg.Real", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0, IsExported: true},
		{ID: pid + "::pkg.Fixture#Function", ProjectID: pid, FilePath: "testdata/corpus/node-monorepo/src/app.js",
			Name: "Fixture", QualifiedName: "pkg.Fixture", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0, IsExported: true},
	})

	res, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql":          `MATCH (n:Function) RETURN n.name, n.file_path`,
		"include_fixtures": true,
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, res)
	rows, _ := body["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected both rows when include_fixtures=true; got %d", len(rows))
	}
	for _, w := range metaWarnings(t, body) {
		if strings.Contains(strings.ToLower(w), "fixture") || strings.Contains(w, "testdata") {
			t.Errorf("include_fixtures=true must not emit a fixture filter warning; got: %q", w)
		}
	}
}

// Control: a query that projects only n.name (no file_path) cannot be
// filtered — we can't see the file path. Rows pass through, no warning.
func TestHandleQuery_FixtureFilter_NoFilePathProjected_PassesThrough(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "q-fxt-ctrl"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::pkg.Real#Function", ProjectID: pid, FilePath: "internal/server/server.go",
			Name: "Real", QualifiedName: "pkg.Real", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0, IsExported: true},
		{ID: pid + "::pkg.Fixture#Function", ProjectID: pid, FilePath: "testdata/corpus/node-monorepo/src/app.js",
			Name: "Fixture", QualifiedName: "pkg.Fixture", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0, IsExported: true},
	})

	// No file_path projected — filter can't apply.
	res, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql": `MATCH (n:Function) RETURN n.name`,
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, res)
	rows, _ := body["rows"].([]any)
	if len(rows) != 2 {
		t.Errorf("expected pass-through (2 rows) when file_path not projected; got %d", len(rows))
	}
	for _, w := range metaWarnings(t, body) {
		if strings.Contains(strings.ToLower(w), "fixture") || strings.Contains(w, "testdata") {
			t.Errorf("must not emit fixture warning when file_path is not projected; got: %q", w)
		}
	}
}

// Cross-check: the filter recognizes the same set of fixture directories
// the architecture/dead_code path uses (isTestFixturePath). All five
// canonical fixture dir prefixes get dropped; real code survives.
func TestHandleQuery_FixtureFilter_AllFixtureDirsRecognized(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "q-fxt-cross"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::a.Real#Function", ProjectID: pid, FilePath: "src/real.go",
			Name: "Real", QualifiedName: "a.Real", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0, IsExported: true},
		{ID: pid + "::a.Testdata#Function", ProjectID: pid, FilePath: "internal/testdata/x.go",
			Name: "Testdata", QualifiedName: "a.Testdata", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0, IsExported: true},
		{ID: pid + "::a.Fixtures#Function", ProjectID: pid, FilePath: "src/__fixtures__/x.js",
			Name: "Fixtures", QualifiedName: "a.Fixtures", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0, IsExported: true},
		{ID: pid + "::a.TestFixtures#Function", ProjectID: pid, FilePath: "lib/test-fixtures/x.go",
			Name: "TestFixtures", QualifiedName: "a.TestFixtures", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0, IsExported: true},
		{ID: pid + "::a.TestFixturesU#Function", ProjectID: pid, FilePath: "lib/test_fixtures/x.go",
			Name: "TestFixturesU", QualifiedName: "a.TestFixturesU", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0, IsExported: true},
		{ID: pid + "::a.PlainFixtures#Function", ProjectID: pid, FilePath: "lib/fixtures/x.go",
			Name: "PlainFixtures", QualifiedName: "a.PlainFixtures", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0, IsExported: true},
	})

	res, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql": `MATCH (n:Function) RETURN n.name, n.file_path`,
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, res)
	rows, _ := body["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected only the real-source row to survive; got %d rows: %v", len(rows), rows)
	}
	row := rows[0].(map[string]any)
	if got := row["n.file_path"]; got != "src/real.go" {
		t.Errorf("expected src/real.go to be the surviving row; got %v", got)
	}
}
