package ast

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	abs := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestPythonSourceRoots_FlatLayout(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "myproj/__init__.py", "")
	writeFile(t, dir, "myproj/foo.py", "")

	got := PythonSourceRoots(dir)
	if !containsStr(got, "") {
		t.Errorf("flat layout should yield empty root in result; got %v", got)
	}
}

func TestPythonSourceRoots_SrcLayout(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/myproj/__init__.py", "")
	writeFile(t, dir, "src/myproj/foo.py", "")

	got := PythonSourceRoots(dir)
	if !containsStr(got, "src") {
		t.Errorf("src layout should yield \"src\" root; got %v", got)
	}
	if !containsStr(got, "") {
		t.Errorf("identity \"\" should always be present; got %v", got)
	}
}

func TestPythonSourceRoots_PyprojectSetuptoolsSrc(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pyproject.toml", `
[tool.setuptools.packages.find]
where = ["src"]
`)
	// No __init__.py — config alone should drive the result.
	got := PythonSourceRoots(dir)
	if !containsStr(got, "src") {
		t.Errorf("setuptools where=src should yield \"src\" root; got %v", got)
	}
}

func TestPythonSourceRoots_PyprojectPoetry(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pyproject.toml", `
[tool.poetry]
name = "x"
version = "0"
packages = [{include = "x", from = "src"}, {include = "y", from = "lib"}]
`)
	got := PythonSourceRoots(dir)
	if !containsStr(got, "src") || !containsStr(got, "lib") {
		t.Errorf("poetry packages with from=src,lib should yield both; got %v", got)
	}
}

func TestPythonSourceRoots_PyprojectHatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pyproject.toml", `
[tool.hatch.build.targets.wheel]
packages = ["src/myproj"]
`)
	got := PythonSourceRoots(dir)
	if !containsStr(got, "src") {
		t.Errorf("hatch packages=[\"src/myproj\"] should yield \"src\"; got %v", got)
	}
}

func TestPythonSourceRoots_SetupPy(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "setup.py", `
from setuptools import setup, find_packages
setup(name="x", package_dir={"": "src"}, packages=find_packages("src"))
`)
	got := PythonSourceRoots(dir)
	if !containsStr(got, "src") {
		t.Errorf("setup.py with package_dir={'': 'src'} should yield \"src\"; got %v", got)
	}
}

func TestPythonSourceRoots_SetupCfg(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "setup.cfg", `
[options.packages.find]
where = src
`)
	got := PythonSourceRoots(dir)
	if !containsStr(got, "src") {
		t.Errorf("setup.cfg where=src should yield \"src\"; got %v", got)
	}
}

func TestPythonSourceRoots_SkipsCommonIgnoredDirs(t *testing.T) {
	dir := t.TempDir()
	// .venv contents shouldn't pollute roots
	writeFile(t, dir, ".venv/lib/python3.11/site-packages/pkg/__init__.py", "")
	writeFile(t, dir, "myproj/__init__.py", "")

	got := PythonSourceRoots(dir)
	for _, r := range got {
		if r == ".venv" || r == ".venv/lib/python3.11/site-packages" {
			t.Errorf("source root walk should skip .venv; got %v", got)
		}
	}
}

func TestPythonSourceRoots_DeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/a/__init__.py", "")
	writeFile(t, dir, "lib/b/__init__.py", "")

	got1 := PythonSourceRoots(dir)
	got2 := PythonSourceRoots(dir)
	if !sort.StringsAreSorted(got1) && len(got1) > 1 {
		// We don't require lexical sort, but order must be stable across calls.
	}
	// Identical inputs must yield identical output ordering.
	if len(got1) != len(got2) {
		t.Fatalf("length mismatch: %v vs %v", got1, got2)
	}
	for i := range got1 {
		if got1[i] != got2[i] {
			t.Errorf("nondeterministic order at index %d: %v vs %v", i, got1, got2)
		}
	}
}

func TestPythonSourceRoots_AlwaysIncludesIdentity(t *testing.T) {
	// Even on a totally empty project we should get the "" identity so the
	// resolver's identity candidate still works.
	dir := t.TempDir()
	got := PythonSourceRoots(dir)
	if !containsStr(got, "") {
		t.Errorf("empty project must still include \"\"; got %v", got)
	}
}
