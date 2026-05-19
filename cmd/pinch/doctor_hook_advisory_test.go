package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #1635 v0.85: hookInstallAdvisory fires when Claude Code looks
// present AND no pincher PreToolUse hook is wired up in any known
// settings location. Tests pin the four corners of the truth table.

// withCwdAndHome runs fn with the process cwd and HOME (USERPROFILE
// on Windows) pointed at fresh temp directories so the advisory's
// filesystem probes operate on a controlled environment instead of
// the real user's home.
func withCwdAndHome(t *testing.T, fn func(cwd, home string)) {
	t.Helper()
	cwd := t.TempDir()
	home := t.TempDir()

	origCwd, _ := os.Getwd()
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	// os.UserHomeDir consults HOME on unix and USERPROFILE on Windows;
	// set both so the test is portable.
	prevHome := os.Getenv("HOME")
	prevUserProfile := os.Getenv("USERPROFILE")
	prevConfigDir := os.Getenv("CLAUDE_CONFIG_DIR")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// Clear CLAUDE_CONFIG_DIR — its presence alone trips the
	// "Claude Code present" gate, which would mask whatever the
	// test was actually checking.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Cleanup(func() {
		_ = os.Setenv("HOME", prevHome)
		_ = os.Setenv("USERPROFILE", prevUserProfile)
		_ = os.Setenv("CLAUDE_CONFIG_DIR", prevConfigDir)
	})

	fn(cwd, home)
}

// No Claude Code present anywhere → silent. The advisory must not
// nag users running under different agent harnesses.
func TestHookInstallAdvisory_SilentWithoutClaudeCode_1635(t *testing.T) {
	withCwdAndHome(t, func(cwd, home string) {
		got := hookInstallAdvisory()
		if got != "" {
			t.Errorf("expected silent advisory when no Claude Code signal present; got: %q", got)
		}
	})
}

// Claude Code present (project-local .claude/), hook NOT installed → fires.
func TestHookInstallAdvisory_FiresWhenHookMissing_1635(t *testing.T) {
	withCwdAndHome(t, func(cwd, home string) {
		// Bare .claude/ directory in cwd — Claude Code is present but
		// no settings file exists.
		if err := os.MkdirAll(filepath.Join(cwd, ".claude"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		got := hookInstallAdvisory()
		if got == "" {
			t.Fatal("expected advisory to fire when Claude Code present and hook not installed")
		}
		for _, want := range []string{"PreToolUse hook is NOT installed", "pincher init --target=claude", "#1635"} {
			if !strings.Contains(got, want) {
				t.Errorf("advisory missing required phrase %q; got:\n%s", want, got)
			}
		}
	})
}

// Hook installed in project-local .claude/settings.json → silent.
func TestHookInstallAdvisory_SilentWithProjectHook_1635(t *testing.T) {
	withCwdAndHome(t, func(cwd, home string) {
		settings := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Read|Grep",
        "hooks": [{"type":"command","command":"pincher hook-check"}]
      }
    ]
  }
}`
		writeSettings(t, filepath.Join(cwd, ".claude", "settings.json"), settings)
		if got := hookInstallAdvisory(); got != "" {
			t.Errorf("expected silent advisory with project-local hook installed; got: %q", got)
		}
	})
}

// Hook installed in home ~/.claude/settings.json → silent. Cwd has no
// .claude/ at all; the gate trips via the home settings file's
// existence.
func TestHookInstallAdvisory_SilentWithHomeHook_1635(t *testing.T) {
	withCwdAndHome(t, func(cwd, home string) {
		settings := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Read|Grep",
        "hooks": [{"type":"command","command":"/usr/local/bin/pincher hook-check --debug"}]
      }
    ]
  }
}`
		writeSettings(t, filepath.Join(home, ".claude", "settings.json"), settings)
		if got := hookInstallAdvisory(); got != "" {
			t.Errorf("expected silent advisory with home-level hook installed; got: %q", got)
		}
	})
}

// Settings file has hooks but none reference pincher → still fires.
// Pin the substring match used by hookFoundInSettings.
func TestHookInstallAdvisory_FiresWhenUnrelatedHookPresent_1635(t *testing.T) {
	withCwdAndHome(t, func(cwd, home string) {
		settings := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Read|Grep",
        "hooks": [{"type":"command","command":"echo unrelated-hook"}]
      }
    ]
  }
}`
		writeSettings(t, filepath.Join(cwd, ".claude", "settings.json"), settings)
		got := hookInstallAdvisory()
		if got == "" {
			t.Fatal("expected advisory to fire when an unrelated PreToolUse hook is present but no pincher hook-check")
		}
		if !strings.Contains(got, "PreToolUse hook is NOT installed") {
			t.Errorf("advisory phrasing changed; got:\n%s", got)
		}
	})
}

// Settings file is malformed JSON → treated as "no hook installed";
// advisory still fires because the read failed silently. This is the
// conservative default — if we can't parse, we can't claim the hook
// IS installed.
func TestHookInstallAdvisory_HandlesMalformedSettings_1635(t *testing.T) {
	withCwdAndHome(t, func(cwd, home string) {
		writeSettings(t, filepath.Join(cwd, ".claude", "settings.json"), "{ not valid json")
		got := hookInstallAdvisory()
		if got == "" {
			t.Fatal("expected advisory to fire when settings.json is malformed (we can't claim hook installed)")
		}
	})
}

// CLAUDE_CONFIG_DIR env var alone trips the "Claude Code present"
// gate even when no .claude/ directories exist. This catches users
// who've relocated their config via the env var override.
func TestHookInstallAdvisory_ClaudeConfigDirGate_1635(t *testing.T) {
	withCwdAndHome(t, func(cwd, home string) {
		// Cwd and home both empty of .claude/; CLAUDE_CONFIG_DIR set.
		t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "custom"))
		got := hookInstallAdvisory()
		if got == "" {
			t.Fatal("expected advisory to fire when CLAUDE_CONFIG_DIR is set (Claude Code is present, hook is not)")
		}
	})
}

func writeSettings(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
