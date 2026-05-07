package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helper: in-memory store
// ─────────────────────────────────────────────────────────────────────────────

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testProject(id string) Project {
	return Project{
		ID:        id,
		Path:      "/tmp/" + id,
		Name:      id,
		IndexedAt: time.Now().Truncate(time.Second),
	}
}

func testSymbol(id, name, kind, projectID, filePath string) Symbol {
	return Symbol{
		ID:            id,
		ProjectID:     projectID,
		FilePath:      filePath,
		Name:          name,
		QualifiedName: name,
		Kind:          kind,
		Language:      "Go",
		StartByte:     0,
		EndByte:       100,
		StartLine:     1,
		EndLine:       10,
		IsExported:    true,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DataDir
// ─────────────────────────────────────────────────────────────────────────────

func TestDataDir(t *testing.T) {
	dir, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if dir == "" {
		t.Error("DataDir returned empty string")
	}
	// Should be a valid directory
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("DataDir %q does not exist: %v", dir, err)
	}
	if !info.IsDir() {
		t.Errorf("DataDir %q is not a directory", dir)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Open / migrate
// ─────────────────────────────────────────────────────────────────────────────

func TestOpen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if s.Path != filepath.Join(dir, "pincher.db") {
		t.Errorf("Path = %q, want %q", s.Path, filepath.Join(dir, "pincher.db"))
	}
}

// TestOpen_DSNPragmasApplied is the regression test for the DSN-syntax bug
// where modernc/sqlite-style `_pragma=name(value)` was written as the
// mattn-style `_name=value` and silently ignored. If WAL ever falls back
// to journal_mode=delete or busy_timeout to 0, queries serialize at the
// file level and contention cascades — this test fails loud.
func TestOpen_DSNPragmasApplied(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	checks := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"synchronous", "1"},   // NORMAL = 1
		{"foreign_keys", "1"},  // ON
		{"busy_timeout", "5000"},
		{"cache_size", "-65536"},
	}
	for _, c := range checks {
		var got string
		if err := s.db.QueryRow("PRAGMA " + c.pragma).Scan(&got); err != nil {
			t.Errorf("PRAGMA %s: %v", c.pragma, err)
			continue
		}
		if !strings.EqualFold(got, c.want) {
			t.Errorf("PRAGMA %s = %q, want %q", c.pragma, got, c.want)
		}
	}
}

func TestOpen_Idempotent(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	s1.Close()

	// Second open should succeed (migrate is idempotent)
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	s2.Close()
}

func TestMigrate_UpgradeFromV1(t *testing.T) {
	// Simulate a pre-versioning database that is at schema v1 (baseline only,
	// no extraction_confidence column, no symbol_moves table).
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pincher.db")

	// Build a v1-era database using a raw connection — apply the baseline schema
	// then pin schema_version to 1 (before any migrations ran).
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(schema); err != nil {
		raw.Close()
		t.Fatalf("baseline schema: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		raw.Close()
		t.Fatalf("create schema_version: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_version(version) VALUES(1)`); err != nil {
		raw.Close()
		t.Fatalf("seed schema_version: %v", err)
	}
	raw.Close()

	// Now open via the normal path — migrate() must detect v1 and apply
	// all pending migrations (v1→v2, v2→v3, …).
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after v1 seed: %v", err)
	}
	defer s.Close()

	// Verify the final version equals 1 + len(schemaMigrations).
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	want := 1 + len(schemaMigrations)
	if version != want {
		t.Errorf("version = %d, want %d", version, want)
	}

	// Seed a project so the symbol/symbol_moves inserts below satisfy FK
	// constraints. Pre-fix, foreign_keys=ON was silently ignored by the
	// (mis-formed) DSN so these inserts succeeded against an empty projects
	// table; now that the DSN is correct, FK is properly enforced.
	if _, err := s.db.Exec(`INSERT INTO projects(id,path,name,indexed_at) VALUES('p','/tmp/p','p',0)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Spot-check: extraction_confidence column must exist (migration 0).
	if _, err := s.db.Exec(`INSERT INTO symbols(id,project_id,file_path,name,qualified_name,kind,language,start_byte,end_byte,start_line,end_line,extraction_confidence) VALUES('x','p','f.go','X','X','func','Go',0,1,1,1,0.85)`); err != nil {
		t.Errorf("extraction_confidence column missing after migration: %v", err)
	}

	// Spot-check: symbol_moves table must exist (migration 1).
	if _, err := s.db.Exec(`INSERT INTO symbol_moves(old_id,new_id,project_id,moved_at) VALUES('old','new','p',0)`); err != nil {
		t.Errorf("symbol_moves table missing after migration: %v", err)
	}
}

func TestMigrate_VersionTracked(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	// Version must be 1 (baseline) + number of migrations applied.
	want := 1 + len(schemaMigrations)
	if version != want {
		t.Errorf("schema_version = %d, want %d", version, want)
	}
}

// TestMigrate_RejectsNewerDB verifies that an older binary refuses to open
// a database whose schema version is higher than any migration it knows
// about. Without this guard, the binary would silently proceed and could
// read/write rows using its older schema understanding.
func TestMigrate_RejectsNewerDB(t *testing.T) {
	dir := t.TempDir()

	// First Open: this binary creates the DB and migrates it to its current
	// max version.
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	// Simulate a future pincher version by bumping schema_version past
	// everything this binary knows about.
	futureVersion := 1 + len(schemaMigrations) + 1
	if _, err := s1.db.Exec(`UPDATE schema_version SET version = ?`, futureVersion); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	s1.Close()

	// Re-opening with the same binary must fail with a clear message. We
	// don't check the exact wording, just that the error mentions the
	// version mismatch so users know what to do.
	_, err = Open(dir)
	if err == nil {
		t.Fatal("Open should have refused a database at a newer schema version")
	}
	msg := err.Error()
	if !strings.Contains(msg, "newer than this binary") && !strings.Contains(msg, "upgrade pincher") {
		t.Errorf("error should point at the version mismatch, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Project CRUD
// ─────────────────────────────────────────────────────────────────────────────

func TestUpsertProject(t *testing.T) {
	s := newTestStore(t)
	p := testProject("proj1")
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	got, err := s.GetProject("proj1")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got == nil {
		t.Fatal("GetProject returned nil")
	}
	if got.Name != "proj1" {
		t.Errorf("Name = %q, want proj1", got.Name)
	}
}

func TestUpsertProject_Update(t *testing.T) {
	s := newTestStore(t)
	p := testProject("proj1")
	s.UpsertProject(p)

	p.FileCount = 42
	p.SymCount = 100
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject update: %v", err)
	}

	got, _ := s.GetProject("proj1")
	if got.FileCount != 42 {
		t.Errorf("FileCount = %d, want 42", got.FileCount)
	}
}

func TestGetProject_NotFound(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetProject("nonexistent")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got != nil {
		t.Error("expected nil for nonexistent project")
	}
}

func TestListProjects(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("a"))
	s.UpsertProject(testProject("b"))
	s.UpsertProject(testProject("c"))

	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 3 {
		t.Errorf("expected 3 projects, got %d", len(projects))
	}
}

func TestDeleteProject(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	sym := testSymbol("s1", "Foo", "Function", "p1", "foo.go")
	s.BulkUpsertSymbols([]Symbol{sym})

	if err := s.DeleteProject("p1"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	got, _ := s.GetProject("p1")
	if got != nil {
		t.Error("project should be deleted")
	}
	// Symbols should also be deleted
	fetched, _ := s.GetSymbol("s1")
	if fetched != nil {
		t.Error("symbols should be deleted with project")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Symbol CRUD
// ─────────────────────────────────────────────────────────────────────────────

func TestBulkUpsertSymbols(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("proj1"))

	syms := []Symbol{
		testSymbol("s1", "Foo", "Function", "proj1", "foo.go"),
		testSymbol("s2", "Bar", "Function", "proj1", "foo.go"),
		testSymbol("s3", "Baz", "Class", "proj1", "baz.go"),
	}
	if err := s.BulkUpsertSymbols(syms); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	got, err := s.GetSymbol("s1")
	if err != nil {
		t.Fatalf("GetSymbol: %v", err)
	}
	if got == nil {
		t.Fatal("symbol not found after upsert")
	}
	if got.Name != "Foo" {
		t.Errorf("Name = %q, want Foo", got.Name)
	}
}

func TestGetSymbol_NotFound(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetSymbol("nonexistent")
	if err != nil {
		t.Fatalf("GetSymbol: %v", err)
	}
	if got != nil {
		t.Error("expected nil for nonexistent symbol")
	}
}

func TestGetSymbolsByName(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("s1", "Process", "Function", "p1", "a.go"),
		testSymbol("s2", "Process", "Method", "p1", "b.go"),
		testSymbol("s3", "Other", "Function", "p1", "c.go"),
	})

	results, err := s.GetSymbolsByName("p1", "Process", 10)
	if err != nil {
		t.Fatalf("GetSymbolsByName: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
}

func TestGetSymbolsForFile(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("s1", "A", "Function", "p1", "target.go"),
		testSymbol("s2", "B", "Function", "p1", "target.go"),
		testSymbol("s3", "C", "Function", "p1", "other.go"),
	})

	results, err := s.GetSymbolsForFile("p1", "target.go")
	if err != nil {
		t.Fatalf("GetSymbolsForFile: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 symbols in target.go, got %d", len(results))
	}
}

func TestDeleteSymbolsForFile(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("s1", "A", "Function", "p1", "target.go"),
		testSymbol("s2", "B", "Function", "p1", "other.go"),
	})
	s.BulkUpsertEdges([]Edge{{ProjectID: "p1", FromID: "s1", ToID: "s2", Kind: "CALLS", Confidence: 1.0}})

	if err := s.DeleteSymbolsForFile("p1", "target.go"); err != nil {
		t.Fatalf("DeleteSymbolsForFile: %v", err)
	}

	got, _ := s.GetSymbol("s1")
	if got != nil {
		t.Error("s1 should be deleted")
	}
	got, _ = s.GetSymbol("s2")
	if got == nil {
		t.Error("s2 in other.go should survive")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// FTS5 search
// ─────────────────────────────────────────────────────────────────────────────

func TestSearchSymbols(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	syms := []Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "auth.go", Name: "AuthService",
			QualifiedName: "auth.AuthService", Kind: "Class", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 20},
		{ID: "s2", ProjectID: "p1", FilePath: "user.go", Name: "UserService",
			QualifiedName: "user.UserService", Kind: "Class", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 20},
		{ID: "s3", ProjectID: "p1", FilePath: "auth.go", Name: "Login",
			QualifiedName: "auth.Login", Kind: "Function", Language: "Go",
			StartByte: 200, EndByte: 300, StartLine: 30, EndLine: 45},
	}
	s.BulkUpsertSymbols(syms)

	results, err := s.SearchSymbols("p1", "auth*", "", "", 10)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected results for 'auth*'")
	}
}

func TestSearchSymbols_KindFilter(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		{ID: "f1", ProjectID: "p1", FilePath: "a.go", Name: "processOrder",
			QualifiedName: "pkg.processOrder", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5},
		{ID: "c1", ProjectID: "p1", FilePath: "a.go", Name: "processOrder",
			QualifiedName: "pkg.OrderProcessor", Kind: "Class", Language: "Go",
			StartByte: 60, EndByte: 200, StartLine: 10, EndLine: 30},
	})

	results, err := s.SearchSymbols("p1", "process*", "Function", "", 10)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	for _, r := range results {
		if r.Symbol.Kind != "Function" {
			t.Errorf("kind filter failed: got %q", r.Symbol.Kind)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Edge operations
// ─────────────────────────────────────────────────────────────────────────────

func TestBulkUpsertEdges(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("a", "A", "Function", "p1", "a.go"),
		testSymbol("b", "B", "Function", "p1", "b.go"),
	})

	edges := []Edge{
		{ProjectID: "p1", FromID: "a", ToID: "b", Kind: "CALLS", Confidence: 1.0},
	}
	if err := s.BulkUpsertEdges(edges); err != nil {
		t.Fatalf("BulkUpsertEdges: %v", err)
	}

	from, err := s.EdgesFrom("a", nil)
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(from) != 1 {
		t.Errorf("expected 1 edge from a, got %d", len(from))
	}

	to, err := s.EdgesTo("b", nil)
	if err != nil {
		t.Fatalf("EdgesTo: %v", err)
	}
	if len(to) != 1 {
		t.Errorf("expected 1 edge to b, got %d", len(to))
	}
}

func TestEdgesFrom_KindFilter(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("a", "A", "Function", "p1", "a.go"),
		testSymbol("b", "B", "Function", "p1", "b.go"),
		testSymbol("c", "C", "Class", "p1", "c.go"),
	})
	s.BulkUpsertEdges([]Edge{
		{ProjectID: "p1", FromID: "a", ToID: "b", Kind: "CALLS", Confidence: 1.0},
		{ProjectID: "p1", FromID: "a", ToID: "c", Kind: "IMPORTS", Confidence: 1.0},
	})

	calls, _ := s.EdgesFrom("a", []string{"CALLS"})
	if len(calls) != 1 {
		t.Errorf("expected 1 CALLS edge, got %d", len(calls))
	}

	all, _ := s.EdgesFrom("a", nil)
	if len(all) != 2 {
		t.Errorf("expected 2 total edges, got %d", len(all))
	}
}

func TestBulkUpsertEdges_Idempotent(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("a", "A", "Function", "p1", "a.go"),
		testSymbol("b", "B", "Function", "p1", "b.go"),
	})

	edge := []Edge{{ProjectID: "p1", FromID: "a", ToID: "b", Kind: "CALLS", Confidence: 1.0}}
	s.BulkUpsertEdges(edge)
	s.BulkUpsertEdges(edge) // second insert should be ignored

	from, _ := s.EdgesFrom("a", nil)
	if len(from) != 1 {
		t.Errorf("duplicate insert should be ignored, got %d edges", len(from))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// File hash operations
// ─────────────────────────────────────────────────────────────────────────────

func TestFileHash(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	// Initially no hash
	h := s.GetFileHash("p1", "file.go")
	if h != "" {
		t.Errorf("expected empty hash, got %q", h)
	}

	if err := s.SetFileHash("p1", "file.go", "abc123"); err != nil {
		t.Fatalf("SetFileHash: %v", err)
	}

	h = s.GetFileHash("p1", "file.go")
	if h != "abc123" {
		t.Errorf("GetFileHash = %q, want abc123", h)
	}
}

func TestDeleteFileHash(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.SetFileHash("p1", "file.go", "abc123")

	if err := s.DeleteFileHash("p1", "file.go"); err != nil {
		t.Fatalf("DeleteFileHash: %v", err)
	}

	h := s.GetFileHash("p1", "file.go")
	if h != "" {
		t.Errorf("expected empty hash after delete, got %q", h)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ADR operations
// ─────────────────────────────────────────────────────────────────────────────

func TestADR_SetGet(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	if err := s.SetADR("p1", "STACK", "Go + SQLite"); err != nil {
		t.Fatalf("SetADR: %v", err)
	}

	val, ok, err := s.GetADR("p1", "STACK")
	if err != nil {
		t.Fatalf("GetADR: %v", err)
	}
	if !ok {
		t.Error("expected ADR to exist")
	}
	if val != "Go + SQLite" {
		t.Errorf("value = %q, want 'Go + SQLite'", val)
	}
}

func TestADR_NotFound(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	_, ok, err := s.GetADR("p1", "NONEXISTENT")
	if err != nil {
		t.Fatalf("GetADR: %v", err)
	}
	if ok {
		t.Error("expected ADR not to exist")
	}
}

func TestADR_List(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.SetADR("p1", "A", "val-a")
	s.SetADR("p1", "B", "val-b")

	entries, err := s.ListADRs("p1")
	if err != nil {
		t.Fatalf("ListADRs: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 ADR entries, got %d", len(entries))
	}
	if entries["A"] != "val-a" || entries["B"] != "val-b" {
		t.Errorf("unexpected entries: %v", entries)
	}
}

func TestADR_Delete(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.SetADR("p1", "KEY", "value")

	if err := s.DeleteADR("p1", "KEY"); err != nil {
		t.Fatalf("DeleteADR: %v", err)
	}

	_, ok, _ := s.GetADR("p1", "KEY")
	if ok {
		t.Error("ADR should be deleted")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Graph stats
// ─────────────────────────────────────────────────────────────────────────────

func TestGraphStats(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("f1", "Foo", "Function", "p1", "a.go"),
		testSymbol("f2", "Bar", "Function", "p1", "a.go"),
		testSymbol("c1", "MyClass", "Class", "p1", "b.go"),
	})
	s.BulkUpsertEdges([]Edge{
		{ProjectID: "p1", FromID: "f1", ToID: "f2", Kind: "CALLS", Confidence: 1.0},
	})

	symCount, edgeCount, kindCounts, edgeKindCounts, err := s.GraphStats("p1")
	if err != nil {
		t.Fatalf("GraphStats: %v", err)
	}
	if symCount != 3 {
		t.Errorf("symCount = %d, want 3", symCount)
	}
	if edgeCount != 1 {
		t.Errorf("edgeCount = %d, want 1", edgeCount)
	}
	if kindCounts["Function"] != 2 {
		t.Errorf("Function count = %d, want 2", kindCounts["Function"])
	}
	if edgeKindCounts["CALLS"] != 1 {
		t.Errorf("CALLS edge count = %d, want 1", edgeKindCounts["CALLS"])
	}
}

func TestGetHotspots(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("a", "A", "Function", "p1", "a.go"),
		testSymbol("b", "B", "Function", "p1", "b.go"),
		testSymbol("c", "C", "Function", "p1", "c.go"),
	})
	// B is called by A and C → hotspot
	s.BulkUpsertEdges([]Edge{
		{ProjectID: "p1", FromID: "a", ToID: "b", Kind: "CALLS", Confidence: 1.0},
		{ProjectID: "p1", FromID: "c", ToID: "b", Kind: "CALLS", Confidence: 1.0},
	})

	hotspots, err := s.GetHotspots("p1", 5)
	if err != nil {
		t.Fatalf("GetHotspots: %v", err)
	}
	if len(hotspots) == 0 {
		t.Error("expected at least 1 hotspot")
	}
	if hotspots[0].Name != "B" {
		t.Errorf("top hotspot = %q, want B", hotspots[0].Name)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Utility functions
// ─────────────────────────────────────────────────────────────────────────────

func TestMakeSymbolID(t *testing.T) {
	id := MakeSymbolID("internal/db/db.go", "db.Open", "Function")
	want := "internal/db/db.go::db.Open#Function"
	if id != want {
		t.Errorf("MakeSymbolID = %q, want %q", id, want)
	}
}

func TestProjectNameFromPath(t *testing.T) {
	cases := []struct {
		path, want string
	}{
		{"/home/user/myproject", "myproject"},
		{"/home/user/myproject/", "myproject"},
		{"C:\\Users\\foo\\bar", "bar"},
	}
	for _, c := range cases {
		got := ProjectNameFromPath(c.path)
		if got != c.want {
			t.Errorf("ProjectNameFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestApproxTokens(t *testing.T) {
	// Counts verified against cl100k_base BPE (same tokenizer family as Claude).
	cases := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"abcd", 1},        // single BPE token
		{"abcde", 2},       // splits at boundary
		{"abcdefgh", 1},    // BPE merges entire sequence
		{"hello world", 2}, // ["hello", " world"]
	}
	for _, c := range cases {
		got := ApproxTokens(c.s)
		if got != c.want {
			t.Errorf("ApproxTokens(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	cases := []struct {
		bytes int
		want  string
	}{
		{500, "500 B"},
		{1500, "1.5 KB"},
		{2000000, "1.9 MB"},
	}
	for _, c := range cases {
		got := FormatSize(c.bytes)
		if got != c.want {
			t.Errorf("FormatSize(%d) = %q, want %q", c.bytes, got, c.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DB accessor
// ─────────────────────────────────────────────────────────────────────────────

func TestDB_Accessor(t *testing.T) {
	store := newTestStore(t)
	if store.DB() == nil {
		t.Error("DB() should return non-nil *sql.DB")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetSymbolsByQN
// ─────────────────────────────────────────────────────────────────────────────

func TestGetSymbolsByQN(t *testing.T) {
	store := newTestStore(t)
	pid := "proj-qn"
	store.UpsertProject(Project{ID: pid, Path: "/tmp/qn", Name: "qn"})
	store.BulkUpsertSymbols([]Symbol{
		{ID: "qn1", ProjectID: pid, FilePath: "a.go", Name: "Foo",
			QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5},
	})
	results, err := store.GetSymbolsByQN(pid, "pkg.Foo")
	if err != nil {
		t.Fatalf("GetSymbolsByQN: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected to find symbol by qualified name")
	}
}

func TestGetSymbolsByQN_NotFound(t *testing.T) {
	store := newTestStore(t)
	pid := "proj-qn2"
	store.UpsertProject(Project{ID: pid, Path: "/tmp/qn2", Name: "qn2"})
	results, err := store.GetSymbolsByQN(pid, "pkg.NonExistent")
	if err != nil {
		t.Fatalf("GetSymbolsByQN: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ProjectIDFromPath
// ─────────────────────────────────────────────────────────────────────────────

func TestProjectIDFromPath(t *testing.T) {
	id := ProjectIDFromPath("/home/user/myproject")
	if id == "" {
		t.Error("ProjectIDFromPath returned empty string")
	}
	id2 := ProjectIDFromPath("/home/user/myproject")
	if id != id2 {
		t.Error("ProjectIDFromPath should be deterministic")
	}
	id3 := ProjectIDFromPath("/home/user/otherproject")
	if id == id3 {
		t.Error("different paths should give different IDs")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DataDir
// ─────────────────────────────────────────────────────────────────────────────

func TestDataDir_ReturnsPath(t *testing.T) {
	dir, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if dir == "" {
		t.Error("DataDir returned empty string")
	}
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		t.Errorf("DataDir %q does not exist after call", dir)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BulkUpsertEdges
// ─────────────────────────────────────────────────────────────────────────────

func TestBulkUpsertEdges_WithProperties(t *testing.T) {
	store := newTestStore(t)
	pid := "proj-edge-props"
	store.UpsertProject(testProject(pid))
	store.BulkUpsertSymbols([]Symbol{
		{ID: "ep1", ProjectID: pid, FilePath: "a.go", Name: "A", QualifiedName: "pkg.A", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2},
		{ID: "ep2", ProjectID: pid, FilePath: "b.go", Name: "B", QualifiedName: "pkg.B", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2},
	})
	edges := []Edge{
		{ProjectID: pid, FromID: "ep1", ToID: "ep2", Kind: "CALLS", Confidence: 0.9,
			Properties: map[string]any{"line": 5}},
	}
	if err := store.BulkUpsertEdges(edges); err != nil {
		t.Fatalf("BulkUpsertEdges with properties: %v", err)
	}
	got, err := store.EdgesFrom("ep1", []string{"CALLS"})
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(got))
	}
}

func TestBulkUpsertEdges_Empty(t *testing.T) {
	store := newTestStore(t)
	if err := store.BulkUpsertEdges(nil); err != nil {
		t.Fatalf("BulkUpsertEdges(nil): %v", err)
	}
	if err := store.BulkUpsertEdges([]Edge{}); err != nil {
		t.Fatalf("BulkUpsertEdges([]): %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteProject
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteProject_RemovesAll(t *testing.T) {
	store := newTestStore(t)
	pid := "proj-del"
	store.UpsertProject(testProject(pid))
	store.BulkUpsertSymbols([]Symbol{
		{ID: "dp1", ProjectID: pid, FilePath: "a.go", Name: "A", QualifiedName: "pkg.A", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2},
		{ID: "dp2", ProjectID: pid, FilePath: "b.go", Name: "B", QualifiedName: "pkg.B", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2},
	})
	store.BulkUpsertEdges([]Edge{
		{ProjectID: pid, FromID: "dp1", ToID: "dp2", Kind: "CALLS", Confidence: 1.0},
	})
	if err := store.DeleteProject(pid); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	p, err := store.GetProject(pid)
	if err != nil {
		t.Fatalf("GetProject after delete: %v", err)
	}
	if p != nil {
		t.Error("project should be nil after deletion")
	}
	syms, _ := store.GetSymbolsForFile(pid, "a.go")
	if len(syms) != 0 {
		t.Errorf("expected 0 symbols after deletion, got %d", len(syms))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteSymbolsForFile
// ─────────────────────────────────────────────────────────────────────────────

// ─────────────────────────────────────────────────────────────────────────────
// GraphStats
// ─────────────────────────────────────────────────────────────────────────────

func TestGraphStats_WithData(t *testing.T) {
	store := newTestStore(t)
	pid := "proj-gs"
	store.UpsertProject(testProject(pid))
	store.BulkUpsertSymbols([]Symbol{
		{ID: "gs1", ProjectID: pid, FilePath: "a.go", Name: "Fn1", QualifiedName: "p.Fn1", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2},
		{ID: "gs2", ProjectID: pid, FilePath: "a.go", Name: "T1", QualifiedName: "p.T1", Kind: "Class", Language: "Go", StartByte: 20, EndByte: 50, StartLine: 5, EndLine: 10},
		{ID: "gs3", ProjectID: pid, FilePath: "b.go", Name: "Fn2", QualifiedName: "p.Fn2", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2},
	})
	store.BulkUpsertEdges([]Edge{
		{ProjectID: pid, FromID: "gs1", ToID: "gs3", Kind: "CALLS", Confidence: 1.0},
		{ProjectID: pid, FromID: "gs2", ToID: "gs1", Kind: "CALLS", Confidence: 1.0},
	})
	symCount, edgeCount, kindCounts, edgeKindCounts, err := store.GraphStats(pid)
	if err != nil {
		t.Fatalf("GraphStats: %v", err)
	}
	if symCount != 3 {
		t.Errorf("expected 3 symbols, got %d", symCount)
	}
	if edgeCount != 2 {
		t.Errorf("expected 2 edges, got %d", edgeCount)
	}
	if kindCounts["Function"] != 2 {
		t.Errorf("expected 2 Function kinds, got %d", kindCounts["Function"])
	}
	if kindCounts["Class"] != 1 {
		t.Errorf("expected 1 Class kind, got %d", kindCounts["Class"])
	}
	if edgeKindCounts["CALLS"] != 2 {
		t.Errorf("expected 2 CALLS edges, got %d", edgeKindCounts["CALLS"])
	}
}

func TestGraphStats_EmptyProject(t *testing.T) {
	store := newTestStore(t)
	pid := "proj-gs-empty"
	store.UpsertProject(testProject(pid))
	symCount, edgeCount, kindCounts, edgeKindCounts, err := store.GraphStats(pid)
	if err != nil {
		t.Fatalf("GraphStats: %v", err)
	}
	if symCount != 0 || edgeCount != 0 {
		t.Errorf("expected 0 counts, got sym=%d edge=%d", symCount, edgeCount)
	}
	if len(kindCounts) != 0 || len(edgeKindCounts) != 0 {
		t.Error("expected empty kind maps for empty project")
	}
}


// ─────────────────────────────────────────────────────────────────────────────
// TraceViaCTE
// ─────────────────────────────────────────────────────────────────────────────

func TestTraceViaCTE_Outbound(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	caller := testSymbol("s1", "Caller", "Function", "p1", "a.go")
	callee := testSymbol("s2", "Callee", "Function", "p1", "b.go")
	s.BulkUpsertSymbols([]Symbol{caller, callee})
	s.BulkUpsertEdges([]Edge{{ProjectID: "p1", FromID: "s1", ToID: "s2", Kind: "CALLS", Confidence: 1.0}})

	results, err := s.TraceViaCTE("s1", "outbound", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE: %v", err)
	}
	if len(results) != 1 || results[0].SymbolID != "s2" {
		t.Errorf("expected [s2], got %v", results)
	}
	if results[0].Depth != 1 {
		t.Errorf("expected depth 1, got %d", results[0].Depth)
	}
	if results[0].ViaKind != "CALLS" {
		t.Errorf("expected via CALLS, got %q", results[0].ViaKind)
	}
}

func TestTraceViaCTE_Inbound(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	caller := testSymbol("s1", "Caller", "Function", "p1", "a.go")
	callee := testSymbol("s2", "Callee", "Function", "p1", "b.go")
	s.BulkUpsertSymbols([]Symbol{caller, callee})
	s.BulkUpsertEdges([]Edge{{ProjectID: "p1", FromID: "s1", ToID: "s2", Kind: "CALLS", Confidence: 1.0}})

	results, err := s.TraceViaCTE("s2", "inbound", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE: %v", err)
	}
	if len(results) != 1 || results[0].SymbolID != "s1" {
		t.Errorf("expected [s1], got %v", results)
	}
}

func TestTraceViaCTE_Both(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	root := testSymbol("root", "Root", "Function", "p1", "root.go")
	caller := testSymbol("caller", "Caller", "Function", "p1", "caller.go")
	callee := testSymbol("callee", "Callee", "Function", "p1", "callee.go")
	s.BulkUpsertSymbols([]Symbol{root, caller, callee})
	s.BulkUpsertEdges([]Edge{
		{ProjectID: "p1", FromID: "caller", ToID: "root", Kind: "CALLS", Confidence: 1.0},
		{ProjectID: "p1", FromID: "root", ToID: "callee", Kind: "CALLS", Confidence: 1.0},
	})

	results, err := s.TraceViaCTE("root", "both", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range results {
		ids[r.SymbolID] = true
	}
	if !ids["caller"] {
		t.Error("expected caller in both-direction trace")
	}
	if !ids["callee"] {
		t.Error("expected callee in both-direction trace")
	}
}

func TestTraceViaCTE_MultiHop(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	a := testSymbol("a", "A", "Function", "p1", "a.go")
	b := testSymbol("b", "B", "Function", "p1", "b.go")
	c := testSymbol("c", "C", "Function", "p1", "c.go")
	s.BulkUpsertSymbols([]Symbol{a, b, c})
	s.BulkUpsertEdges([]Edge{
		{ProjectID: "p1", FromID: "a", ToID: "b", Kind: "CALLS", Confidence: 1.0},
		{ProjectID: "p1", FromID: "b", ToID: "c", Kind: "CALLS", Confidence: 1.0},
	})

	results, err := s.TraceViaCTE("a", "outbound", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE: %v", err)
	}
	ids := map[string]int{}
	for _, r := range results {
		ids[r.SymbolID] = r.Depth
	}
	if ids["b"] != 1 {
		t.Errorf("expected B at depth 1, got %d", ids["b"])
	}
	if ids["c"] != 2 {
		t.Errorf("expected C at depth 2, got %d", ids["c"])
	}
}

func TestTraceViaCTE_DepthLimit(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	a := testSymbol("a", "A", "Function", "p1", "a.go")
	b := testSymbol("b", "B", "Function", "p1", "b.go")
	c := testSymbol("c", "C", "Function", "p1", "c.go")
	s.BulkUpsertSymbols([]Symbol{a, b, c})
	s.BulkUpsertEdges([]Edge{
		{ProjectID: "p1", FromID: "a", ToID: "b", Kind: "CALLS", Confidence: 1.0},
		{ProjectID: "p1", FromID: "b", ToID: "c", Kind: "CALLS", Confidence: 1.0},
	})

	// maxDepth=1 should only find b, not c
	results, err := s.TraceViaCTE("a", "outbound", []string{"CALLS"}, 1)
	if err != nil {
		t.Fatalf("TraceViaCTE: %v", err)
	}
	for _, r := range results {
		if r.SymbolID == "c" {
			t.Error("c should be out of reach at maxDepth=1")
		}
	}
}

func TestTraceViaCTE_NoEdges(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	sym := testSymbol("iso", "Isolated", "Function", "p1", "iso.go")
	s.BulkUpsertSymbols([]Symbol{sym})

	results, err := s.TraceViaCTE("iso", "both", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results for isolated node, got %d", len(results))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Symbol move tracking
// ─────────────────────────────────────────────────────────────────────────────

func TestRecordSymbolMove_Basic(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	if err := s.RecordSymbolMove("p1", "old-id", "new-id"); err != nil {
		t.Fatalf("RecordSymbolMove: %v", err)
	}
	newID, ok := s.ResolveStaleID("p1", "old-id")
	if !ok {
		t.Fatal("expected stale ID to resolve")
	}
	if newID != "new-id" {
		t.Errorf("expected new-id, got %q", newID)
	}
}

func TestResolveStaleID_NotFound(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	_, ok := s.ResolveStaleID("p1", "nonexistent")
	if ok {
		t.Error("expected false for nonexistent stale ID")
	}
}

func TestRecordSymbolMove_Upsert(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.RecordSymbolMove("p1", "old-id", "new-id-1")
	// Second call should update new_id
	if err := s.RecordSymbolMove("p1", "old-id", "new-id-2"); err != nil {
		t.Fatalf("RecordSymbolMove upsert: %v", err)
	}
	newID, _ := s.ResolveStaleID("p1", "old-id")
	if newID != "new-id-2" {
		t.Errorf("expected updated new-id-2, got %q", newID)
	}
}

func TestDetectAndRecordMoves_DetectsMove(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	// Original symbol at old path
	old := testSymbol("old/path.go::MyFn#Function", "MyFn", "Function", "p1", "old/path.go")
	old.QualifiedName = "MyFn"
	s.BulkUpsertSymbols([]Symbol{old})

	// Same qualified name + kind, new path (file moved)
	newSym := testSymbol("new/path.go::MyFn#Function", "MyFn", "Function", "p1", "new/path.go")
	newSym.QualifiedName = "MyFn"

	if err := s.DetectAndRecordMoves("p1", []Symbol{newSym}); err != nil {
		t.Fatalf("DetectAndRecordMoves: %v", err)
	}

	newID, ok := s.ResolveStaleID("p1", old.ID)
	if !ok {
		t.Fatal("expected move to be recorded")
	}
	if newID != newSym.ID {
		t.Errorf("expected %q, got %q", newSym.ID, newID)
	}
}

func TestDetectAndRecordMoves_NoMove(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	// Brand new symbol — no existing symbol with same QN+kind
	newSym := testSymbol("path.go::BrandNew#Function", "BrandNew", "Function", "p1", "path.go")
	newSym.QualifiedName = "BrandNew"

	if err := s.DetectAndRecordMoves("p1", []Symbol{newSym}); err != nil {
		t.Fatalf("DetectAndRecordMoves: %v", err)
	}
	_, ok := s.ResolveStaleID("p1", newSym.ID)
	if ok {
		t.Error("no move should be recorded for brand-new symbol")
	}
}

func TestDetectAndRecordMoves_Empty(t *testing.T) {
	s := newTestStore(t)
	if err := s.DetectAndRecordMoves("p1", nil); err != nil {
		t.Fatalf("DetectAndRecordMoves empty: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteSymbolsForFile
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteSymbolsForFile_RemovesSymbolsAndEdges(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("dsf"))

	a := testSymbol("dsf::A#Function", "A", "Function", "dsf", "a.go")
	b := testSymbol("dsf::B#Function", "B", "Function", "dsf", "b.go")
	if err := s.BulkUpsertSymbols([]Symbol{a, b}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.BulkUpsertEdges([]Edge{
		{ProjectID: "dsf", FromID: a.ID, ToID: b.ID, Kind: "CALLS"},
	}); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	if err := s.DeleteSymbolsForFile("dsf", "a.go"); err != nil {
		t.Fatalf("DeleteSymbolsForFile: %v", err)
	}

	// Symbol A should be gone
	got, err := s.GetSymbol(a.ID)
	if err != nil {
		t.Fatalf("GetSymbol: %v", err)
	}
	if got != nil {
		t.Error("symbol A still exists after DeleteSymbolsForFile")
	}

	// Symbol B should still exist (different file)
	gotB, err := s.GetSymbol(b.ID)
	if err != nil {
		t.Fatalf("GetSymbol B: %v", err)
	}
	if gotB == nil {
		t.Error("symbol B should still exist")
	}

	// Edge from A should be gone
	var edgeCount int
	s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE from_id=? OR to_id=?`, a.ID, a.ID).Scan(&edgeCount)
	if edgeCount != 0 {
		t.Errorf("edges referencing deleted symbol still exist: count=%d", edgeCount)
	}
}

func TestDeleteSymbolsForFile_EmptyFile(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("dsf2"))
	// Deleting symbols for a file that has no symbols should not error.
	if err := s.DeleteSymbolsForFile("dsf2", "nonexistent.go"); err != nil {
		t.Errorf("DeleteSymbolsForFile nonexistent: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GraphStats
// ─────────────────────────────────────────────────────────────────────────────

func TestGraphStats_Kinds(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("gs"))

	fn := testSymbol("gs::Fn#Function", "Fn", "Function", "gs", "f.go")
	cl := testSymbol("gs::Cls#Class", "Cls", "Class", "gs", "f.go")
	cl.Kind = "Class"
	if err := s.BulkUpsertSymbols([]Symbol{fn, cl}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.BulkUpsertEdges([]Edge{
		{ProjectID: "gs", FromID: fn.ID, ToID: cl.ID, Kind: "CALLS"},
	}); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	symCount, edgeCount, kindCounts, edgeKindCounts, err := s.GraphStats("gs")
	if err != nil {
		t.Fatalf("GraphStats: %v", err)
	}
	if symCount != 2 {
		t.Errorf("symCount=%d, want 2", symCount)
	}
	if edgeCount != 1 {
		t.Errorf("edgeCount=%d, want 1", edgeCount)
	}
	if kindCounts["Function"] != 1 {
		t.Errorf("Function kind count=%d, want 1", kindCounts["Function"])
	}
	if kindCounts["Class"] != 1 {
		t.Errorf("Class kind count=%d, want 1", kindCounts["Class"])
	}
	if edgeKindCounts["CALLS"] != 1 {
		t.Errorf("CALLS edge count=%d, want 1", edgeKindCounts["CALLS"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SearchSymbols — kind and language filters
// ─────────────────────────────────────────────────────────────────────────────

func TestSearchSymbols_KindFilterCombined(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("ssk"))

	fn := testSymbol("ssk::ProcessOrder#Function", "ProcessOrder", "Function", "ssk", "f.go")
	cl := testSymbol("ssk::OrderService#Class", "OrderService", "Class", "ssk", "f.go")
	cl.Kind = "Class"
	cl.Name = "OrderService"
	cl.QualifiedName = "OrderService"

	if err := s.BulkUpsertSymbols([]Symbol{fn, cl}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Search with kind=Function should only return Function kinds
	results, err := s.SearchSymbols("ssk", "Order*", "Function", "", 10)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	for _, r := range results {
		if r.Symbol.Kind != "Function" {
			t.Errorf("kind filter violated: got kind=%q", r.Symbol.Kind)
		}
	}

	// Search with kind=Class should only return Class kinds
	results, err = s.SearchSymbols("ssk", "Order*", "Class", "", 10)
	if err != nil {
		t.Fatalf("SearchSymbols Class: %v", err)
	}
	for _, r := range results {
		if r.Symbol.Kind != "Class" {
			t.Errorf("Class filter violated: got kind=%q", r.Symbol.Kind)
		}
	}
}

func TestSearchSymbols_LanguageFilter(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("ssl"))

	goSym := testSymbol("ssl::Handler#Function", "Handler", "Function", "ssl", "f.go")
	goSym.Language = "Go"

	pySym := testSymbol("ssl::handler#Function", "handler", "Function", "ssl", "f.py")
	pySym.Language = "Python"
	pySym.Name = "handler"
	pySym.QualifiedName = "handler"

	if err := s.BulkUpsertSymbols([]Symbol{goSym, pySym}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	results, err := s.SearchSymbols("ssl", "handler*", "", "Go", 10)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	for _, r := range results {
		if r.Symbol.Language != "Go" {
			t.Errorf("language filter violated: got language=%q", r.Symbol.Language)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetHotspots
// ─────────────────────────────────────────────────────────────────────────────

func TestGetHotspots_TopCalled(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("hot2"))

	caller := testSymbol("hot2::Caller#Function", "Caller", "Function", "hot2", "f.go")
	callee := testSymbol("hot2::Callee#Function", "Callee", "Function", "hot2", "f.go")
	if err := s.BulkUpsertSymbols([]Symbol{caller, callee}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// callee is called twice
	if err := s.BulkUpsertEdges([]Edge{
		{ProjectID: "hot2", FromID: caller.ID, ToID: callee.ID, Kind: "CALLS"},
		{ProjectID: "hot2", FromID: callee.ID, ToID: callee.ID, Kind: "CALLS"}, // self-call
	}); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	hotspots, err := s.GetHotspots("hot2", 5)
	if err != nil {
		t.Fatalf("GetHotspots: %v", err)
	}
	if len(hotspots) == 0 {
		t.Fatal("expected at least one hotspot")
	}
	// Callee has 2 in-edges, so it should be top hotspot
	if hotspots[0].Name != "Callee" {
		t.Errorf("top hotspot=%q, want Callee", hotspots[0].Name)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TraceViaCTE — direction variants
// ─────────────────────────────────────────────────────────────────────────────

func TestTraceViaCTE_InboundDirection(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("tr"))

	caller := testSymbol("tr::Caller#Function", "Caller", "Function", "tr", "f.go")
	callee := testSymbol("tr::Callee#Function", "Callee", "Function", "tr", "f.go")
	if err := s.BulkUpsertSymbols([]Symbol{caller, callee}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.BulkUpsertEdges([]Edge{
		{ProjectID: "tr", FromID: caller.ID, ToID: callee.ID, Kind: "CALLS"},
	}); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	// Inbound trace from callee — should find caller
	results, err := s.TraceViaCTE(callee.ID, "inbound", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE inbound: %v", err)
	}
	found := false
	for _, r := range results {
		if r.SymbolID == caller.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("inbound trace from Callee should find Caller, got %v", results)
	}
}

func TestTraceViaCTE_BothDirections(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("trb"))

	a := testSymbol("trb::A#Function", "A", "Function", "trb", "f.go")
	b := testSymbol("trb::B#Function", "B", "Function", "trb", "f.go")
	c := testSymbol("trb::C#Function", "C", "Function", "trb", "f.go")
	if err := s.BulkUpsertSymbols([]Symbol{a, b, c}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.BulkUpsertEdges([]Edge{
		{ProjectID: "trb", FromID: a.ID, ToID: b.ID, Kind: "CALLS"}, // A calls B
		{ProjectID: "trb", FromID: c.ID, ToID: b.ID, Kind: "CALLS"}, // C calls B
	}); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	// Both directions from B — outbound: nothing, inbound: A and C
	results, err := s.TraceViaCTE(b.ID, "both", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE both: %v", err)
	}
	ids := make(map[string]bool)
	for _, r := range results {
		ids[r.SymbolID] = true
	}
	if !ids[a.ID] {
		t.Error("both trace should find A (inbound caller)")
	}
	if !ids[c.ID] {
		t.Error("both trace should find C (inbound caller)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BulkUpsertSymbols — ExtractionConfidence default
// ─────────────────────────────────────────────────────────────────────────────

func TestBulkUpsertSymbols_DefaultConfidence(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("buc"))

	sym := testSymbol("buc::Fn#Function", "Fn", "Function", "buc", "f.go")
	sym.ExtractionConfidence = 0 // should default to 1.0

	if err := s.BulkUpsertSymbols([]Symbol{sym}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	got, err := s.GetSymbol(sym.ID)
	if err != nil {
		t.Fatalf("GetSymbol: %v", err)
	}
	if got == nil {
		t.Fatal("symbol not found")
	}
	if got.ExtractionConfidence != 1.0 {
		t.Errorf("ExtractionConfidence=%f, want 1.0 (default)", got.ExtractionConfidence)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ListADRs — multiple entries
// ─────────────────────────────────────────────────────────────────────────────

func TestListADRs_MultipleEntries(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("adr"))

	entries := map[string]string{
		"PURPOSE": "code intelligence MCP",
		"STACK":   "Go + SQLite",
		"TEAM":    "platform",
	}
	for k, v := range entries {
		if err := s.SetADR("adr", k, v); err != nil {
			t.Fatalf("SetADR %s: %v", k, v)
		}
	}

	got, err := s.ListADRs("adr")
	if err != nil {
		t.Fatalf("ListADRs: %v", err)
	}
	if len(got) != len(entries) {
		t.Errorf("ListADRs count=%d, want %d", len(got), len(entries))
	}
	for k, want := range entries {
		if got[k] != want {
			t.Errorf("ADR[%q]=%q, want %q", k, got[k], want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ListProjects — multiple projects
// ─────────────────────────────────────────────────────────────────────────────

func TestListProjects_Multiple(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"lp1", "lp2", "lp3"} {
		if err := s.UpsertProject(testProject(id)); err != nil {
			t.Fatalf("UpsertProject %s: %v", id, err)
		}
	}

	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) < 3 {
		t.Errorf("ListProjects count=%d, want >=3", len(projects))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetProject — not found returns nil
// ─────────────────────────────────────────────────────────────────────────────

func TestGetProject_MissingID(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetProject("nonexistent-project-xyz")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got != nil {
		t.Error("GetProject nonexistent should return nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// HealthCheck + formatStaleness
// ─────────────────────────────────────────────────────────────────────────────

func TestHealthCheck_NoProject(t *testing.T) {
	s := newTestStore(t)
	report, err := s.HealthCheck("")
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if report == nil {
		t.Fatal("report is nil")
	}
	if report.SchemaVersion <= 0 {
		t.Errorf("SchemaVersion=%d, want >0", report.SchemaVersion)
	}
	if report.Project != nil {
		t.Errorf("Project should be nil for empty projectID")
	}
}

func TestHealthCheck_WithProject(t *testing.T) {
	s := newTestStore(t)
	p := testProject("hp1")
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	// Insert a Go symbol so coverage has something to aggregate.
	sym := testSymbol("hp1::Fn#Function", "Fn", "Function", "hp1", "main.go")
	sym.ExtractionConfidence = 1.0
	if err := s.BulkUpsertSymbols([]Symbol{sym}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	report, err := s.HealthCheck("hp1")
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if report.Project == nil {
		t.Fatal("expected Project in report")
	}
	if report.Project.ID != "hp1" {
		t.Errorf("Project.ID=%q, want hp1", report.Project.ID)
	}
	if report.StalenessHuman == "" {
		t.Error("StalenessHuman should not be empty")
	}
	if len(report.Coverage) == 0 {
		t.Error("Coverage should have at least one entry")
	}
	// Go symbols with confidence=1.0 should be AST-parsed
	for _, lc := range report.Coverage {
		if lc.Language == "Go" && lc.Parser != "AST" {
			t.Errorf("Go Parser=%q, want AST", lc.Parser)
		}
	}
}

func TestHealthCheck_RegexLanguageCoverage(t *testing.T) {
	s := newTestStore(t)
	p := testProject("hpr")
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	sym := testSymbol("hpr::Fn#Function", "Fn", "Function", "hpr", "main.py")
	sym.Language = "Python"
	sym.ExtractionConfidence = 0.85 // regex extraction
	if err := s.BulkUpsertSymbols([]Symbol{sym}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	report, err := s.HealthCheck("hpr")
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	for _, lc := range report.Coverage {
		if lc.Language == "Python" && lc.Parser != "Regex" {
			t.Errorf("Python Parser=%q, want Regex (confidence=0.85)", lc.Parser)
		}
	}
}

func TestFormatStaleness(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{10 * time.Second, "10s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{72 * time.Hour, "3d"},
	}
	for _, tc := range cases {
		got := formatStaleness(tc.d)
		if got != tc.want {
			t.Errorf("formatStaleness(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DataDir
// ─────────────────────────────────────────────────────────────────────────────

func TestDataDir_ReturnsNonEmpty(t *testing.T) {
	dir, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if dir == "" {
		t.Error("DataDir returned empty string")
	}
	// Must be a directory that exists after the call
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("DataDir path does not exist: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("DataDir %q is not a directory", dir)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteProject
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteProject_ClearsSymbols(t *testing.T) {
	s := newTestStore(t)
	p := testProject("del-syms")
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	sym := testSymbol("del-syms::Fn#Function", "Fn", "Function", "del-syms", "f.go")
	if err := s.BulkUpsertSymbols([]Symbol{sym}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	if err := s.DeleteProject("del-syms"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	// Project should be gone
	got, err := s.GetProject("del-syms")
	if err != nil {
		t.Fatalf("GetProject after delete: %v", err)
	}
	if got != nil {
		t.Error("project still exists after DeleteProject")
	}

	// Symbols should be gone
	sym2, err := s.GetSymbol(sym.ID)
	if err != nil {
		t.Fatalf("GetSymbol after delete: %v", err)
	}
	if sym2 != nil {
		t.Error("symbol still exists after DeleteProject")
	}
}

func TestDeleteProject_NonExistent(t *testing.T) {
	s := newTestStore(t)
	// Deleting a project that doesn't exist should not error.
	if err := s.DeleteProject("does-not-exist"); err != nil {
		t.Errorf("DeleteProject nonexistent: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// migrate — schema version tracking
// ─────────────────────────────────────────────────────────────────────────────

func TestMigrate_SchemaVersion(t *testing.T) {
	s := newTestStore(t)
	// After Open(), schema_version should be at the latest version.
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	expected := 1 + len(schemaMigrations)
	if version != expected {
		t.Errorf("schema version=%d, want %d", version, expected)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	s := newTestStore(t)
	// Running migrate() again on an already-migrated DB should be safe.
	if err := s.migrate(); err != nil {
		t.Fatalf("second migrate(): %v", err)
	}
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	expected := 1 + len(schemaMigrations)
	if version != expected {
		t.Errorf("schema version after re-migrate=%d, want %d", version, expected)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Session persistence tests (RecordSession, GetSessions, GetAllTimeSavings)
// ─────────────────────────────────────────────────────────────────────────────

func TestRecordSession_Basic(t *testing.T) {
	s := newTestStore(t)
	start := time.Now().Add(-5 * time.Minute)
	if err := s.RecordSession("sess-001", start, 10, 500, 12000, 0.036); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	rows, err := s.GetSessions(10)
	if err != nil {
		t.Fatalf("GetSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d sessions, want 1", len(rows))
	}
	r := rows[0]
	if r.SessionID != "sess-001" {
		t.Errorf("session_id=%q, want sess-001", r.SessionID)
	}
	if r.Calls != 10 {
		t.Errorf("calls=%d, want 10", r.Calls)
	}
	if r.TokensUsed != 500 {
		t.Errorf("tokens_used=%d, want 500", r.TokensUsed)
	}
	if r.TokensSaved != 12000 {
		t.Errorf("tokens_saved=%d, want 12000", r.TokensSaved)
	}
}

func TestRecordSession_Upsert(t *testing.T) {
	s := newTestStore(t)
	start := time.Now().Add(-10 * time.Minute)
	// First write
	if err := s.RecordSession("sess-abc", start, 5, 200, 4000, 0.012); err != nil {
		t.Fatalf("first RecordSession: %v", err)
	}
	// Upsert with updated stats (same session_id)
	if err := s.RecordSession("sess-abc", start, 20, 900, 18000, 0.054); err != nil {
		t.Fatalf("second RecordSession: %v", err)
	}
	rows, err := s.GetSessions(10)
	if err != nil {
		t.Fatalf("GetSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d sessions after upsert, want 1", len(rows))
	}
	if rows[0].Calls != 20 {
		t.Errorf("calls after upsert=%d, want 20", rows[0].Calls)
	}
}

func TestGetSessions_OrderAndLimit(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	// Insert 3 sessions with different start times
	for i, offset := range []time.Duration{-3 * time.Hour, -2 * time.Hour, -1 * time.Hour} {
		id := "sess-" + string(rune('a'+i))
		if err := s.RecordSession(id, now.Add(offset), int64(i+1)*5, 100, 1000, 0.003); err != nil {
			t.Fatalf("RecordSession %d: %v", i, err)
		}
	}
	// Limit 2 — should get the 2 most recent
	rows, err := s.GetSessions(2)
	if err != nil {
		t.Fatalf("GetSessions(2): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d sessions, want 2", len(rows))
	}
	// Most recent first
	if rows[0].SessionID != "sess-c" {
		t.Errorf("first result=%q, want sess-c (most recent)", rows[0].SessionID)
	}
	if rows[1].SessionID != "sess-b" {
		t.Errorf("second result=%q, want sess-b", rows[1].SessionID)
	}
}

func TestGetSessions_Empty(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.GetSessions(10)
	if err != nil {
		t.Fatalf("GetSessions on empty DB: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows on empty DB, want 0", len(rows))
	}
}

func TestGetAllTimeSavings_Empty(t *testing.T) {
	s := newTestStore(t)
	calls, used, saved, cost, err := s.GetAllTimeSavings()
	if err != nil {
		t.Fatalf("GetAllTimeSavings on empty DB: %v", err)
	}
	if calls != 0 || used != 0 || saved != 0 || cost != 0 {
		t.Errorf("expected all zeros on empty DB, got calls=%d used=%d saved=%d cost=%v", calls, used, saved, cost)
	}
}

func TestGetAllTimeSavings_Aggregates(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	if err := s.RecordSession("sess-x", now.Add(-2*time.Hour), 10, 300, 5000, 0.015); err != nil {
		t.Fatalf("RecordSession 1: %v", err)
	}
	if err := s.RecordSession("sess-y", now.Add(-1*time.Hour), 20, 600, 10000, 0.030); err != nil {
		t.Fatalf("RecordSession 2: %v", err)
	}
	calls, used, saved, cost, err := s.GetAllTimeSavings()
	if err != nil {
		t.Fatalf("GetAllTimeSavings: %v", err)
	}
	if calls != 30 {
		t.Errorf("total calls=%d, want 30", calls)
	}
	if used != 900 {
		t.Errorf("total tokens_used=%d, want 900", used)
	}
	if saved != 15000 {
		t.Errorf("total tokens_saved=%d, want 15000", saved)
	}
	if cost < 0.044 || cost > 0.046 {
		t.Errorf("total cost_avoided=%v, want ~0.045", cost)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// withTx and migrate coverage tests
// ─────────────────────────────────────────────────────────────────────────────

func TestWithTx_RollbackOnError(t *testing.T) {
	s := newTestStore(t)
	// Insert a project, then roll it back via an error in withTx.
	intentionalErr := fmt.Errorf("intentional rollback")
	err := s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO projects(id, path, name, indexed_at) VALUES(?,?,?,?)`,
			"proj-rollback", "/tmp/rollback", "rollback", 0); err != nil {
			return err
		}
		return intentionalErr // triggers rollback
	})
	if err != intentionalErr {
		t.Fatalf("withTx returned %v, want intentionalErr", err)
	}
	// Project must NOT be in DB (rollback succeeded)
	p, _ := s.GetProject("proj-rollback")
	if p != nil {
		t.Error("project survived rollback — withTx did not roll back correctly")
	}
}

func TestWithTx_CommitOnSuccess(t *testing.T) {
	s := newTestStore(t)
	err := s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects(id, path, name, indexed_at) VALUES(?,?,?,?)`,
			"proj-commit", "/tmp/commit", "commit", 0)
		return err
	})
	if err != nil {
		t.Fatalf("withTx commit: %v", err)
	}
	p, err := s.GetProject("proj-commit")
	if err != nil {
		t.Fatalf("GetProject after commit: %v", err)
	}
	if p == nil {
		t.Error("project not found after successful withTx commit")
	}
}

func TestMigrate_SchemaVersionAfterReopen(t *testing.T) {
	dir := t.TempDir()
	// Open once — migrates to latest version
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	var v1 int
	_ = s1.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v1)
	s1.Close()

	// Reopen — migrate() must be idempotent; version must stay the same
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer s2.Close()
	var v2 int
	_ = s2.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v2)

	if v2 != v1 {
		t.Errorf("version after reopen=%d, want %d (same as first open)", v2, v1)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DataDir — idempotency
// ─────────────────────────────────────────────────────────────────────────────

func TestDataDir_CalledTwice_Idempotent(t *testing.T) {
	d1, err1 := DataDir()
	d2, err2 := DataDir()
	if err1 != nil || err2 != nil {
		t.Fatalf("DataDir errors: %v, %v", err1, err2)
	}
	if d1 != d2 {
		t.Errorf("DataDir not idempotent: %q != %q", d1, d2)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ProjectIDFromPath
// ─────────────────────────────────────────────────────────────────────────────

func TestProjectIDFromPath_AbsolutePassthrough(t *testing.T) {
	// An already-absolute path should come back as-is (after filepath.Abs no-op)
	dir := t.TempDir()
	id := ProjectIDFromPath(dir)
	if id != dir {
		t.Errorf("ProjectIDFromPath(%q) = %q, want same", dir, id)
	}
}

func TestProjectIDFromPath_RelativeResolved(t *testing.T) {
	// A relative path must be resolved to an absolute path
	id := ProjectIDFromPath(".")
	if id == "." {
		t.Error("ProjectIDFromPath('.') returned '.', want absolute path")
	}
	if !filepath.IsAbs(id) {
		t.Errorf("ProjectIDFromPath('.') = %q, want absolute", id)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ListADRs — empty project
// ─────────────────────────────────────────────────────────────────────────────

func TestListADRs_EmptyProject(t *testing.T) {
	store := newTestStore(t)
	adrs, err := store.ListADRs("no-such-project")
	if err != nil {
		t.Fatalf("ListADRs: %v", err)
	}
	if len(adrs) != 0 {
		t.Errorf("ListADRs nonexistent project: got %d, want 0", len(adrs))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GraphStats
// ─────────────────────────────────────────────────────────────────────────────

func TestGraphStats_Empty(t *testing.T) {
	store := newTestStore(t)
	symCount, edgeCount, kindCounts, edgeKindCounts, err := store.GraphStats("no-project")
	if err != nil {
		t.Fatalf("GraphStats: %v", err)
	}
	if symCount != 0 || edgeCount != 0 {
		t.Errorf("GraphStats empty: symCount=%d edgeCount=%d, want 0", symCount, edgeCount)
	}
	if len(kindCounts) != 0 || len(edgeKindCounts) != 0 {
		t.Errorf("GraphStats empty: kindCounts=%v edgeKindCounts=%v, want empty", kindCounts, edgeKindCounts)
	}
}

func TestGraphStats_WithMixedKinds(t *testing.T) {
	store := newTestStore(t)
	pid := "gstat-proj"
	store.UpsertProject(testProject(pid))

	syms := []Symbol{
		testSymbol("f1", "FuncA", "Function", pid, "a.go"),
		testSymbol("f2", "FuncB", "Function", pid, "a.go"),
		testSymbol("c1", "MyClass", "Class", pid, "b.go"),
	}
	if err := store.BulkUpsertSymbols(syms); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	edges := []Edge{
		{FromID: "f1", ToID: "f2", Kind: "CALLS", ProjectID: pid},
		{FromID: "c1", ToID: "f1", Kind: "IMPORTS", ProjectID: pid},
	}
	if err := store.BulkUpsertEdges(edges); err != nil {
		t.Fatalf("BulkUpsertEdges: %v", err)
	}

	symCount, edgeCount, kindCounts, edgeKindCounts, err := store.GraphStats(pid)
	if err != nil {
		t.Fatalf("GraphStats with data: %v", err)
	}
	if symCount != 3 {
		t.Errorf("symCount=%d, want 3", symCount)
	}
	if edgeCount != 2 {
		t.Errorf("edgeCount=%d, want 2", edgeCount)
	}
	if kindCounts["Function"] != 2 {
		t.Errorf("kindCounts[Function]=%d, want 2", kindCounts["Function"])
	}
	if kindCounts["Class"] != 1 {
		t.Errorf("kindCounts[Class]=%d, want 1", kindCounts["Class"])
	}
	if edgeKindCounts["CALLS"] != 1 {
		t.Errorf("edgeKindCounts[CALLS]=%d, want 1", edgeKindCounts["CALLS"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteSymbolsForFile
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteSymbolsForFile_ClearsEdges(t *testing.T) {
	store := newTestStore(t)
	pid := "dsff-proj"
	store.UpsertProject(testProject(pid))

	syms := []Symbol{
		testSymbol("dsff-a", "Alpha", "Function", pid, "alpha.go"),
		testSymbol("dsff-b", "Beta", "Function", pid, "beta.go"),
	}
	store.BulkUpsertSymbols(syms)
	edges := []Edge{
		{FromID: "dsff-a", ToID: "dsff-b", Kind: "CALLS", ProjectID: pid},
	}
	store.BulkUpsertEdges(edges)

	// Delete symbols for alpha.go
	if err := store.DeleteSymbolsForFile(pid, "alpha.go"); err != nil {
		t.Fatalf("DeleteSymbolsForFile: %v", err)
	}

	// dsff-a should be gone
	sym, err := store.GetSymbol("dsff-a")
	if err != nil {
		t.Fatalf("GetSymbol after delete: %v", err)
	}
	if sym != nil {
		t.Error("dsff-a still exists after DeleteSymbolsForFile")
	}
	// dsff-b should still exist
	sym2, err := store.GetSymbol("dsff-b")
	if err != nil {
		t.Fatalf("GetSymbol dsff-b: %v", err)
	}
	if sym2 == nil {
		t.Error("dsff-b was incorrectly deleted")
	}
}

func TestDeleteSymbolsForFile_NonExistent_NoError(t *testing.T) {
	store := newTestStore(t)
	// Deleting symbols for a file that doesn't exist should not error
	if err := store.DeleteSymbolsForFile("any-proj", "nonexistent.go"); err != nil {
		t.Errorf("DeleteSymbolsForFile nonexistent: %v", err)
	}
}

func TestDeleteEmptyProjects(t *testing.T) {
	s := newTestStore(t)

	// Three projects: one real, one empty (ghost), one with only edges.
	real := testProject("real")
	real.SymCount = 42
	real.EdgeCount = 7
	s.UpsertProject(real)

	ghost := testProject("ghost")
	ghost.SymCount = 0
	ghost.EdgeCount = 0
	s.UpsertProject(ghost)

	edgesOnly := testProject("edges-only")
	edgesOnly.SymCount = 0
	edgesOnly.EdgeCount = 3
	s.UpsertProject(edgesOnly)

	n, err := s.DeleteEmptyProjects()
	if err != nil {
		t.Fatalf("DeleteEmptyProjects: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted count = %d, want 1", n)
	}

	// Only the ghost should be gone.
	if got, _ := s.GetProject("ghost"); got != nil {
		t.Error("ghost project should be deleted")
	}
	if got, _ := s.GetProject("real"); got == nil {
		t.Error("real project should survive")
	}
	if got, _ := s.GetProject("edges-only"); got == nil {
		t.Error("edges-only project should survive (has edges)")
	}

	// Idempotent: a second call has nothing to do.
	n2, err := s.DeleteEmptyProjects()
	if err != nil {
		t.Fatalf("second DeleteEmptyProjects: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second call deleted count = %d, want 0", n2)
	}
}
