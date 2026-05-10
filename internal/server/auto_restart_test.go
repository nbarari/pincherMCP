package server

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// #352: maybeAutoRestart fires only when env var is set AND binary was
// replaced. Tests substitute s.exitFn so the test process isn't killed.

func TestMaybeAutoRestart_EnvVarUnset_DoesNotExit(t *testing.T) {
	srv, _, _ := newTestServer(t)
	t.Setenv(autoRestartEnvVar, "")
	var exited int32
	srv.exitFn = func(code int) { atomic.StoreInt32(&exited, 1) }

	srv.maybeAutoRestart(true, true) // binary replaced AND drift — env var still gates

	if atomic.LoadInt32(&exited) != 0 {
		t.Error("exit fired with env var unset; expected no-op")
	}
}

func TestMaybeAutoRestart_EnvVarSet_BinaryNotReplaced_DoesNotExit(t *testing.T) {
	srv, _, _ := newTestServer(t)
	t.Setenv(autoRestartEnvVar, "1")
	var exited int32
	srv.exitFn = func(code int) { atomic.StoreInt32(&exited, 1) }

	srv.maybeAutoRestart(false, true) // env on, drift, but no new binary

	if atomic.LoadInt32(&exited) != 0 {
		t.Error("exit fired without binary replacement; expected no-op (would loop forever)")
	}
}

func TestMaybeAutoRestart_EnvVarSet_BinaryReplaced_Exits(t *testing.T) {
	srv, _, _ := newTestServer(t)
	t.Setenv(autoRestartEnvVar, "1")
	var exitedWith int32 = -1
	srv.exitFn = func(code int) { atomic.StoreInt32(&exitedWith, int32(code)) }

	srv.maybeAutoRestart(true, true) // env on, binary replaced — should fire

	if got := atomic.LoadInt32(&exitedWith); got != 0 {
		t.Errorf("exitFn called with code %d, want 0", got)
	}
}

func TestMaybeAutoRestart_OnceGate(t *testing.T) {
	srv, _, _ := newTestServer(t)
	t.Setenv(autoRestartEnvVar, "1")
	var calls int32
	srv.exitFn = func(code int) { atomic.AddInt32(&calls, 1) }

	// Multiple concurrent calls — only one exit should fire.
	for i := 0; i < 5; i++ {
		srv.maybeAutoRestart(true, true)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("exitFn fired %d times under sync.Once; want 1", got)
	}
}

// binaryReplacedSinceStart checks the actual mtime comparison logic.
func TestBinaryReplacedSinceStart_NewerOnDisk(t *testing.T) {
	srv, _, _ := newTestServer(t)
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "pincher.exe")
	if err := os.WriteFile(fakeBin, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(fakeBin)
	srv.binaryPath = fakeBin
	srv.binaryStartMTime = info.ModTime()

	// Simulate rebuild: rewrite the file with a forward-shifted mtime.
	future := info.ModTime().Add(2 * time.Second)
	if err := os.WriteFile(fakeBin, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fakeBin, future, future); err != nil {
		t.Fatal(err)
	}

	if !srv.binaryReplacedSinceStart() {
		t.Error("binaryReplacedSinceStart() = false, want true after on-disk mtime moved forward")
	}
}

func TestBinaryReplacedSinceStart_SameMtime(t *testing.T) {
	srv, _, _ := newTestServer(t)
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "pincher.exe")
	if err := os.WriteFile(fakeBin, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(fakeBin)
	srv.binaryPath = fakeBin
	srv.binaryStartMTime = info.ModTime()

	if srv.binaryReplacedSinceStart() {
		t.Error("binaryReplacedSinceStart() = true with no rebuild; expected false (would loop)")
	}
}

func TestBinaryReplacedSinceStart_CaptureFailed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.binaryPath = ""
	srv.binaryStartMTime = time.Time{}

	if srv.binaryReplacedSinceStart() {
		t.Error("expected false when capture failed (empty path)")
	}
}

// #364: checkAutoRestart fires when called from non-health paths
// (jsonResultWithMeta / textResultWithMeta), not just from handleHealth.
func TestCheckAutoRestart_TriggersOnReplacedBinary(t *testing.T) {
	srv, _, _ := newTestServer(t)
	t.Setenv(autoRestartEnvVar, "1")

	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "pincher.exe")
	if err := os.WriteFile(fakeBin, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(fakeBin)
	srv.binaryPath = fakeBin
	srv.binaryStartMTime = info.ModTime()

	// Move on-disk mtime forward to simulate a rebuild.
	future := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(fakeBin, future, future); err != nil {
		t.Fatal(err)
	}

	var exited int32
	srv.exitFn = func(code int) { atomic.StoreInt32(&exited, 1) }

	srv.checkAutoRestart()

	if atomic.LoadInt32(&exited) != 1 {
		t.Error("checkAutoRestart() did not fire exit; expected restart on replaced binary")
	}
}

func TestCheckAutoRestart_NoOpWhenEnvUnset(t *testing.T) {
	srv, _, _ := newTestServer(t)
	t.Setenv(autoRestartEnvVar, "")

	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "pincher.exe")
	if err := os.WriteFile(fakeBin, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(fakeBin)
	srv.binaryPath = fakeBin
	srv.binaryStartMTime = info.ModTime()
	future := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(fakeBin, future, future); err != nil {
		t.Fatal(err)
	}

	var exited int32
	srv.exitFn = func(code int) { atomic.StoreInt32(&exited, 1) }

	srv.checkAutoRestart()

	if atomic.LoadInt32(&exited) != 0 {
		t.Error("exit fired with env var unset; opt-in semantics broken")
	}
}

func TestCheckAutoRestart_NoOpWhenBinaryNotReplaced(t *testing.T) {
	srv, _, _ := newTestServer(t)
	t.Setenv(autoRestartEnvVar, "1")

	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "pincher.exe")
	if err := os.WriteFile(fakeBin, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(fakeBin)
	srv.binaryPath = fakeBin
	srv.binaryStartMTime = info.ModTime()

	var exited int32
	srv.exitFn = func(code int) { atomic.StoreInt32(&exited, 1) }

	srv.checkAutoRestart()

	if atomic.LoadInt32(&exited) != 0 {
		t.Error("exit fired without binary replacement; would loop forever")
	}
}
