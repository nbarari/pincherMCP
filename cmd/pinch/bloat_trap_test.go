package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsBloatTrap_FilesystemRoot(t *testing.T) {
	// Use a platform-appropriate filesystem root so the test exercises the
	// real production input shape. On Linux/macOS this is "/"; on Windows
	// the current drive's root (e.g. `C:\`). filepath.Abs of either form
	// resolves to the OS-native root, which the production guard detects
	// via `filepath.Dir(abs) == abs`.
	root := "/"
	if runtime.GOOS == "windows" {
		// Resolve the current drive's root rather than hardcoding C:\.
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		root = filepath.VolumeName(cwd) + `\`
	}

	for _, hook := range []bool{true, false} {
		trap, reason := isBloatTrap(root, hook)
		if !trap {
			t.Errorf("isBloatTrap(%q, hook=%v) = false; want true", root, hook)
		}
		if reason == "" {
			t.Errorf("hook=%v: empty reason for %s", hook, root)
		}
	}
}

func TestIsBloatTrap_UserHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no HOME available")
	}
	for _, hook := range []bool{true, false} {
		trap, reason := isBloatTrap(home, hook)
		if !trap {
			t.Errorf("isBloatTrap(%q, hook=%v) = false; want true", home, hook)
		}
		if reason == "" {
			t.Errorf("hook=%v: empty reason for HOME", hook)
		}
	}
}

func TestIsBloatTrap_HookModeRequiresProjectMarker(t *testing.T) {
	// Empty temp directory — no project markers, not HOME, not /.
	dir := t.TempDir()

	// Hook mode should refuse: no project marker.
	trap, reason := isBloatTrap(dir, true)
	if !trap {
		t.Errorf("hook mode in empty dir should refuse, got allow")
	}
	if reason == "" {
		t.Error("hook mode refusal had empty reason")
	}

	// Manual mode should allow: trusts the explicit user action.
	trap, _ = isBloatTrap(dir, false)
	if trap {
		t.Error("manual mode in empty dir should allow")
	}
}

func TestIsBloatTrap_HookModeAllowsRecognizedProject(t *testing.T) {
	// Each marker, alone, should be enough to satisfy the hook guard.
	for _, marker := range []string{".git", "go.mod", "package.json", "Cargo.toml", "Makefile"} {
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			markerPath := filepath.Join(dir, marker)
			if marker == ".git" {
				if err := os.MkdirAll(markerPath, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", markerPath, err)
				}
			} else {
				if err := os.WriteFile(markerPath, []byte{}, 0o644); err != nil {
					t.Fatalf("write %s: %v", markerPath, err)
				}
			}

			trap, _ := isBloatTrap(dir, true)
			if trap {
				t.Errorf("hook mode with %s should allow indexing", marker)
			}
		})
	}
}

func TestIsBloatTrap_NonexistentPath(t *testing.T) {
	// A path that doesn't exist: filepath.EvalSymlinks fails and we fall
	// back to the absolute path. Catastrophic-case checks still apply,
	// project-marker check finds nothing → refused in hook mode.
	abs := filepath.Join(os.TempDir(), "definitely-not-a-real-path-pincher-test-12345")
	_, _ = os.Stat(abs) // ensure it doesn't exist; harmless if it does
	trap, _ := isBloatTrap(abs, true)
	if !trap {
		t.Errorf("hook mode against nonexistent path should refuse")
	}
}
