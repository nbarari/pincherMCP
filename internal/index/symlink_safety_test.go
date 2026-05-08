package index

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pincherMCP/pincher/internal/db"
)

// symlink_safety_test pins gocodewalker's symlink behaviour as the
// security contract pincher relies on (#41 item 3).
//
// Audit finding: gocodewalker (v1.5.1) does NOT recurse through symlinked
// directories — it reports the symlink itself as a path and stops. The
// indexer's per-file ReadFile call then fails with "is a directory" on
// any directory symlink, so files inside an external directory pointed
// at by a project-internal symlink are never indexed.
//
// This test pins that behaviour. A future gocodewalker upgrade that
// silently changes the default to "follow symlinks" would walk a project
// containing a `vendor/external -> /etc` symlink straight into /etc.
// These tests fail loud at upgrade time so the trust model can be
// re-evaluated rather than silently degraded.
//
// Threat model the gates:
//
//   1. Symlink-to-directory inside the project pointing OUTSIDE the
//      project root — MUST NOT be walked-into. The symlink is a leaf.
//   2. Symlink loops (a -> b -> a) — MUST NOT cause infinite recursion.
//   3. Symlinks resolved during the user-supplied root path itself
//      (`pincher index ~/myproject` where myproject is a symlink) —
//      filepath.Abs alone does NOT resolve symlinks; the bloat-trap
//      EvalSymlinks before refusing $HOME / filesystem root.

func TestSymlinkSafety_DirectorySymlinkNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Symlinks on Windows require admin/dev mode; skip rather
		// than skew CI on a less-relevant platform for this audit.
		t.Skip("symlink tests skipped on Windows")
	}

	idx, store := newTestIndexer(t)
	project := t.TempDir()
	target := t.TempDir()

	// Setup:
	//   <project>/inside.go            (real file inside project)
	//   <project>/escape -> <target>   (symlink to a directory outside)
	//   <target>/escaped_secret.go     (file the symlink points at)
	if err := os.WriteFile(filepath.Join(project, "inside.go"),
		[]byte("package x\nfunc Inside() {}\n"), 0o644); err != nil {
		t.Fatalf("write inside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "escaped_secret.go"),
		[]byte("package y\nfunc Escaped() {}\n"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(project, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := idx.Index(context.Background(), project, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	pid := db.ProjectIDFromPath(project)

	// Positive: real file inside the project IS indexed.
	insideSyms, err := store.GetSymbolsByName(pid, "Inside", 5)
	if err != nil || len(insideSyms) == 0 {
		t.Errorf("expected Inside() indexed, got err=%v len=%d", err, len(insideSyms))
	}

	// SECURITY NEGATIVE: Escaped() from outside the project root MUST NOT
	// have been indexed. The symlinked-directory contents are not walked.
	escapedSyms, err := store.GetSymbolsByName(pid, "Escaped", 5)
	if err != nil {
		t.Fatalf("GetSymbolsByName(Escaped): %v", err)
	}
	if len(escapedSyms) != 0 {
		t.Errorf("symlink escape: %d symbols from outside project were indexed; "+
			"gocodewalker followed the directory symlink. Expected 0.", len(escapedSyms))
		for _, s := range escapedSyms {
			t.Logf("  leaked symbol: %s @ %s", s.QualifiedName, s.FilePath)
		}
	}
}

func TestSymlinkSafety_LoopDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests skipped on Windows")
	}

	idx, _ := newTestIndexer(t)
	project := t.TempDir()

	// Setup: a -> b -> a. If gocodewalker followed symlinks, this would
	// loop forever. Since it doesn't, indexing completes promptly.
	a := filepath.Join(project, "a")
	b := filepath.Join(project, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatalf("symlink a->b: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatalf("symlink b->a: %v", err)
	}
	// Plus a real file so Index has something to walk.
	if err := os.WriteFile(filepath.Join(project, "real.go"),
		[]byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write real.go: %v", err)
	}

	// MUST complete (the test framework's own timeout will fire if not).
	if _, err := idx.Index(context.Background(), project, false); err != nil {
		t.Fatalf("Index hit a real error (not a loop): %v", err)
	}
}

func TestSymlinkSafety_SymlinkToFileInsideProject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests skipped on Windows")
	}

	// A symlink to a regular file INSIDE the project is allowed and
	// indexed (this is a legitimate developer pattern — e.g.,
	// pre-commit-config.yaml linked into a subdirectory).
	idx, store := newTestIndexer(t)
	project := t.TempDir()

	if err := os.WriteFile(filepath.Join(project, "real.go"),
		[]byte("package x\nfunc Real() {}\n"), 0o644); err != nil {
		t.Fatalf("write real: %v", err)
	}
	if err := os.Symlink("real.go", filepath.Join(project, "alias.go")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := idx.Index(context.Background(), project, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	pid := db.ProjectIDFromPath(project)

	// Real() should be present (and only once at minimum).
	syms, err := store.GetSymbolsByName(pid, "Real", 10)
	if err != nil || len(syms) == 0 {
		t.Errorf("expected Real() indexed, got err=%v len=%d", err, len(syms))
	}
}
