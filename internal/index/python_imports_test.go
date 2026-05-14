package index

import (
	"context"
	"testing"

	"github.com/kwad77/pincher/internal/ast"
	"github.com/kwad77/pincher/internal/db"
)

// Python IMPORTS edge resolution end-to-end test. Builds a src-layout
// project on disk, indexes it, and asserts that:
//  1. Each .py file gets a Module symbol whose QN is the file's
//     dotted relpath (src.myproj.config etc.).
//  2. The `from myproj.config import ServerSpec` in main.py resolves
//     into an IMPORTS edge to src.myproj.config — i.e. the src-prefix
//     gap from the user's report is closed.
//
// Skips when python3 is unavailable; the AST extractor is the only
// path that emits Module symbols for Python, so the resolver can't
// match either side without it.
func TestIndex_PythonImportsResolveAcrossSrcLayout(t *testing.T) {
	if !ast.PythonAvailable() {
		t.Skip("python3 not on PATH; Python AST resolution test skipped")
	}

	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	writeFile(t, dir, "pyproject.toml", `
[tool.setuptools.packages.find]
where = ["src"]
`)
	writeFile(t, dir, "src/myproj/__init__.py", "")
	writeFile(t, dir, "src/myproj/config.py", `class ServerSpec:
    pass
`)
	writeFile(t, dir, "src/myproj/main.py", `from myproj.config import ServerSpec

def run():
    return ServerSpec()
`)

	if _, err := idx.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("Index: %v", err)
	}
	projectID := db.ProjectIDFromPath(dir)

	mainMods, err := store.GetSymbolsByQN(projectID, "src.myproj.main")
	if err != nil || len(mainMods) == 0 {
		t.Fatalf("expected Module symbol src.myproj.main, got %d (err=%v)", len(mainMods), err)
	}
	if mainMods[0].Kind != "Module" {
		t.Errorf("main kind = %q, want Module", mainMods[0].Kind)
	}

	// The import target was written as `myproj.config.ServerSpec`; the
	// resolver should prepend the `src` source-root prefix and find the
	// class symbol.
	targetSyms, err := store.GetSymbolsByQN(projectID, "src.myproj.config.ServerSpec")
	if err != nil || len(targetSyms) == 0 {
		t.Fatalf("expected ServerSpec class symbol, got %d (err=%v)", len(targetSyms), err)
	}

	edges, err := store.EdgesFrom(mainMods[0].ID, nil)
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	var found bool
	for _, e := range edges {
		if e.Kind == "IMPORTS" && e.ToID == targetSyms[0].ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected IMPORTS edge from src.myproj.main to src.myproj.config.ServerSpec; edges=%+v", edges)
	}
}

// #860: a Python import whose name collides with a config-file key must
// not false-bind to that key's Setting symbol. `import os` has no
// in-project target — it's a stdlib import — but a top-level `os` key in
// a JSON/YAML/TOML file extracts as a Setting whose QN is literally
// "os", and resolveImports' canonical-pick would otherwise match it. An
// IMPORTS-edge target is always a code symbol.
func TestIndex_PythonImportsSkipConfigKeyCollision(t *testing.T) {
	if !ast.PythonAvailable() {
		t.Skip("python3 not on PATH; Python AST resolution test skipped")
	}

	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	// A config file with a top-level `os` key → Setting symbol QN "os".
	writeFile(t, dir, "settings.json", `{"os": "linux", "name": "demo"}`)
	// A Python file that does `import os` (stdlib — no in-project target).
	writeFile(t, dir, "app.py", `import os

def where():
    return os.getcwd()
`)

	if _, err := idx.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("Index: %v", err)
	}
	projectID := db.ProjectIDFromPath(dir)

	// The `os` Setting must exist (sanity: the collision is real).
	osSyms, err := store.GetSymbolsByQN(projectID, "os")
	if err != nil {
		t.Fatalf("GetSymbolsByQN os: %v", err)
	}
	var osSettingID string
	for _, s := range osSyms {
		if s.Kind == "Setting" {
			osSettingID = s.ID
		}
	}
	if osSettingID == "" {
		t.Fatal("test setup invariant: expected a Setting symbol with QN \"os\" from settings.json")
	}

	// app.py's Module must NOT carry an IMPORTS edge to that Setting.
	appMods, err := store.GetSymbolsByQN(projectID, "app")
	if err != nil || len(appMods) == 0 {
		t.Fatalf("expected Module symbol app, got %d (err=%v)", len(appMods), err)
	}
	edges, err := store.EdgesFrom(appMods[0].ID, nil)
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	for _, e := range edges {
		if e.Kind == "IMPORTS" && e.ToID == osSettingID {
			t.Errorf("import os false-bound to the config-key Setting %q — IMPORTS targets must be code symbols", osSettingID)
		}
	}
}

// Python CALLS resolution across files: a function in main.py imports a
// helper from another module and calls it. The extractor's import-alias
// rewrite produces a to_name dotted at Python's module path; the resolver's
// PythonImportCandidates expansion prepends the source root to find the
// real helper symbol QN.
func TestIndex_PythonCallsResolveAcrossFiles(t *testing.T) {
	if !ast.PythonAvailable() {
		t.Skip("python3 not on PATH; Python CALLS resolution test skipped")
	}

	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	writeFile(t, dir, "src/myproj/__init__.py", "")
	writeFile(t, dir, "src/myproj/util.py", `def helper():
    return 1
`)
	writeFile(t, dir, "src/myproj/main.py", `from myproj.util import helper

def run():
    return helper()
`)

	if _, err := idx.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("Index: %v", err)
	}
	projectID := db.ProjectIDFromPath(dir)

	runSyms, err := store.GetSymbolsByQN(projectID, "src.myproj.main.run")
	if err != nil || len(runSyms) == 0 {
		t.Fatalf("expected src.myproj.main.run symbol, got %d (err=%v)", len(runSyms), err)
	}
	helperSyms, err := store.GetSymbolsByQN(projectID, "src.myproj.util.helper")
	if err != nil || len(helperSyms) == 0 {
		t.Fatalf("expected src.myproj.util.helper symbol, got %d (err=%v)", len(helperSyms), err)
	}

	edges, err := store.EdgesFrom(runSyms[0].ID, nil)
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	var found bool
	for _, e := range edges {
		if e.Kind == "CALLS" && e.ToID == helperSyms[0].ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected CALLS edge run→helper across files; got edges=%+v", edges)
	}
}

// Same-class self.X() call: the extractor rewrites `self.helper()` to the
// class-qualified target so the resolver can match the method's actual QN.
func TestIndex_PythonCallsResolveSelfMethod(t *testing.T) {
	if !ast.PythonAvailable() {
		t.Skip("python3 not on PATH")
	}

	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	writeFile(t, dir, "src/myproj/__init__.py", "")
	writeFile(t, dir, "src/myproj/svc.py", `class Svc:
    def helper(self):
        return 1

    def run(self):
        return self.helper()
`)

	if _, err := idx.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("Index: %v", err)
	}
	projectID := db.ProjectIDFromPath(dir)

	runSyms, err := store.GetSymbolsByQN(projectID, "src.myproj.svc.Svc.run")
	if err != nil || len(runSyms) == 0 {
		t.Fatalf("expected Svc.run symbol, got %d (err=%v)", len(runSyms), err)
	}
	helperSyms, err := store.GetSymbolsByQN(projectID, "src.myproj.svc.Svc.helper")
	if err != nil || len(helperSyms) == 0 {
		t.Fatalf("expected Svc.helper symbol, got %d (err=%v)", len(helperSyms), err)
	}

	edges, err := store.EdgesFrom(runSyms[0].ID, nil)
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	var found bool
	for _, e := range edges {
		if e.Kind == "CALLS" && e.ToID == helperSyms[0].ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected CALLS edge Svc.run→Svc.helper via self.helper(); got edges=%+v", edges)
	}
}

// Without source-root awareness, the resolver can't bridge "myproj.config"
// (Python import path) and "src.myproj.config" (pincher's file-path QN).
// Setting PINCHER_DISABLE_PY_AST=1 forces the regex path, which doesn't
// emit Module symbols — IMPORTS stays unresolved. This is the negative
// baseline that proves the AST path is doing the resolution work.
func TestIndex_PythonImportsUnresolvedWithoutAST(t *testing.T) {
	if !ast.PythonAvailable() {
		t.Skip("python3 not on PATH")
	}
	t.Setenv("PINCHER_DISABLE_PY_AST", "1")

	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	writeFile(t, dir, "src/myproj/__init__.py", "")
	writeFile(t, dir, "src/myproj/config.py", `class ServerSpec:
    pass
`)
	writeFile(t, dir, "src/myproj/main.py", `from myproj.config import ServerSpec
`)

	if _, err := idx.Index(context.Background(), dir, true); err != nil {
		t.Fatalf("Index: %v", err)
	}
	projectID := db.ProjectIDFromPath(dir)

	// Regex extractor doesn't emit Module symbols → from-side has nothing
	// to resolve against → no IMPORTS edges land. This is the bug the AST
	// path closes; the test pins the baseline so we notice if regex starts
	// emitting Modules in the future.
	mods, err := store.GetSymbolsByQN(projectID, "src.myproj.main")
	if err != nil {
		t.Fatalf("GetSymbolsByQN: %v", err)
	}
	for _, m := range mods {
		if m.Kind == "Module" {
			t.Errorf("regex path should not emit Module symbols for Python, got %+v", m)
		}
	}
}
