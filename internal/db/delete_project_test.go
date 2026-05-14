package db

import (
	"testing"
	"time"
)

// #799: DeleteProject only cleaned edges/symbols/files/adrs/projects.
// `closure` and `extraction_failures` carry a REFERENCES projects(id)
// FK, so for any project that had extraction failures or a built
// closure table the final `DELETE FROM projects` failed outright with
// "FOREIGN KEY constraint failed". The unconstrained per-project tables
// (pending_edges, struct_fields, interface_methods, symbol_moves,
// slow_queries) leaked orphan rows on every delete. DeleteProject now
// covers every per-project table.
func TestDeleteProject_ClearsAllPerProjectTables(t *testing.T) {
	s := newTestStore(t)
	const pid = "delproj"

	if err := s.UpsertProject(testProject(pid)); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := s.BulkUpsertSymbols([]Symbol{
		testSymbol(pid+"::A#Function", "A", "Function", pid, "a.go"),
		testSymbol(pid+"::B#Function", "B", "Function", pid, "b.go"),
	}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	now := time.Now().Unix()
	// Seed a row in every per-project table DeleteProject must clear —
	// directly, so the test is independent of the writer APIs.
	seed := []struct {
		label string
		sql   string
		args  []any
	}{
		{"edges", `INSERT INTO edges(project_id,from_id,to_id,kind) VALUES(?,?,?,?)`,
			[]any{pid, pid + "::A#Function", pid + "::B#Function", "CALLS"}},
		{"files", `INSERT INTO files(project_id,path,hash,indexed_at) VALUES(?,?,?,?)`,
			[]any{pid, "a.go", "h", now}},
		{"adrs", `INSERT INTO adrs(project_id,key,value,updated_at) VALUES(?,?,?,?)`,
			[]any{pid, "K", "V", now}},
		{"extraction_failures", `INSERT INTO extraction_failures(project_id,file_path,language,reason,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?)`,
			[]any{pid, "broken.c", "C", "parse_error", now, now}},
		{"closure", `INSERT INTO closure(project_id,from_id,to_id,depth) VALUES(?,?,?,?)`,
			[]any{pid, pid + "::A#Function", pid + "::B#Function", 1}},
		{"pending_edges", `INSERT INTO pending_edges(project_id,from_file,kind,from_qn,to_name) VALUES(?,?,?,?,?)`,
			[]any{pid, "a.go", "CALLS", "A", "B"}},
		{"struct_fields", `INSERT INTO struct_fields(project_id,struct_id,field_name,field_type) VALUES(?,?,?,?)`,
			[]any{pid, pid + "::A#Function", "f", "int"}},
		{"interface_methods", `INSERT INTO interface_methods(project_id,interface_id,method_name) VALUES(?,?,?)`,
			[]any{pid, pid + "::A#Function", "M"}},
		{"symbol_moves", `INSERT INTO symbol_moves(old_id,new_id,project_id,moved_at) VALUES(?,?,?,?)`,
			[]any{pid + "::A#Function", pid + "::A2#Function", pid, now}},
		{"slow_queries", `INSERT INTO slow_queries(tool,project_id,duration_ms,occurred_at) VALUES(?,?,?,?)`,
			[]any{"query", pid, 999, now}},
	}
	for _, row := range seed {
		if _, err := s.db.Exec(row.sql, row.args...); err != nil {
			t.Fatalf("seed %s: %v", row.label, err)
		}
	}

	// Pre-fix this errored with "FOREIGN KEY constraint failed".
	if err := s.DeleteProject(pid); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	// Every per-project table must have zero rows for this project.
	tables := []struct {
		name, where string
	}{
		{"edges", "project_id=?"},
		{"symbols", "project_id=?"},
		{"files", "project_id=?"},
		{"adrs", "project_id=?"},
		{"extraction_failures", "project_id=?"},
		{"closure", "project_id=?"},
		{"pending_edges", "project_id=?"},
		{"struct_fields", "project_id=?"},
		{"interface_methods", "project_id=?"},
		{"symbol_moves", "project_id=?"},
		{"slow_queries", "project_id=?"},
		{"projects", "id=?"},
	}
	for _, tbl := range tables {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM "+tbl.name+" WHERE "+tbl.where, pid).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl.name, err)
		}
		if n != 0 {
			t.Errorf("%s still has %d row(s) for the deleted project — DeleteProject leaked them", tbl.name, n)
		}
	}
}
