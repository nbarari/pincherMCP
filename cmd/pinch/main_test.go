package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrintHelpBanner_ListsAllSubcommands pins the contract that
// `pincher --help` (which calls printHelpBanner) advertises every
// subcommand main.go dispatches to. If a future PR adds a subcommand
// without updating the banner, this test catches it — discoverability
// is the whole point of the banner.
func TestPrintHelpBanner_ListsAllSubcommands(t *testing.T) {
	var out bytes.Buffer
	printHelpBanner(&out)
	body := out.String()

	for _, sub := range []string{"index", "doctor", "self-test", "rebuild-fts", "stats", "hook-stats", "init", "project", "--version", "--http"} {
		if !strings.Contains(body, sub) {
			t.Errorf("banner missing subcommand mention %q:\n%s", sub, body)
		}
	}
	// The banner should also include the "Usage:" header so flag's
	// PrintDefaults output reads as the flag list rather than a continuation.
	if !strings.Contains(body, "Usage:") {
		t.Errorf("banner missing 'Usage:' header:\n%s", body)
	}
}

// TestIndexCLI_Binary_Plain exercises the runIndexCLI dispatch wrapper
// end-to-end against a synthetic project. With GOCOVERDIR set
// externally, the instrumented binary's coverage is folded into the
// merged profile.
func TestIndexCLI_Binary_Plain(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()

	// Synthetic project: 1 Go file with a known function so the indexer
	// emits at least one symbol and the success-line counts can be asserted.
	projDir := t.TempDir()
	projFile := filepath.Join(projDir, "main.go")
	if err := os.WriteFile(projFile, []byte("package main\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatalf("write project file: %v", err)
	}
	// `git init` so the indexer doesn't blame an unmanaged dir; the bloat
	// trap also requires a project marker for hook mode.
	if _, err := exec.LookPath("git"); err == nil {
		exec.Command("git", "-C", projDir, "init", "-q").Run()
	} else {
		// No git on PATH — write a fallback project marker (empty go.mod
		// satisfies the bloat-trap; standalone CLI mode skips the marker
		// check, so this is belt-and-suspenders).
		os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module test\n"), 0o644)
	}

	cmd := exec.Command(bin, "index", "--data-dir", dataDir, projDir)
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pincher index: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "indexed") {
		t.Errorf("expected 'indexed' banner; got:\n%s", got)
	}
}

// TestIndexCLI_Binary_JSONSummary asserts --json-summary emits valid
// JSON with the documented top-level keys (used by the corpus-snapshot
// pipeline).
func TestIndexCLI_Binary_JSONSummary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()
	projDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "x.go"), []byte("package test\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatalf("write x.go: %v", err)
	}

	cmd := exec.Command(bin, "index", "--data-dir", dataDir, "--json-summary", projDir)
	cmd.Env = pincherCoverEnv()
	// #1621 v0.85: stdout-only capture. Pre-fix this test used
	// CombinedOutput, but v0.85's #1613 observability legitimately
	// streams `pincher.*.summary` slog.Info lines to stderr — those
	// are diagnostic, not part of the --json-summary contract.
	// `--json-summary`'s stdout is a single JSON object; tests must
	// validate stdout-only.
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("pincher index --json-summary: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}

	var summary map[string]any
	if err := json.Unmarshal(out, &summary); err != nil {
		t.Fatalf("invalid JSON output: %v\n%s", err, out)
	}
	for _, key := range []string{"files_indexed", "schema_version", "symbol_count_by_kind"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("missing key %q in --json-summary output:\n%s", key, out)
		}
	}
}

// TestIndexCLI_Binary_Force exercises the --force code path through the
// dispatch wrapper so runIndexCLI's force=true branch in idx.Index gets
// coverage credit.
func TestIndexCLI_Binary_Force(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()
	projDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "x.go"), []byte("package test\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatalf("write x.go: %v", err)
	}

	// First run seeds the DB; second run with --force re-parses.
	for _, args := range [][]string{
		{"index", "--data-dir", dataDir, projDir},
		{"index", "--force", "--data-dir", dataDir, projDir},
	} {
		cmd := exec.Command(bin, args...)
		cmd.Env = pincherCoverEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pincher %v: %v\n%s", args, err, out)
		}
	}
}

// TestIndexCLI_Binary_BloatTrapManual asserts non-hook mode exits with a
// loud error (not 0) when refusing to index $HOME. Distinct from
// TestIndexCLI_Binary_BloatTrap which covers hook mode silent-exit.
func TestIndexCLI_Binary_BloatTrapManual(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()
	homeDir := t.TempDir()
	cmd := exec.Command(bin, "index", "--data-dir", dataDir, homeDir)
	env := pincherCoverEnv()
	env = append(env, "HOME="+homeDir, "USERPROFILE="+homeDir)
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit indexing $HOME; output: %s", out)
	}
	if !strings.Contains(string(out), "refusing to index") {
		t.Errorf("expected 'refusing to index' in stderr, got: %s", out)
	}
}

// TestVersionFlag exercises --version through the binary.
func TestVersionFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	cmd := exec.Command(bin, "--version")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pincher --version: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "pincherMCP v") {
		t.Errorf("expected 'pincherMCP v...' in --version output, got: %s", out)
	}
}

// #796: an unrecognized first arg (a typo'd subcommand) used to fall
// through to flag.Parse() and run the MCP stdio server, which on a
// non-tty stdin reads EOF and exits 0 — so `pincher doctr` looked like
// it silently succeeded. It now errors with the usage banner and
// exits non-zero.
func TestUnknownSubcommand_RejectedWithExit1(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	cmd := exec.Command(bin, "bogus-subcommand")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit for an unknown subcommand; got success\n%s", out)
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Errorf("expected exit code 1, got %v", err)
	}
	if !strings.Contains(string(out), "unknown subcommand") {
		t.Errorf("expected 'unknown subcommand' in output, got: %s", out)
	}
	if !strings.Contains(string(out), `"bogus-subcommand"`) {
		t.Errorf("error should name the offending token, got: %s", out)
	}
	// The usage banner must follow so the user sees the valid set.
	if !strings.Contains(string(out), "pincher doctor") {
		t.Errorf("expected the usage banner after the error, got: %s", out)
	}
}

// TestHelpFlag exercises the --help dispatch through the binary so the
// flag.Usage hook + os.Exit path get coverage credit. The banner content
// is pinned by TestPrintHelpBanner_ListsAllSubcommands above.
func TestHelpFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	cmd := exec.Command(bin, "--help")
	cmd.Env = pincherCoverEnv()
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "pincherMCP") {
		t.Errorf("expected banner in --help output, got: %s", out)
	}
}

// TestIndexCLI_Binary_BloatTrap asserts the bloat trap fires when
// indexing a directory whose absolute parent matches itself (Windows
// drive root). We can't easily test the actual root from a test, but
// we can confirm the trap path executes via a non-project dir in
// hook mode.
func TestIndexCLI_Binary_BloatTrap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()
	emptyDir := t.TempDir() // no project markers

	cmd := exec.Command(bin, "index", "--hook", "--data-dir", dataDir, emptyDir)
	cmd.Env = pincherCoverEnv()
	out, _ := cmd.CombinedOutput()
	// Hook mode exits 0 silently on a refused path so SessionStart
	// doesn't fail loudly; we just assert there's no "indexed" success
	// line (since indexing was refused).
	if strings.Contains(string(out), "indexed ") {
		t.Errorf("hook mode should not have indexed a non-project dir; got:\n%s", out)
	}
}
