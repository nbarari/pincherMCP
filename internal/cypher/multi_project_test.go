package cypher

import (
	"context"
	"strings"
	"testing"
)

// AllowAllProjects=false (default) with empty ProjectID is the
// existing safety guard — defense-in-depth against agents that
// forget to scope. The error message must point the caller at the
// opt-in.
func TestExecutor_EmptyProjectID_DefaultRejects(t *testing.T) {
	db := newTestDB(t)
	e := &Executor{DB: db, MaxRows: 100} // no ProjectID, AllowAllProjects=false
	_, err := e.Execute(context.Background(), `MATCH (n) RETURN n.name LIMIT 1`)
	if err == nil {
		t.Fatal("expected error for empty ProjectID without AllowAllProjects")
	}
	if !strings.Contains(err.Error(), "cross-project") {
		t.Errorf("error should mention cross-project context; got: %v", err)
	}
}

// AllowAllProjects=true opts in to cross-project execution. The
// query returns rows from every project in the store.
func TestExecutor_AllowAllProjects_ReturnsCrossProjectRows(t *testing.T) {
	db := newTestDB(t)

	// Seed two projects with one symbol each.
	if _, err := db.Exec(`
		INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language,
			start_byte, end_byte, start_line, end_line)
		VALUES
			('p1::Foo#Function', 'p1', 'a.go', 'Foo', 'pkg.Foo', 'Function', 'Go', 0, 10, 1, 2),
			('p2::Bar#Function', 'p2', 'b.go', 'Bar', 'pkg.Bar', 'Function', 'Go', 0, 10, 1, 2)
	`); err != nil {
		t.Fatal(err)
	}

	// Cross-project: should see both Foo (p1) and Bar (p2).
	e := &Executor{DB: db, MaxRows: 100, AllowAllProjects: true}
	r, err := e.Execute(context.Background(), `MATCH (n:Function) RETURN n.name`)
	if err != nil {
		t.Fatalf("cross-project Execute: %v", err)
	}
	names := map[string]bool{}
	for _, row := range r.Rows {
		if v, ok := row["n.name"].(string); ok {
			names[v] = true
		}
	}
	if !names["Foo"] || !names["Bar"] {
		t.Errorf("expected both Foo (p1) and Bar (p2) in cross-project result; got %v", names)
	}
}

// Single-project mode (ProjectID set, AllowAllProjects=false) is
// unchanged — must still scope to that project even when other
// projects exist in the store.
func TestExecutor_SingleProject_StillScopes(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`
		INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language,
			start_byte, end_byte, start_line, end_line)
		VALUES
			('p1::Foo#Function', 'p1', 'a.go', 'Foo', 'pkg.Foo', 'Function', 'Go', 0, 10, 1, 2),
			('p2::Bar#Function', 'p2', 'b.go', 'Bar', 'pkg.Bar', 'Function', 'Go', 0, 10, 1, 2)
	`); err != nil {
		t.Fatal(err)
	}

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "p1"} // single-project
	r, err := e.Execute(context.Background(), `MATCH (n:Function) RETURN n.name`)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, row := range r.Rows {
		if v, ok := row["n.name"].(string); ok {
			names[v] = true
		}
	}
	if !names["Foo"] {
		t.Errorf("Foo (p1) missing from single-project result; got %v", names)
	}
	if names["Bar"] {
		t.Errorf("Bar (p2) leaked into p1-scoped result; got %v", names)
	}
}
