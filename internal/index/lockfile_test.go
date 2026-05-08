package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireProjectLock_FreshSucceeds(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireProjectLock(dir, "/Users/nick/some/project")
	if err != nil {
		t.Fatalf("acquire on empty dir: %v", err)
	}
	defer release()

	// Lockfile must exist with current PID.
	lockPath := projectLockPath(dir, "/Users/nick/some/project")
	info, err := readLockInfo(lockPath)
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	if info.PID != os.Getpid() {
		t.Errorf("lockfile pid = %d, want %d", info.PID, os.Getpid())
	}
	if info.ProjectID != "/Users/nick/some/project" {
		t.Errorf("project id = %q, want /Users/nick/some/project", info.ProjectID)
	}
}

func TestAcquireProjectLock_ConflictWithLiveHolder(t *testing.T) {
	dir := t.TempDir()
	release1, err := acquireProjectLock(dir, "shared")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release1()

	// Second acquire from the same process — the lockfile records the
	// current PID, which is alive, so the second attempt must reject.
	if _, err := acquireProjectLock(dir, "shared"); err == nil {
		t.Error("second acquire should have failed; live holder is current pid")
	} else if !strings.Contains(err.Error(), "already being indexed") {
		t.Errorf("expected 'already being indexed' in error, got: %v", err)
	}
}

func TestAcquireProjectLock_ReleaseAllowsReacquire(t *testing.T) {
	dir := t.TempDir()
	release1, err := acquireProjectLock(dir, "rotate")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	release1()

	release2, err := acquireProjectLock(dir, "rotate")
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	release2()
}

func TestAcquireProjectLock_StaleHolderIsReclaimed(t *testing.T) {
	dir := t.TempDir()
	lockPath := projectLockPath(dir, "abandoned")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pretend an unrealistically high PID held the lock and crashed.
	// PIDs above 4 million are extremely unlikely on any current OS.
	stale := lockInfo{PID: 4_000_001, StartTime: 1, ProjectID: "abandoned"}
	data, _ := json.Marshal(stale)
	if err := os.WriteFile(lockPath, data, 0o644); err != nil {
		t.Fatalf("seed stale lockfile: %v", err)
	}

	release, err := acquireProjectLock(dir, "abandoned")
	if err != nil {
		t.Fatalf("acquire over stale lockfile: %v", err)
	}
	defer release()

	// The lockfile now records THIS process, not the stale PID.
	got, _ := readLockInfo(lockPath)
	if got.PID != os.Getpid() {
		t.Errorf("after reclaim, lockfile pid = %d, want %d", got.PID, os.Getpid())
	}
}

func TestAcquireProjectLock_CorruptLockfileIsReclaimed(t *testing.T) {
	dir := t.TempDir()
	lockPath := projectLockPath(dir, "corrupt")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt lockfile: %v", err)
	}

	release, err := acquireProjectLock(dir, "corrupt")
	if err != nil {
		t.Fatalf("acquire over corrupt lockfile: %v", err)
	}
	release()
}

func TestProcessExists(t *testing.T) {
	if !processExists(os.Getpid()) {
		t.Error("processExists(self) returned false")
	}
	// PID 0 / negative are documented as not-a-real-process.
	if processExists(0) {
		t.Error("processExists(0) returned true")
	}
	if processExists(-1) {
		t.Error("processExists(-1) returned true")
	}
	// PID 4_000_001 is far above realistic process tables on macOS/Linux/Windows.
	if processExists(4_000_001) {
		t.Error("processExists(4_000_001) returned true; expected nonexistent")
	}
}

func TestProjectLockPath_StableHash(t *testing.T) {
	dir := "/x/data"
	a := projectLockPath(dir, "/some/project")
	b := projectLockPath(dir, "/some/project")
	if a != b {
		t.Errorf("hash not deterministic: %s vs %s", a, b)
	}
	c := projectLockPath(dir, "/different/project")
	if a == c {
		t.Errorf("different projectIDs collide on lockpath: %s", a)
	}
	if !strings.HasSuffix(a, ".lock") {
		t.Errorf("lockpath %q missing .lock suffix", a)
	}
}
