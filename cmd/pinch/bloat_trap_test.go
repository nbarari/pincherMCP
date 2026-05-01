package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsBloatTrap_FilesystemRoot(t *testing.T) {
	for _, hook := range []bool{true, false} {
		trap, reason := isBloatTrap("/", hook)
		if !trap {
			t.Errorf("isBloatTrap(\"/\", hook=%v) = false; want true", hook)
		}
		if reason == "" {
			t.Errorf("hook=%v: empty reason for /", hook)
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
