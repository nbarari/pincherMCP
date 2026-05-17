package index

import (
	"context"
	"testing"

	"github.com/kwad77/pincher/internal/ast"
	"github.com/kwad77/pincher/internal/db"
)

// #1165: end-to-end fixture — declare `db_host` in group_vars/all.yml,
// reference it from a task file via `{{ db_host }}`, and a Jinja
// template via `{{ db_host }}`. resolveUsesVar must bind both edges
// to the canonical Setting symbol.
func TestIndex_AnsibleUsesVar_BindsAcrossFiles_1165(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	writeFile(t, dir, "group_vars/all.yml", "db_host: postgres.internal\n")
	writeFile(t, dir, "roles/app/tasks/main.yml", `---
- name: configure
  shell: psql -h {{ db_host }} -c "SELECT 1"
`)
	writeFile(t, dir, "roles/app/templates/config.j2",
		"DATABASE_URL=postgres://{{ db_host }}/mydb\n")

	res, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	// Find the Setting symbol that USES_VAR edges should point at.
	settings, err := store.GetSymbolsByName(res.ProjectID, "db_host", 50)
	if err != nil {
		t.Fatalf("GetSymbolsByName: %v", err)
	}
	var dbHost db.Symbol
	for _, s := range settings {
		if s.Kind == "Setting" && s.FilePath == "group_vars/all.yml" {
			dbHost = s
			break
		}
	}
	if dbHost.ID == "" {
		t.Fatalf("group_vars/all.yml::db_host Setting not found in %d candidates: %+v", len(settings), settings)
	}

	// Collect every USES_VAR edge whose to_id is db_host.
	usesVarEdges, err := store.EdgesTo(dbHost.ID, []string{"USES_VAR"})
	if err != nil {
		t.Fatalf("EdgesTo(USES_VAR): %v", err)
	}
	var fromFiles []string
	for _, e := range usesVarEdges {
		src, err := store.GetSymbol(e.FromID)
		if err != nil || src == nil {
			t.Errorf("GetSymbol(%s): %v", e.FromID, err)
			continue
		}
		fromFiles = append(fromFiles, src.FilePath)
	}
	wantSources := map[string]bool{
		"roles/app/tasks/main.yml":      true,
		"roles/app/templates/config.j2": true,
	}
	for _, got := range fromFiles {
		delete(wantSources, got)
	}
	if len(wantSources) != 0 {
		t.Errorf("missing USES_VAR edges from %v; saw edges from %v", wantSources, fromFiles)
	}
}

// Negative: USES_VAR refs whose name doesn't bind to any var-decl
// Setting must not silently bind to a same-named code symbol. The
// resolver filters Settings by canonical Ansible var-decl paths, so a
// `db_host` *Function* sitting outside group_vars/ must not satisfy the
// reference.
func TestIndex_AnsibleUsesVar_UnboundDropsCleanly_1165(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	// db_host as a Setting in a NON-var-decl path (e.g. a Helm values
	// file). Reference from an Ansible task. The resolver must not
	// bind through this — the path is outside group_vars/ / host_vars/
	// / vars/ / defaults/.
	writeFile(t, dir, "charts/myapp/values.yaml", "db_host: postgres.internal\n")
	writeFile(t, dir, "roles/app/tasks/main.yml", `---
- name: configure
  shell: psql -h {{ db_host }} -c "SELECT 1"
`)

	res, err := idx.Index(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	// Walk every Module symbol and assert none has an outbound USES_VAR
	// edge — the only USES_VAR ref in the project pointed at a Setting
	// in a non-recognised path, so it must have dropped.
	allMods, err := store.GetSymbolsByQN(res.ProjectID, "main")
	if err != nil {
		t.Fatalf("GetSymbolsByQN: %v", err)
	}
	for _, mod := range allMods {
		out, err := store.EdgesFrom(mod.ID, []string{"USES_VAR"})
		if err != nil {
			t.Fatalf("EdgesFrom: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("module %s (file %s) emitted %d USES_VAR edges; want 0: %+v",
				mod.ID, mod.FilePath, len(out), out)
		}
	}
}

// Direct unit test of the canonical-pick when multiple var-decl files
// declare the same name (e.g. group_vars/all.yml AND
// group_vars/staging.yml both declare `db_host`). The lex-smallest ID
// must win — same #428 pattern as resolveImports.
func TestResolveUsesVar_PickCanonicalLexSmallest_1165(t *testing.T) {
	idx, store := newTestIndexer(t)

	projectID := "test-uses-var"
	if err := store.UpsertProject(db.Project{ID: projectID, Path: "/test", Name: "test"}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	// Two var-decl Settings with the same name. ID = file_path::qn#kind,
	// so all.yml < staging.yml lexicographically — all.yml wins.
	settingAll := db.Symbol{
		ID:                   db.MakeSymbolID("group_vars/all.yml", "db_host", "Setting"),
		ProjectID:            projectID,
		FilePath:             "group_vars/all.yml",
		Name:                 "db_host",
		QualifiedName:        "db_host",
		Kind:                 "Setting",
		Language:             "YAML",
		ExtractionConfidence: 1.0,
	}
	settingStaging := db.Symbol{
		ID:                   db.MakeSymbolID("group_vars/staging.yml", "db_host", "Setting"),
		ProjectID:            projectID,
		FilePath:             "group_vars/staging.yml",
		Name:                 "db_host",
		QualifiedName:        "db_host",
		Kind:                 "Setting",
		Language:             "YAML",
		ExtractionConfidence: 1.0,
	}
	// The from-side Module symbol (Ansible task file).
	fromMod := db.Symbol{
		ID:                   db.MakeSymbolID("roles/app/tasks/main.yml", "main", "Module"),
		ProjectID:            projectID,
		FilePath:             "roles/app/tasks/main.yml",
		Name:                 "main",
		QualifiedName:        "main",
		Kind:                 "Module",
		Language:             "YAML",
		ExtractionConfidence: 1.0,
	}
	if err := store.BulkUpsertSymbols([]db.Symbol{settingAll, settingStaging, fromMod}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	pending := []ast.ExtractedEdge{
		{
			FromQN: "main", FromFile: "roles/app/tasks/main.yml",
			ToName: "db_host", Kind: "USES_VAR", Confidence: 0.85,
		},
	}
	n := idx.resolveUsesVar(projectID, pending)
	if n != 1 {
		t.Fatalf("resolveUsesVar inserted %d edges; want 1", n)
	}

	edges, err := store.EdgesFrom(fromMod.ID, []string{"USES_VAR"})
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 USES_VAR edge; got %d", len(edges))
	}
	if edges[0].ToID != settingAll.ID {
		t.Errorf("canonical-pick chose ToID=%s; want %s (lex-smallest)", edges[0].ToID, settingAll.ID)
	}
}
