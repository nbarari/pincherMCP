package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// Tests for #247 #4: tests_to_run on changes. The tool should return
// test functions whose call graphs reach the changed symbols, sorted
// by overlap descending so the agent can pick the top entries first.

// setupChangesGitRepo wires a temp git repo with one committed file
// then mutates it so `git diff` produces output. Returns the repo dir.
func setupChangesGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := runCmd(t, dir, "git", "init"); err != nil {
		t.Skipf("git not available: %v (%s)", err, out)
	}
	runCmd(t, dir, "git", "config", "user.email", "test@test.com")
	runCmd(t, dir, "git", "config", "user.name", "Test")

	// Commit one file, then modify it so `git diff` returns content.
	target := filepath.Join(dir, "main.go")
	os.WriteFile(target, []byte("package main\nfunc Foo() {}\nfunc Bar() {}\n"), 0o644)
	runCmd(t, dir, "git", "add", ".")
	runCmd(t, dir, "git", "commit", "-m", "init")
	os.WriteFile(target, []byte("package main\nfunc Foo() { return }\nfunc Bar() { return }\n"), 0o644)
	return dir
}

// TestHandleChanges_TestsToRun_OrderedByOverlap is the core feature
// gate. Two changed functions (Foo, Bar). Three test functions:
//   - TestBoth covers Foo AND Bar (overlap=2)
//   - TestFoo covers only Foo (overlap=1)
//   - TestBar covers only Bar (overlap=1)
// The output must rank TestBoth first, then TestBar, then TestFoo
// (the IDs are the lex tiebreaker so TestBar < TestFoo by IDs alpha).
func TestHandleChanges_TestsToRun_OrderedByOverlap(t *testing.T) {
	srv, store, _ := newTestServer(t)
	repoDir := setupChangesGitRepo(t)
	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "tests-to-run", IndexedAt: time.Now()})
	srv.sessionID = repoDir
	srv.sessionRoot = repoDir

	// Seed the symbols: 2 production functions + 3 tests.
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p::main.Foo#Function", ProjectID: repoDir, FilePath: "main.go", Name: "Foo",
			QualifiedName: "main.Foo", Kind: "Function", Language: "Go",
			StartByte: 13, EndByte: 30, StartLine: 2, EndLine: 2, ExtractionConfidence: 1.0},
		{ID: "p::main.Bar#Function", ProjectID: repoDir, FilePath: "main.go", Name: "Bar",
			QualifiedName: "main.Bar", Kind: "Function", Language: "Go",
			StartByte: 31, EndByte: 48, StartLine: 3, EndLine: 3, ExtractionConfidence: 1.0},
		{ID: "p::main.TestBoth#Function", ProjectID: repoDir, FilePath: "main_test.go", Name: "TestBoth",
			QualifiedName: "main.TestBoth", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5, IsTest: true, ExtractionConfidence: 1.0},
		{ID: "p::main.TestFoo#Function", ProjectID: repoDir, FilePath: "main_test.go", Name: "TestFoo",
			QualifiedName: "main.TestFoo", Kind: "Function", Language: "Go",
			StartByte: 100, EndByte: 200, StartLine: 6, EndLine: 10, IsTest: true, ExtractionConfidence: 1.0},
		{ID: "p::main.TestBar#Function", ProjectID: repoDir, FilePath: "main_test.go", Name: "TestBar",
			QualifiedName: "main.TestBar", Kind: "Function", Language: "Go",
			StartByte: 200, EndByte: 300, StartLine: 11, EndLine: 15, IsTest: true, ExtractionConfidence: 1.0},
	})

	// Edges: tests CALL the production funcs.
	mustUpsertEdges(t, store, repoDir, []db.Edge{
		{FromID: "p::main.TestBoth#Function", ToID: "p::main.Foo#Function", Kind: "CALLS"},
		{FromID: "p::main.TestBoth#Function", ToID: "p::main.Bar#Function", Kind: "CALLS"},
		{FromID: "p::main.TestFoo#Function", ToID: "p::main.Foo#Function", Kind: "CALLS"},
		{FromID: "p::main.TestBar#Function", ToID: "p::main.Bar#Function", Kind: "CALLS"},
	})

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{"scope": "all"}))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	body := decode(t, result)
	tests, _ := body["tests_to_run"].([]any)
	if len(tests) != 3 {
		t.Fatalf("tests_to_run length = %d, want 3 (TestBoth + TestFoo + TestBar):\n%v", len(tests), body)
	}
	first, _ := tests[0].(map[string]any)
	if first["name"] != "TestBoth" {
		t.Errorf("first tests_to_run entry should be TestBoth (overlap 2); got %v", first["name"])
	}
	if overlap, _ := first["overlap"].(float64); overlap != 2 {
		t.Errorf("first overlap = %v, want 2", overlap)
	}
	// The remaining two tests both have overlap=1; the lex tie-break
	// orders TestBar before TestFoo since their IDs start with the
	// same prefix and "Bar" < "Foo".
	second, _ := tests[1].(map[string]any)
	third, _ := tests[2].(map[string]any)
	if second["name"] != "TestBar" || third["name"] != "TestFoo" {
		t.Errorf("tied-overlap entries not lex-ordered: 2nd=%v 3rd=%v (want TestBar then TestFoo)", second["name"], third["name"])
	}
}

// Summary count exposed alongside the array so consumers can show the
// count without parsing the array. Keeps the response shape consistent
// with the existing summary fields (changed_files, total_impacted, etc).
func TestHandleChanges_SummaryIncludesTestsToRunCount(t *testing.T) {
	srv, store, _ := newTestServer(t)
	repoDir := setupChangesGitRepo(t)
	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "summary-count", IndexedAt: time.Now()})
	srv.sessionID = repoDir
	srv.sessionRoot = repoDir

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p::main.Foo#Function", ProjectID: repoDir, FilePath: "main.go", Name: "Foo",
			QualifiedName: "main.Foo", Kind: "Function", Language: "Go",
			StartByte: 13, EndByte: 30, StartLine: 2, EndLine: 2, ExtractionConfidence: 1.0},
		{ID: "p::main.Bar#Function", ProjectID: repoDir, FilePath: "main.go", Name: "Bar",
			QualifiedName: "main.Bar", Kind: "Function", Language: "Go",
			StartByte: 31, EndByte: 48, StartLine: 3, EndLine: 3, ExtractionConfidence: 1.0},
		{ID: "p::main.TestFoo#Function", ProjectID: repoDir, FilePath: "main_test.go", Name: "TestFoo",
			QualifiedName: "main.TestFoo", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5, IsTest: true, ExtractionConfidence: 1.0},
	})
	mustUpsertEdges(t, store, repoDir, []db.Edge{
		{FromID: "p::main.TestFoo#Function", ToID: "p::main.Foo#Function", Kind: "CALLS"},
	})

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{"scope": "all"}))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	body := decode(t, result)
	summary, _ := body["summary"].(map[string]any)
	count, _ := summary["tests_to_run"].(float64)
	if count != 1 {
		t.Errorf("summary.tests_to_run = %v, want 1", count)
	}
}

// Changed code with no inbound test edges: tests_to_run is an empty
// (non-nil) array so JSON consumers don't have to handle null. This
// is the correct UX signal — "no test exercises this change; consider
// writing one" — but the existing next_steps logic already covers
// the recommendation; we just need the array to be safely consumable.
func TestHandleChanges_TestsToRun_EmptyWhenNoTestEdges(t *testing.T) {
	srv, store, _ := newTestServer(t)
	repoDir := setupChangesGitRepo(t)
	store.UpsertProject(db.Project{ID: repoDir, Path: repoDir, Name: "no-tests", IndexedAt: time.Now()})
	srv.sessionID = repoDir
	srv.sessionRoot = repoDir

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p::main.Foo#Function", ProjectID: repoDir, FilePath: "main.go", Name: "Foo",
			QualifiedName: "main.Foo", Kind: "Function", Language: "Go",
			StartByte: 13, EndByte: 30, StartLine: 2, EndLine: 2, ExtractionConfidence: 1.0},
	})
	// No edges → no callers → no tests reach Foo.

	result, err := srv.handleChanges(context.Background(), makeReq(map[string]any{"scope": "all"}))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	body := decode(t, result)
	tests, ok := body["tests_to_run"].([]any)
	if !ok {
		t.Fatalf("tests_to_run missing or wrong type:\n%v", body)
	}
	if len(tests) != 0 {
		t.Errorf("tests_to_run should be empty when no test reaches the change; got %v", tests)
	}
}

// mustUpsertEdges is local because no shared helper exists yet — the
// bulk-insert error is fatal since a broken fixture invalidates the
// whole test. Stamps ProjectID on each edge for caller convenience.
// (mustUpsertSymbols is already defined in project_scoping_test.go.)
func mustUpsertEdges(t *testing.T, store *db.Store, projectID string, edges []db.Edge) {
	t.Helper()
	for i := range edges {
		edges[i].ProjectID = projectID
	}
	if err := store.BulkUpsertEdges(edges); err != nil {
		t.Fatalf("BulkUpsertEdges: %v", err)
	}
}
