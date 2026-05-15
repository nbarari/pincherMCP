package init

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #658: Zed init target wave 1. `.rules` plain markdown at project
// root (per Zed AI rules docs). Marker-block convention handles
// safe coexistence with other tools that may also write to `.rules`.

func TestZed_FreshWriteCreatesRulesFile(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	path, err := ZedTarget.PathFn(cwd, false)
	if err != nil {
		t.Fatalf("PathFn: %v", err)
	}
	if got, want := path, filepath.Join(cwd, ".rules"); got != want {
		t.Errorf("PathFn = %q, want %q", got, want)
	}

	out, action := ZedTarget.WriteFn("", samplePolicy)
	if action != "wrote" {
		t.Errorf("action = %q, want wrote", action)
	}
	if !strings.Contains(out, MarkerStart) || !strings.Contains(out, MarkerEnd) {
		t.Error("expected pincher marker block in fresh write")
	}
	if !strings.Contains(out, samplePolicy) {
		t.Error("expected policy content in fresh write")
	}
}

// Idempotent re-run replaces the marker block in place rather than
// duplicating — matches the codex/claude/cursor pattern.
func TestZed_ReplaceUpdatesMarkerBlockInPlace(t *testing.T) {
	t.Parallel()
	prior, _ := ZedTarget.WriteFn("", samplePolicy)
	updated, action := ZedTarget.WriteFn(prior, samplePolicy+"\n\nNew clause.")
	if action != "updated" {
		t.Errorf("action = %q, want updated", action)
	}
	// Marker pair appears exactly once after replace.
	if got := strings.Count(updated, MarkerStart); got != 1 {
		t.Errorf("MarkerStart count = %d, want 1", got)
	}
	if got := strings.Count(updated, MarkerEnd); got != 1 {
		t.Errorf("MarkerEnd count = %d, want 1", got)
	}
	if !strings.Contains(updated, "New clause.") {
		t.Error("updated block should contain new policy text")
	}
}

// Unmanaged content above/below the marker block is preserved on
// re-run — the user's hand-edited rules survive.
func TestZed_PreservesUnmanagedContent(t *testing.T) {
	t.Parallel()
	preamble := "# My project rules\n\n- Be terse.\n\n"
	postscript := "\n\n## Custom additions\n- Pin every test.\n"
	existing := preamble
	managed, _ := ZedTarget.WriteFn("", samplePolicy)
	existing += managed
	existing += postscript

	updated, _ := ZedTarget.WriteFn(existing, samplePolicy)
	if !strings.Contains(updated, "Be terse.") {
		t.Error("preamble lost on re-run")
	}
	if !strings.Contains(updated, "Pin every test.") {
		t.Error("postscript lost on re-run")
	}
}

// DetectFn returns true when `.zed/` directory exists OR `.rules`
// file exists at cwd. Either is a Zed signal.
func TestZed_DetectFn_MatchesZedDirectoryOrRulesFile(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	if ZedTarget.DetectFn(cwd) {
		t.Errorf("empty dir should not detect Zed")
	}

	// `.zed/` directory case.
	if err := os.Mkdir(filepath.Join(cwd, ".zed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !ZedTarget.DetectFn(cwd) {
		t.Error("expected detect to fire on .zed/ directory presence")
	}

	// `.rules` file case.
	cwd2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd2, ".rules"), []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ZedTarget.DetectFn(cwd2) {
		t.Error("expected detect to fire on .rules file presence")
	}
}

func TestZed_GlobalPathResolvesUnderConfigDir(t *testing.T) {
	// Cannot t.Parallel() — withHome uses t.Setenv which forbids
	// parallel tests.
	home := t.TempDir()
	withHome(t, home)

	path, err := ZedTarget.PathFn(t.TempDir(), true)
	if err != nil {
		t.Fatalf("PathFn global: %v", err)
	}
	want := filepath.Join(home, ".config", "zed", ".rules")
	if path != want {
		t.Errorf("global PathFn = %q, want %q", path, want)
	}
}
