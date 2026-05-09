package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPincherPolicyMarkdown_NotEmpty(t *testing.T) {
	if strings.TrimSpace(pincherPolicyMarkdown) == "" {
		t.Fatal("embedded pincher policy must not be empty")
	}
	if !strings.Contains(pincherPolicyMarkdown, "Pincher Usage Policy") {
		t.Fatal("embedded policy missing the 'Pincher Usage Policy' heading")
	}
}

func TestMergePolicyBlock_FromEmpty(t *testing.T) {
	out, action := mergePolicyBlock("", "## Test policy\n")
	if action != "wrote" {
		t.Errorf("action=%q, want 'wrote'", action)
	}
	if !strings.Contains(out, "# CLAUDE.md") {
		t.Error("expected new file to include the standard CLAUDE.md header")
	}
	if !strings.Contains(out, pincherInitMarkerStart) || !strings.Contains(out, pincherInitMarkerEnd) {
		t.Error("expected both markers in new file")
	}
	if !strings.Contains(out, "## Test policy") {
		t.Error("expected policy body in new file")
	}
}

func TestMergePolicyBlock_AppendToExisting(t *testing.T) {
	existing := "# Project rules\n\nFollow our internal docs.\n"
	out, action := mergePolicyBlock(existing, "## Pincher\n")
	if action != "appended" {
		t.Errorf("action=%q, want 'appended'", action)
	}
	if !strings.Contains(out, "Project rules") {
		t.Error("existing content should be preserved")
	}
	if !strings.Contains(out, pincherInitMarkerStart) {
		t.Error("expected start marker")
	}
	// Existing content should come before the markers.
	startIdx := strings.Index(out, pincherInitMarkerStart)
	rulesIdx := strings.Index(out, "Project rules")
	if rulesIdx >= startIdx {
		t.Error("existing content should appear before the pincher block")
	}
}

func TestMergePolicyBlock_ReplaceExistingBlock(t *testing.T) {
	existing := "# CLAUDE.md\n\nIntro.\n\n" +
		pincherInitMarkerStart + "\n" +
		"OLD CONTENT THAT SHOULD GET REPLACED\n" +
		pincherInitMarkerEnd + "\n\n" +
		"Trailing text.\n"
	out, action := mergePolicyBlock(existing, "## NEW POLICY\n")
	if action != "updated" {
		t.Errorf("action=%q, want 'updated'", action)
	}
	if strings.Contains(out, "OLD CONTENT") {
		t.Error("old content should be removed")
	}
	if !strings.Contains(out, "## NEW POLICY") {
		t.Error("new content should be present")
	}
	if !strings.Contains(out, "Intro.") {
		t.Error("content before block should survive")
	}
	if !strings.Contains(out, "Trailing text.") {
		t.Error("content after block should survive")
	}
	// Should still have one marker pair (not two from accidental duplication).
	if strings.Count(out, pincherInitMarkerStart) != 1 || strings.Count(out, pincherInitMarkerEnd) != 1 {
		t.Errorf("expected exactly one marker pair, got start=%d end=%d",
			strings.Count(out, pincherInitMarkerStart), strings.Count(out, pincherInitMarkerEnd))
	}
}

func TestMergePolicyBlock_Idempotent(t *testing.T) {
	policy := "## Pincher\nuse pincher.\n"
	first, _ := mergePolicyBlock("", policy)
	second, action := mergePolicyBlock(first, policy)
	if action != "updated" {
		t.Errorf("re-run action=%q, want 'updated'", action)
	}
	if first != second {
		t.Errorf("re-running with same input should produce identical output\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestMergePolicyBlock_OnlyStartMarker_AppendsNewBlock(t *testing.T) {
	// Malformed: only the start marker present. We refuse to "guess" where
	// the user meant the block to end and just append a fresh block.
	existing := "# Project\n\n" + pincherInitMarkerStart + "\nbroken\n"
	out, action := mergePolicyBlock(existing, "## Body\n")
	if action != "appended" {
		t.Errorf("action=%q, want 'appended' (malformed → append safely)", action)
	}
	if !strings.Contains(out, "broken") {
		t.Error("the original malformed text should be preserved (we don't auto-recover)")
	}
}

func TestResolveCLAUDEPath_ProjectDefault(t *testing.T) {
	tmp := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer os.Chdir(cwd)
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got, err := resolveCLAUDEPath(false)
	if err != nil {
		t.Fatalf("resolveCLAUDEPath: %v", err)
	}
	want := filepath.Join(tmp, "CLAUDE.md")
	// Use EvalSymlinks because tmpdir on macOS is /var/folders/... which
	// resolves to /private/var/folders/...
	gotResolved, _ := filepath.EvalSymlinks(filepath.Dir(got))
	wantResolved, _ := filepath.EvalSymlinks(filepath.Dir(want))
	if gotResolved != wantResolved {
		t.Errorf("got dir %q, want dir %q", gotResolved, wantResolved)
	}
	if filepath.Base(got) != "CLAUDE.md" {
		t.Errorf("got base %q, want CLAUDE.md", filepath.Base(got))
	}
}

func TestResolveCLAUDEPath_Global(t *testing.T) {
	got, err := resolveCLAUDEPath(true)
	if err != nil {
		t.Fatalf("resolveCLAUDEPath(true): %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join(".claude", "CLAUDE.md")) {
		t.Errorf("global path %q should end with .claude/CLAUDE.md", got)
	}
}

func TestInitCLI_Binary_DryRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)

	workdir := t.TempDir()
	cmd := exec.Command(bin, "init", "--dry-run")
	cmd.Dir = workdir
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pincher init --dry-run: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "would wrote") && !strings.Contains(got, "would write") {
		// Accept either "would wrote" (action verb passes through literal)
		// or "would write" (more grammatical) — we use "would <action>"
		// in the implementation, where action is "wrote", so the literal
		// message is "would wrote" today. Keep the test tolerant either way.
		t.Errorf("expected dry-run preface; got:\n%s", got)
	}
	if !strings.Contains(got, pincherInitMarkerStart) {
		t.Errorf("dry-run output should include start marker; got:\n%s", got)
	}
	// Confirm nothing was written.
	if _, err := os.Stat(filepath.Join(workdir, "CLAUDE.md")); err == nil {
		t.Error("dry-run should not create CLAUDE.md, but it exists")
	}
}

func TestInitCLI_Binary_WriteThenIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)

	workdir := t.TempDir()
	cmd := exec.Command(bin, "init", "--data-dir", t.TempDir())
	cmd.Dir = workdir
	cmd.Env = pincherCoverEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("first init: %v\n%s", err, out)
	}

	target := filepath.Join(workdir, "CLAUDE.md")
	first, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	if !strings.Contains(string(first), pincherInitMarkerStart) {
		t.Fatalf("first init didn't write the marker block:\n%s", first)
	}

	// Second init should produce identical output (idempotent).
	cmd2 := exec.Command(bin, "init", "--data-dir", t.TempDir())
	cmd2.Dir = workdir
	cmd2.Env = pincherCoverEnv()
	if out, err := cmd2.CombinedOutput(); err != nil {
		t.Fatalf("second init: %v\n%s", err, out)
	}
	second, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read CLAUDE.md after re-init: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("re-running init should be idempotent\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}

	// Marker block should appear exactly once.
	startCount := strings.Count(string(second), pincherInitMarkerStart)
	endCount := strings.Count(string(second), pincherInitMarkerEnd)
	if startCount != 1 || endCount != 1 {
		t.Errorf("expected exactly one marker pair, got start=%d end=%d", startCount, endCount)
	}
}

func TestInitCLI_Binary_PreservesExistingContent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)

	workdir := t.TempDir()
	target := filepath.Join(workdir, "CLAUDE.md")
	pre := "# CLAUDE.md\n\nProject-specific guidance:\n- Use snake_case for filenames.\n- Tests live in tests/.\n"
	if err := os.WriteFile(target, []byte(pre), 0o644); err != nil {
		t.Fatalf("seed CLAUDE.md: %v", err)
	}

	cmd := exec.Command(bin, "init", "--data-dir", t.TempDir())
	cmd.Dir = workdir
	cmd.Env = pincherCoverEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read post-init: %v", err)
	}
	gotStr := string(got)
	if !strings.Contains(gotStr, "Project-specific guidance:") {
		t.Error("existing content was lost")
	}
	if !strings.Contains(gotStr, "snake_case") {
		t.Error("existing bullet was lost")
	}
	if !strings.Contains(gotStr, pincherInitMarkerStart) {
		t.Error("pincher block missing")
	}
}

// ── --target dispatch via the binary ─────────────────────────────────────────

// TestInitCLI_Binary_TargetCursor walks the runInitCLI → resolveTargets
// → runInitTarget(cursor) path through the cover-instrumented binary so
// the dispatch wrapper picks up coverage credit.
func TestInitCLI_Binary_TargetCursor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	workdir := t.TempDir()
	cmd := exec.Command(bin, "init", "--target", "cursor", "--data-dir", t.TempDir())
	cmd.Dir = workdir
	cmd.Env = pincherCoverEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pincher init --target=cursor: %v\n%s", err, out)
	}
	want := filepath.Join(workdir, ".cursor", "rules", "pincher.mdc")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", want, err)
	}
	if !strings.HasPrefix(string(got), "---\n") {
		t.Errorf("cursor target should start with frontmatter delimiter; got: %s", got[:50])
	}
}

// TestInitCLI_Binary_TargetAll asserts --target=all writes every
// project-scoped target in one invocation.
func TestInitCLI_Binary_TargetAll(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	workdir := t.TempDir()
	homeDir := t.TempDir()
	cmd := exec.Command(bin, "init", "--target", "all", "--data-dir", t.TempDir())
	cmd.Dir = workdir
	env := pincherCoverEnv()
	env = append(env, "HOME="+homeDir, "USERPROFILE="+homeDir)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pincher init --target=all: %v\n%s", err, out)
	}
	for _, sub := range []string{
		filepath.Join(workdir, "CLAUDE.md"),
		filepath.Join(workdir, ".cursor", "rules", "pincher.mdc"),
		filepath.Join(workdir, ".cursorrules"),
		filepath.Join(workdir, ".windsurfrules"),
		filepath.Join(workdir, "CONVENTIONS.md"),
		filepath.Join(homeDir, ".continue", "config.json"),
	} {
		if _, err := os.Stat(sub); err != nil {
			t.Errorf("expected %s to exist after --target=all: %v", sub, err)
		}
	}
}

// TestInitCLI_Binary_TargetDetect verifies the --target=detect path
// exits cleanly and writes only to detected targets.
func TestInitCLI_Binary_TargetDetect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	workdir := t.TempDir()
	homeDir := t.TempDir()
	// Seed only the windsurf marker so detection should pick exactly that.
	if err := os.WriteFile(filepath.Join(workdir, ".windsurfrules"), []byte("# old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "init", "--target", "detect", "--data-dir", t.TempDir())
	cmd.Dir = workdir
	env := pincherCoverEnv()
	env = append(env, "HOME="+homeDir, "USERPROFILE="+homeDir)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pincher init --target=detect: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(workdir, ".windsurfrules"))
	if err != nil {
		t.Fatalf("windsurfrules should exist: %v", err)
	}
	if !strings.Contains(string(got), pincherInitMarkerStart) {
		t.Error("expected pincher block in .windsurfrules")
	}
	// CLAUDE.md should NOT be written (not detected).
	if _, err := os.Stat(filepath.Join(workdir, "CLAUDE.md")); err == nil {
		t.Error("CLAUDE.md should not be written when only windsurf was detected")
	}
}

// TestInitCLI_Binary_UnknownTargetExits asserts an unknown --target
// value produces a non-zero exit and a useful error.
func TestInitCLI_Binary_UnknownTargetExits(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	cmd := exec.Command(bin, "init", "--target", "vim", "--dry-run")
	cmd.Dir = t.TempDir()
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit on unknown --target; got output: %s", out)
	}
	if !strings.Contains(string(out), "unknown --target") {
		t.Errorf("expected 'unknown --target' message; got: %s", out)
	}
}
