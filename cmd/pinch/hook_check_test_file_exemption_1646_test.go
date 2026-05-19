package main

import (
	"path/filepath"
	"testing"
)

// #1646 v0.86: PreToolUse hook must pass through Read calls on test
// source files. The hook's "redirect to context lite=true" advice is
// unhelpful when the agent is about to Edit the file — the lite-mode
// envelope drops the byte content, forcing a follow-up Read. Test
// files are written by hand and frequently Edit-ed, so the hook
// blocking them on every read cycle compounds across every test fix.
//
// These tests pin the isTestFile contract across the languages we
// support. A blocked test file is the painful UX path; a passed-
// through non-test file is a tiny correctness loss the agent absorbs.

func TestIsTestFile_Matrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
		why  string
	}{
		// Go
		{"hook_check_test.go", true, "Go _test.go"},
		{"foo_bar_test.go", true, "Go _test.go with underscores"},
		{"hook_check.go", false, "Go non-test"},
		{"server.go", false, "Go non-test"},
		{"internal/server/server_test.go", true, "Go _test.go nested"},

		// Python
		{"test_models.py", true, "Python test_*.py"},
		{"models_test.py", true, "Python *_test.py"},
		{"models.py", false, "Python non-test"},

		// JS / TS
		{"app.test.js", true, "JS .test.js"},
		{"app.test.tsx", true, "TS .test.tsx"},
		{"app.spec.ts", true, "TS .spec.ts"},
		{"app.spec.mjs", true, "JS .spec.mjs"},
		{"app.js", false, "JS non-test"},
		{"app.ts", false, "TS non-test"},

		// Ruby
		{"models_spec.rb", true, "Ruby _spec.rb"},
		{"models_test.rb", true, "Ruby _test.rb"},
		{"models.rb", false, "Ruby non-test"},

		// Java
		{"ModelsTest.java", true, "Java *Test.java"},
		{"ModelsTests.java", true, "Java *Tests.java"},
		{"ModelsIT.java", true, "Java *IT.java integration"},
		{"Models.java", false, "Java non-test"},

		// Swift / Kotlin / C#
		{"ModelsTests.swift", true, "Swift *Tests.swift"},
		{"ModelsTest.kt", true, "Kotlin *Test.kt"},
		{"ModelsTests.cs", true, "C# *Tests.cs"},
		{"Models.swift", false, "Swift non-test"},
		{"Models.kt", false, "Kotlin non-test"},
		{"Models.cs", false, "C# non-test"},

		// PHP
		{"ModelsTest.php", true, "PHP *Test.php"},
		{"Models.php", false, "PHP non-test"},

		// Directory-segment match
		{"src/tests/something.go", true, "tests/ directory"},
		{"src/__tests__/something.js", true, "__tests__/ directory"},
		{"src/test/something.java", true, "test/ directory"},
		{"src/spec/something.rb", true, "spec/ directory"},
		{"src/e2e/something.ts", true, "e2e/ directory"},
		{"src/it/something.kt", true, "it/ directory"},

		// Edge cases
		{"", false, "empty path"},
		{"testing.go", false, "file named `testing.go` is NOT a test file"},
		{"latest.go", false, "`latest.go` substring near `test` must not match"},
		{"src/contests/leaderboard.go", false, "`contests/` must not match `tests/`"},

		// Windows separators normalize
		{`internal\server\server_test.go`, true, "Windows backslash separators"},
	}
	for _, c := range cases {
		if got := isTestFile(c.path); got != c.want {
			t.Errorf("isTestFile(%q) = %v, want %v (%s)", c.path, got, c.want, c.why)
		}
	}
}

// decideReadHook integration: a test file in an indexed project passes
// through even when the file is large enough to ordinarily trigger the
// redirect. Mirrors the existing TestDecideHook_Read_*_PassesThrough
// shape from hook_check_test.go.
func TestDecideReadHook_TestFile_PassesThrough_1646(t *testing.T) {
	t.Parallel()
	store := newHookTestStore(t)
	projectDir := t.TempDir()
	// A 50 KB Go test file would otherwise trip the redirect.
	relPath := "internal/server/something_test.go"
	indexLargeFakeFile(t, store, projectDir, relPath, 50_000)

	in := hookCheckInput{
		ToolName: "Read",
		ToolInput: map[string]any{
			"file_path": filepath.Join(projectDir, relPath),
		},
	}
	d := decideReadHook(store, in, false)
	if !d.Continue {
		t.Fatalf("test file must pass through even when large; got Continue=%v Decision=%q StopReason=%q",
			d.Continue, d.Decision, d.StopReason)
	}
	if d.Decision != "pass_through" {
		t.Errorf("expected Decision=\"pass_through\"; got %q", d.Decision)
	}
}

// Control: a production .go file at the same size DOES still redirect.
// The exemption must be specific to test files, not a blanket
// pass-through. Without this cross-check, a regression that always
// passed through would silently disable the hook.
func TestDecideReadHook_ProductionFile_StillRedirects_1646(t *testing.T) {
	t.Parallel()
	store := newHookTestStore(t)
	projectDir := t.TempDir()
	relPath := "internal/server/something.go"
	indexLargeFakeFile(t, store, projectDir, relPath, 50_000)

	in := hookCheckInput{
		ToolName: "Read",
		ToolInput: map[string]any{
			"file_path": filepath.Join(projectDir, relPath),
		},
	}
	d := decideReadHook(store, in, false)
	if d.Continue {
		t.Fatalf("production file must still redirect; got Continue=true (regression — exemption is too broad)")
	}
	if d.Decision != "redirect" {
		t.Errorf("expected Decision=\"redirect\"; got %q", d.Decision)
	}
}
