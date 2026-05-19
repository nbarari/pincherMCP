package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1667 v0.87: branch-drift advisory must skip projects whose
// on-disk path no longer exists. Those are dead-path ghosts —
// `list prune_dead=true` is the right remediation, not
// `pincher index`. Without this, the advisory keeps firing on
// rows that can't be fixed by the suggested action.
//
// #1669 v0.87: extended to also skip paths that exist but aren't
// directories. Caught on the dogfood machine where a `probe`
// project pointed at a 3.6 MB executable file; git -C resolved
// via the parent .git and false-positively reported drift.

func TestBranchDriftAdvisory_SkipsDeadPath_1667(t *testing.T) {
	t.Parallel()
	// Two projects: one whose on-disk path exists (a real temp dir,
	// not a git repo — git rev-parse will fail there, so it falls
	// through naturally), one whose path has been deleted out from
	// under us. The dead-path one must drop silently so the
	// advisory has no record of it.
	live := t.TempDir() // exists, but not a git repo
	dead := filepath.Join(t.TempDir(), "doesnt-exist-anymore")

	// Sanity: dead path should not be stat-able.
	if _, err := os.Stat(dead); err == nil {
		t.Fatalf("dead path %q exists; test setup wrong", dead)
	}

	projects := []db.Project{
		{ID: "p-live", Path: live, Name: "live-but-not-git", CurrentBranch: "old-branch"},
		{ID: "p-dead", Path: dead, Name: "dead-ghost", CurrentBranch: "old-branch"},
	}
	got := branchDriftAdvisory(projects)
	// The dead-ghost project must never appear in the message,
	// regardless of whether the live one fell through.
	if strings.Contains(got, "dead-ghost") {
		t.Errorf("dead-path project must not appear in advisory; got: %s", got)
	}
}

// #1669 v0.87: branch-drift advisory must skip projects whose path
// points to a file, not a directory. Found on the dogfood machine
// where a `probe` project's directory had been replaced by a
// same-named executable; git -C "<path-to-file>" resolved via the
// parent's .git and false-positively reported drift.
func TestBranchDriftAdvisory_SkipsPathPointingAtFile_1669(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "i-am-a-file-not-a-dir.bin")
	if err := os.WriteFile(filePath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	projects := []db.Project{
		{ID: "p-file", Path: filePath, Name: "file-ghost", CurrentBranch: "old-branch"},
	}
	got := branchDriftAdvisory(projects)
	if strings.Contains(got, "file-ghost") {
		t.Errorf("project whose path is a file (not dir) must not appear in advisory; got: %s", got)
	}
}
