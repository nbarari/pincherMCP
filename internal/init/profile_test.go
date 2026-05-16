package init

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #631 acceptance: pincher init on a Go repo prints AST-majority
// profile with high-tier claims.
func TestProfileDir_GoMajority(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(dir, "lib.go"), "package main\n")
	mustWrite(t, filepath.Join(dir, "config.yaml"), "key: value\n")

	p, err := ProfileDir(dir)
	if err != nil {
		t.Fatalf("ProfileDir: %v", err)
	}
	if p.HeadlineTier != tierAST {
		t.Errorf("headline tier = %q, want %s; profile=%s", p.HeadlineTier, tierAST, FormatProfile(p))
	}
	if !strings.Contains(p.HeadlineMessage, "AST-majority") {
		t.Errorf("headline message should call out AST-majority; got %q", p.HeadlineMessage)
	}
	// Per-language census should include Go and YAML.
	langs := map[string]int{}
	for _, ls := range p.Languages {
		langs[ls.Language] = ls.Files
	}
	if langs["Go"] != 2 {
		t.Errorf("Go files = %d, want 2; languages=%v", langs["Go"], langs)
	}
	if langs["YAML"] != 1 {
		t.Errorf("YAML files = %d, want 1", langs["YAML"])
	}
}

// #631 acceptance: stub-majority repo prints honest warning.
// v0.63 (#1186/#1187) promoted Lua/Elixir/Zig/Scala/Dart/R from
// stub-tier to regex-tier. Haskell is the only remaining stub-tier
// language (indentation-sensitive layout deferred — see #1161). Test
// now uses .hs so the assertion still exercises the stub-majority
// headline path; if Haskell promotes too, swap to whatever the last
// stub-tier language is at that point — or, when none remain, retire
// this test and pin the property "no stub-tier languages" elsewhere.
func TestProfileDir_StubMajority(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "Main.hs"), "module Main where\n")
	mustWrite(t, filepath.Join(dir, "Lib.hs"), "module Lib where\n")
	mustWrite(t, filepath.Join(dir, "Util.hs"), "module Util where\n")

	p, err := ProfileDir(dir)
	if err != nil {
		t.Fatalf("ProfileDir: %v", err)
	}
	if p.HeadlineTier != tierStub {
		t.Errorf("headline tier = %q, want %s; profile=%s", p.HeadlineTier, tierStub, FormatProfile(p))
	}
	out := FormatProfile(p)
	if !strings.Contains(out, "won't accelerate") {
		t.Errorf("stub profile should warn 'won't accelerate'; got %s", out)
	}
}

// #631 acceptance: mixed repo prints per-language breakdown.
// v0.63 (#1186/#1187) promoted Scala out of stub-tier; this test now
// uses Haskell (.hs) for the stub-tier slot so the "stub" assertion
// stays meaningful. See TestProfileDir_StubMajority for the broader
// stub-tier-language story.
func TestProfileDir_Mixed(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.go"), "package a\n")
	mustWrite(t, filepath.Join(dir, "b.py"), "x=1\n")
	mustWrite(t, filepath.Join(dir, "c.rb"), "x=1\n")
	mustWrite(t, filepath.Join(dir, "d.hs"), "module D where\n")

	p, err := ProfileDir(dir)
	if err != nil {
		t.Fatalf("ProfileDir: %v", err)
	}
	out := FormatProfile(p)
	for _, want := range []string{"Go", "Python", "Ruby", "Haskell", "AST", "stub"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatted profile missing %q; got:\n%s", want, out)
		}
	}
}

// Empty / non-source-file directory still emits a profile rather than
// silent no-op — silence trains the user to think init didn't do
// anything.
func TestProfileDir_NoIndexableFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "binary.bin"), "\x00\x01\x02")
	mustWrite(t, filepath.Join(dir, "image.jpg"), "\xff\xd8\xff")

	p, err := ProfileDir(dir)
	if err != nil {
		t.Fatalf("ProfileDir: %v", err)
	}
	if len(p.Languages) != 0 {
		t.Errorf("non-source-file dir should have no language stats; got %v", p.Languages)
	}
	out := FormatProfile(p)
	if !strings.Contains(out, "no files in indexable languages") {
		t.Errorf("empty-source profile should say so; got %s", out)
	}
}

// Skipped dirs (vendor, node_modules, .git) shouldn't dominate counts.
// gocodewalker also respects .gitignore so we don't have to fight it.
func TestProfileDir_ExcludesVendorAndNodeModules(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n")
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		mustWrite(t, filepath.Join(dir, "node_modules", "foo", fmt.Sprintf("x%d.js", i)), "var x=1;\n")
	}

	p, err := ProfileDir(dir)
	if err != nil {
		t.Fatalf("ProfileDir: %v", err)
	}
	for _, ls := range p.Languages {
		if ls.Language == "JavaScript" {
			t.Errorf("node_modules JS should be excluded; saw %d JS files", ls.Files)
		}
	}
}

// tierFor cutoffs match the README #621 vocabulary. Pin them so a
// future tweak to extractor confidence values doesn't silently
// reclassify a language without updating this table.
func TestTierFor_Cutoffs(t *testing.T) {
	cases := []struct {
		conf float64
		tier string
	}{
		{1.0, tierAST},
		{0.95, tierAST},
		{0.85, tierStableRegex},
		{0.80, tierStableRegex},
		{0.70, tierApproxRegex},
		{0.50, tierApproxRegex},
		{0.0, tierStub},
		{-1.0, tierUnsupported},
	}
	for _, c := range cases {
		got, _ := tierFor(c.conf)
		if got != c.tier {
			t.Errorf("tierFor(%.2f) = %s, want %s", c.conf, got, c.tier)
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
