package cypher

import (
	"context"
	"database/sql"
	"testing"
)

// insertSymInProject inserts a symbol under a caller-chosen project_id —
// needed for the cross-project project_id-property tests, which span
// two projects to prove rows are attributable.
func insertSymInProject(t *testing.T, db *sql.DB, id, project, name, kind string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language,
			start_byte, end_byte, start_line, end_line) VALUES (?,?,?,?,?,?,?, 0,100,1,5)`,
		id, project, "file.go", name, name, kind, "Go",
	)
	if err != nil {
		t.Fatalf("insert symbol %q: %v", id, err)
	}
}

// #746: project_id was not an addressable pinchQL property, so a
// project=* cross-project query returned rows with no way to tell
// which repo each came from — file_path is project-relative and
// collides across repos. project_id is now exposed.
func TestExecute_ProjectIDProperty_CrossProjectAttribution(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSymInProject(t, db, "a1", "repoA", "Handler", "Function")
	insertSymInProject(t, db, "b1", "repoB", "Handler", "Function")

	e := &Executor{DB: db, MaxRows: 100, AllowAllProjects: true}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.name = "Handler" RETURN n.name, n.project_id`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 2 {
		t.Fatalf("rows=%d, want 2 (one Handler per project)", len(r.Rows))
	}
	// project_id must be populated and distinguish the two rows.
	seen := map[string]bool{}
	for _, row := range r.Rows {
		pid, _ := row["n.project_id"].(string)
		if pid == "" {
			t.Errorf("row %v has empty n.project_id — cross-project rows must be attributable", row)
		}
		seen[pid] = true
	}
	if !seen["repoA"] || !seen["repoB"] {
		t.Errorf("expected both repoA and repoB in project_id column; got %v", seen)
	}
	// project_id is a known property — no unknown-property warning.
	for _, w := range r.Warnings {
		if contains(w, "project_id") && contains(w, "not recognized") {
			t.Errorf("project_id flagged as unknown property: %q", w)
		}
	}
}

// The `project` alias resolves to the same column.
func TestExecute_ProjectIDProperty_AliasAndFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSymInProject(t, db, "a1", "repoA", "Alpha", "Function")
	insertSymInProject(t, db, "b1", "repoB", "Beta", "Function")

	e := &Executor{DB: db, MaxRows: 100, AllowAllProjects: true}
	// WHERE on the `project` alias must push down / evaluate correctly.
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.project = "repoA" RETURN n.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("rows=%d, want 1 (only repoA)", len(r.Rows))
	}
	if name, _ := r.Rows[0]["n.name"].(string); name != "Alpha" {
		t.Errorf("row name = %q, want Alpha", name)
	}
	for _, w := range r.Warnings {
		if contains(w, "project") && contains(w, "not recognized") {
			t.Errorf("project alias flagged as unknown property: %q", w)
		}
	}
}

// #774: the documented property aliases (project/qn/label) resolved in
// WHERE (via cypherPropToCol's SQL pushdown) but returned null in RETURN
// — symRowToMap only carried the canonical keys. A cross-project query
// asking `RETURN n.project` got rows with n.project=null even though
// n.project_id had the value.
func TestExecute_PropertyAliases_ResolveInReturn(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSymInProject(t, db, "a1", "repoA", "Alpha", "Function")

	e := &Executor{DB: db, MaxRows: 100, AllowAllProjects: true}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.name = "Alpha" RETURN n.project, n.qn, n.label`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("rows=%d, want 1", len(r.Rows))
	}
	row := r.Rows[0]
	// insertSymInProject sets qualified_name = name and kind = "Function".
	for col, want := range map[string]string{
		"n.project": "repoA",
		"n.qn":      "Alpha",
		"n.label":   "Function",
	} {
		if got, _ := row[col].(string); got != want {
			t.Errorf("RETURN %s = %v, want %q — documented alias must resolve in RETURN, not just WHERE", col, row[col], want)
		}
	}
}

// contains is a tiny substring helper local to this test file.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
