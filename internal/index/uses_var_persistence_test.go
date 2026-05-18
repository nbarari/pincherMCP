package index

import (
	"context"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// TestIndex_AnsibleUsesVar_PersistsAcrossForceReindex_1479 covers the
// production-pipeline regression reported in #1479: the dogfood case
// where `pincher index --force --json-summary` reports a non-zero
// USES_VAR count in edges_by_kind but a follow-up `mcp__pincher__schema`
// shows USES_VAR absent from the project.
//
// The hypothesis tested here: a second force-reindex pass on top of an
// existing USES_VAR-populated DB MUST land USES_VAR rows in the DB,
// not just report them in the summary. This catches an early-return
// or delete-without-replace in resolveUsesVar that the single-pass
// TestIndex_AnsibleUsesVar_BindsAcrossFiles_1165 doesn't exercise.
//
// Mirrors the user's corpus shape more closely than the existing test:
// var declarations live in three of the canonical path families
// (group_vars/, host_vars/, roles/<r>/defaults/), and tasks +
// templates reference them across files.
func TestIndex_AnsibleUsesVar_PersistsAcrossForceReindex_1479(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	// Three var-decl paths covering three canonical families.
	writeFile(t, dir, "group_vars/all.yml", "db_host: postgres.internal\n")
	writeFile(t, dir, "host_vars/web01.yml", "log_level: debug\n")
	writeFile(t, dir, "roles/app/defaults/main.yml", "app_port: 8080\n")

	// Tasks + template referencing all three vars.
	writeFile(t, dir, "roles/app/tasks/main.yml", `---
- name: configure
  shell: psql -h {{ db_host }} -c "SELECT 1"
- name: log
  debug:
    msg: "level={{ log_level }}"
`)
	writeFile(t, dir, "roles/app/templates/app.conf.j2",
		"DATABASE_URL=postgres://{{ db_host }}/mydb\nPORT={{ app_port }}\nLOG={{ log_level }}\n")

	// First pass: incremental (force=false) — same shape as the
	// existing TestIndex_AnsibleUsesVar_BindsAcrossFiles_1165 test,
	// confirms baseline.
	res, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("first Index: %v", err)
	}
	projectID := res.ProjectID

	countUsesVar := func(label string) int {
		t.Helper()
		settings, err := store.GetSymbolsByName(projectID, "db_host", 50)
		if err != nil {
			t.Fatalf("[%s] GetSymbolsByName(db_host): %v", label, err)
		}
		var dbHost db.Symbol
		for _, s := range settings {
			if s.Kind == "Setting" && s.FilePath == "group_vars/all.yml" {
				dbHost = s
				break
			}
		}
		if dbHost.ID == "" {
			t.Fatalf("[%s] group_vars/all.yml::db_host Setting not found", label)
		}
		edges, err := store.EdgesTo(dbHost.ID, []string{"USES_VAR"})
		if err != nil {
			t.Fatalf("[%s] EdgesTo USES_VAR: %v", label, err)
		}
		return len(edges)
	}

	first := countUsesVar("first pass")
	if first == 0 {
		t.Fatalf("first pass produced zero USES_VAR edges to db_host — baseline broken before reindex regression test runs")
	}

	// Second pass: force=true. Triggers DeleteEdgesByKindAndSource
	// then BulkUpsertEdges. The #1479 regression would leave the DB
	// at zero USES_VAR if either:
	//   - resolveUsesVar early-returns before BulkUpsertEdges, OR
	//   - resolveUsesVar runs but every pending edge fails to bind
	//     (fromID or toID empty), AND the DELETE runs anyway, leaving
	//     the project at 0 rows.
	if _, err := idx.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("force Index: %v", err)
	}
	second := countUsesVar("force reindex")
	if second != first {
		t.Errorf("USES_VAR count after force reindex = %d, want %d (matches first pass — DELETE-without-INSERT regression?)", second, first)
	}

	// Third pass: another force, mimics the watcher-triggered
	// binaryDriftForce path that the user's repro implicates.
	if _, err := idx.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("second force Index: %v", err)
	}
	third := countUsesVar("second force reindex")
	if third != first {
		t.Errorf("USES_VAR count after second force reindex = %d, want %d", third, first)
	}
}

// TestIndex_AnsibleUsesVar_RoleVarsAndBuildsVarsBind_1479 covers the
// remaining two canonical var-decl path families the existing test
// doesn't exercise: `roles/<r>/vars/main.yml` and `vars/<name>.yml`
// (playbook-adjacent). Tests that isAnsibleVarDeclFile + the
// settingByName bucket actually thread both shapes through to USES_VAR
// edges in the DB.
func TestIndex_AnsibleUsesVar_RoleVarsAndBuildsVarsBind_1479(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	writeFile(t, dir, "roles/app/vars/main.yml", "service_name: webapp\n")
	writeFile(t, dir, "vars/common.yml", "region: us-east-1\n")
	writeFile(t, dir, "roles/app/tasks/main.yml", `---
- name: tag
  debug:
    msg: "service={{ service_name }} region={{ region }}"
`)

	res, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	expectEdgesFor := func(name, declPath string) {
		t.Helper()
		settings, err := store.GetSymbolsByName(res.ProjectID, name, 50)
		if err != nil {
			t.Fatalf("GetSymbolsByName(%s): %v", name, err)
		}
		var target db.Symbol
		for _, s := range settings {
			if s.Kind == "Setting" && s.FilePath == declPath {
				target = s
				break
			}
		}
		if target.ID == "" {
			t.Fatalf("Setting %s in %s not found", name, declPath)
		}
		edges, err := store.EdgesTo(target.ID, []string{"USES_VAR"})
		if err != nil {
			t.Fatalf("EdgesTo USES_VAR for %s: %v", name, err)
		}
		if len(edges) == 0 {
			t.Errorf("expected USES_VAR edges to %s (decl in %s); got 0 — isAnsibleVarDeclFile may not be matching this path family", name, declPath)
		}
	}

	// roles/<r>/vars/main.yml — matches isAnsibleVarDeclFile via /vars/.
	expectEdgesFor("service_name", "roles/app/vars/main.yml")
	// vars/<name>.yml — matches via vars/ prefix.
	expectEdgesFor("region", "vars/common.yml")
}
