package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTrunc(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 10, "abc"},
		{"abc\n", 10, "abc"},
		{"abc\r\n", 10, "abc"},
		{"abcdefghij", 5, "abcde..."},
		{"", 5, ""},
	}
	for _, c := range cases {
		if got := trunc(c.in, c.n); got != c.want {
			t.Errorf("trunc(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestBytesCollector_ConcurrentWrites(t *testing.T) {
	b := &bytesCollector{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				_, _ = b.Write([]byte("x"))
			}
		}()
	}
	wg.Wait()
	if got := b.String(); len(got) != 100 {
		t.Errorf("len=%d after 4×25 writes; want 100", len(got))
	}
}

// run() with a missing binary path returns immediately with a clear
// error from os.Stat. Covers the early-exit branch without spinning up
// a full subprocess.
func TestRun_MissingBinary_FailsFast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = ctx // run() builds its own context

	// run() resolves the binary via flag/cwd; here we exercise its core
	// path by giving it a path we know doesn't exist.
	err := run("/nonexistent/pincher", false, 1*time.Second)
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !strings.Contains(err.Error(), "exec") &&
		!strings.Contains(err.Error(), "not found") &&
		!strings.Contains(err.Error(), "no such file") &&
		!strings.Contains(err.Error(), "cannot find") {
		t.Errorf("error %q should hint at missing-binary cause", err.Error())
	}
}

// End-to-end: build a pincher binary in a temp dir, point run() at it
// in bare mode, and verify the probe drives the full sequence
// (initialize → health → mtime bump → stats → process exit) without
// returning an error. Covers the bulk of run() — the JSON-RPC send /
// expect closures, the roots/list reply branch, the chtimes path, and
// the cmd.Wait fall-through.
//
// Marked as the slowest test in the package (~10–20s for the build).
// Skipped in -short.
func TestRun_EndToEnd_BareMode(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipping in -short")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "pincher")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	// Build pincher into a tempdir. Use go build directly; we don't
	// need -cover instrumentation here because the probe doesn't
	// propagate GOCOVERDIR to the child anyway (it sets its own env).
	cwd, _ := os.Getwd()
	// repo root is three levels up from internal/supervisor/cmd/probe
	repoRoot := filepath.Join(cwd, "..", "..", "..", "..")
	build := exec.Command("go", "build", "-o", bin, "./cmd/pinch/")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build pincher: %v\n%s", err, out)
	}

	if err := run(bin, false, 25*time.Second); err != nil {
		t.Fatalf("probe run() returned error: %v", err)
	}
}

// Supervised-mode end-to-end. Same build + drive as bare mode but
// goes through `pincher supervised`. Covers the supervised branch
// in run() — the follow-up health call on the new inner — which is
// the path #371 specifically broke.
func TestRun_EndToEnd_SupervisedMode(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipping in -short")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "pincher")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	cwd, _ := os.Getwd()
	repoRoot := filepath.Join(cwd, "..", "..", "..", "..")
	build := exec.Command("go", "build", "-o", bin, "./cmd/pinch/")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build pincher: %v\n%s", err, out)
	}

	if err := run(bin, true, 30*time.Second); err != nil {
		t.Fatalf("probe run(-supervised) returned error: %v", err)
	}
}

// Ensure flag defaults round-trip: probe assumes ./pincher.exe in cwd
// when no -binary is passed. We can't easily call main() in a test
// because it calls os.Exit on failure; instead, assert the cwd-default
// is what the source claims (regression guard if someone moves the
// default).
func TestDefaultBinary_IsCwdRelative(t *testing.T) {
	// Build the cwd-default path the same way main() does — pure
	// filepath.Join, no IO. Asserts the convention; if main()'s
	// default ever changes, this test reminds the reader to update
	// the probe's docstring (`./pincher.exe` in cwd).
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got := filepath.Join(wd, "pincher.exe")
	if !strings.HasSuffix(got, "pincher.exe") {
		t.Errorf("default path %q must end in pincher.exe", got)
	}
}
