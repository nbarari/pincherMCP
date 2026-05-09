package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// Smoke test for the self-test runtime steps. Mirrors `pincher self-test`
// but runs in-process so we can catch regressions in the harness itself
// before they ship as silent self-test failures.
func TestSelfTestSteps_AllPass(t *testing.T) {
	rt := &selfTestRuntime{dataDir: t.TempDir()}
	t.Cleanup(func() {
		if rt.store != nil {
			_ = rt.store.Close()
		}
		if rt.projectDir != "" {
			_ = os.RemoveAll(rt.projectDir)
		}
	})

	steps := []selfTestStep{
		{"open database", openDB},
		{"create synthetic project", createSynthetic},
		{"index the project", indexSynthetic},
		{"search for known symbol", searchSynthetic},
		{"retrieve symbol source", retrieveSynthetic},
	}
	for _, step := range steps {
		if err := step.fn(rt); err != nil {
			t.Fatalf("step %q failed: %v", step.label, err)
		}
	}

	// Post-conditions: rt should carry state forward as each step runs.
	if rt.store == nil {
		t.Error("openDB should populate rt.store")
	}
	if rt.indexer == nil {
		t.Error("openDB should populate rt.indexer")
	}
	if rt.projectDir == "" {
		t.Error("createSynthetic should set rt.projectDir")
	}
	if rt.projectID == "" {
		t.Error("indexSynthetic should set rt.projectID")
	}
	if rt.symbolID == "" {
		t.Error("searchSynthetic should set rt.symbolID")
	}
}

// TestRunSelfTest_HappyPath exercises the full runSelfTest entrypoint
// (the part runSelfTestCLI calls), captures stderr, asserts exit=0 and
// the all-OK summary line.
func TestRunSelfTest_HappyPath(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	exitCode := runSelfTest([]string{"--data-dir", dir}, &out)
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0; output:\n%s", exitCode, out.String())
	}
	if !strings.Contains(out.String(), "self-test: OK") {
		t.Errorf("missing OK summary; output:\n%s", out.String())
	}
	// Each of the 5 step labels should appear with OK.
	for _, label := range []string{"1/5", "2/5", "3/5", "4/5", "5/5"} {
		if !strings.Contains(out.String(), label) {
			t.Errorf("step %s missing from output:\n%s", label, out.String())
		}
	}
}

// TestRunSelfTest_FailPathReturnsNonZero forces the open-database step
// to fail (parent path is a regular file, not a directory) and asserts
// runSelfTest reports it loudly + exits non-zero.
func TestRunSelfTest_FailPathReturnsNonZero(t *testing.T) {
	// Setup: create a file, then point --data-dir at a path UNDER it.
	parent := t.TempDir()
	notADir := parent + string(os.PathSeparator) + "i-am-a-file"
	if err := os.WriteFile(notADir, []byte("file"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	bogus := notADir + string(os.PathSeparator) + "child"

	var out bytes.Buffer
	exitCode := runSelfTest([]string{"--data-dir", bogus}, &out)
	if exitCode != 1 {
		t.Errorf("expected exit=1 on bad data-dir, got %d; output:\n%s", exitCode, out.String())
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Errorf("output should contain FAIL marker; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "self-test: FAIL") {
		t.Errorf("output should contain final summary line; got:\n%s", out.String())
	}
}

func TestRunSelfTest_VerboseShowsTimings(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	exitCode := runSelfTest([]string{"--data-dir", dir, "--verbose"}, &out)
	if exitCode != 0 {
		t.Fatalf("exit = %d; output:\n%s", exitCode, out.String())
	}
	// Verbose mode should print the (Nms) timing column.
	if !strings.Contains(out.String(), "ms)") {
		t.Errorf("verbose mode should include timings; output:\n%s", out.String())
	}
}

// TestSelfTestStep_SearchFailsOnEmptyIndex ensures the search step fails
// loudly when there's nothing to find — catches a future indexer regression
// that silently produces 0 symbols (the symptom self-test exists to surface).
func TestSelfTestStep_SearchFailsOnEmptyIndex(t *testing.T) {
	rt := &selfTestRuntime{dataDir: t.TempDir()}
	t.Cleanup(func() {
		if rt.store != nil {
			_ = rt.store.Close()
		}
	})
	if err := openDB(rt); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	rt.projectID = "nonexistent-project"

	if err := searchSynthetic(rt); err == nil {
		t.Error("search should fail when project has no indexed symbols")
	}
}
