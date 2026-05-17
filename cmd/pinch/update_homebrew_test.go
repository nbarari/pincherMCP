package main

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
)

// #1260 §5: pincher update detects install method by inspecting the
// running binary path. Homebrew-installed binaries get dispatched to
// `brew upgrade pincher` instead of the GitHub-asset / go-install
// fallback that Mac users can't follow without a Go toolchain.

func TestDetectInstallMethod_HomebrewPrefixes(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"apple-silicon-cellar", "/opt/homebrew/Cellar/pincher/0.69.0/bin/pincher", "homebrew"},
		{"apple-silicon-bin", "/opt/homebrew/bin/pincher", "homebrew"},
		{"intel-mac-bin", "/usr/local/bin/pincher", "homebrew"},
		{"linuxbrew", "/home/linuxbrew/.linuxbrew/bin/pincher", "homebrew"},
		{"direct-binary-linux", "/usr/bin/pincher", "binary"},
		{"direct-binary-home", "/home/user/.local/bin/pincher", "binary"},
		{"empty-path", "", "binary"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Brew paths only apply on non-Windows; on Windows the abs
			// resolution turns these into something that won't match.
			// Skip on Windows since the symbol set is wrong for the
			// platform — the runtime path discovery would never produce
			// these strings there anyway.
			if runtime.GOOS == "windows" && c.path != "" {
				t.Skip("brew prefixes are POSIX-only; Windows path resolution would mangle these")
			}
			got := detectInstallMethod(c.path)
			if got != c.want {
				t.Errorf("detectInstallMethod(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

// TestUpgradeViaHomebrew_AdvisoryByDefault pins the safety default:
// without --yes, the function prints the brew command and exits. We
// don't auto-run brew from inside an unrelated process — brew is the
// kind of tool whose output the user wants to see live.
func TestUpgradeViaHomebrew_AdvisoryByDefault(t *testing.T) {
	prevRunner := brewRunner
	called := false
	brewRunner = func(out io.Writer, args ...string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { brewRunner = prevRunner })

	var buf bytes.Buffer
	if err := upgradeViaHomebrew(&buf, false /* check */, false /* yes */, false /* dryRun */); err != nil {
		t.Fatalf("upgradeViaHomebrew: %v", err)
	}
	if called {
		t.Error("brew was invoked without --yes; advisory default broken")
	}
	out := buf.String()
	if !strings.Contains(out, "brew upgrade pincher") {
		t.Errorf("expected brew upgrade hint in output; got:\n%s", out)
	}
	if !strings.Contains(out, "re-run with --yes") {
		t.Errorf("expected --yes opt-in hint; got:\n%s", out)
	}
}

// TestUpgradeViaHomebrew_DryRunDoesNotInvoke pins --dry-run safety —
// the brew runner must stay un-called even when --yes is passed, as
// long as --dry-run is set.
func TestUpgradeViaHomebrew_DryRunDoesNotInvoke(t *testing.T) {
	prevRunner := brewRunner
	called := false
	brewRunner = func(out io.Writer, args ...string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { brewRunner = prevRunner })

	var buf bytes.Buffer
	if err := upgradeViaHomebrew(&buf, false, true /* yes */, true /* dryRun */); err != nil {
		t.Fatalf("upgradeViaHomebrew dry-run: %v", err)
	}
	if called {
		t.Error("brew was invoked under --dry-run; the would-run path must stay print-only")
	}
	if !strings.Contains(buf.String(), "would run") {
		t.Errorf("expected 'would run' marker; got:\n%s", buf.String())
	}
}

// TestUpgradeViaHomebrew_YesInvokesBoth pins the active path: with
// --yes set and brew available, both `brew update` and `brew upgrade
// pincher` get invoked in that order. The test injects a brew runner
// that records argv per call rather than shelling out.
func TestUpgradeViaHomebrew_YesInvokesBoth(t *testing.T) {
	prevRunner := brewRunner
	var calls [][]string
	brewRunner = func(out io.Writer, args ...string) error {
		calls = append(calls, args)
		return nil
	}
	t.Cleanup(func() { brewRunner = prevRunner })

	var buf bytes.Buffer
	if err := upgradeViaHomebrew(&buf, false, true /* yes */, false); err != nil {
		// We accept the LookPath('brew') error here on systems without brew installed —
		// this test is about argv ordering, not toolchain presence.
		if !strings.Contains(err.Error(), "brew not found") {
			t.Fatalf("upgradeViaHomebrew --yes: %v", err)
		}
		t.Skip("brew not installed on this runner; argv check skipped")
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 brew invocations; got %d: %v", len(calls), calls)
	}
	if calls[0][0] != "update" {
		t.Errorf("first call should be `brew update`; got %v", calls[0])
	}
	if calls[1][0] != "upgrade" || calls[1][1] != "pincher" {
		t.Errorf("second call should be `brew upgrade pincher`; got %v", calls[1])
	}
}

// TestUpgradeViaHomebrew_PropagatesBrewError pins the error path:
// a brew failure surfaces with context, not a silent zero-exit. On
// systems where brew is not on PATH (most CI runners), the LookPath
// check preempts the runner — we still validate the error surface.
func TestUpgradeViaHomebrew_PropagatesBrewError(t *testing.T) {
	prevRunner := brewRunner
	brewRunner = func(out io.Writer, args ...string) error {
		if args[0] == "upgrade" {
			return errors.New("formula not found")
		}
		return nil
	}
	t.Cleanup(func() { brewRunner = prevRunner })

	var buf bytes.Buffer
	err := upgradeViaHomebrew(&buf, false, true, false)
	if err == nil {
		t.Fatal("expected non-nil error (either runner failure or LookPath)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "formula not found") && !strings.Contains(msg, "brew not found") {
		t.Errorf("error should be either the runner's brew-upgrade failure OR a LookPath('brew') miss; got: %v", err)
	}
}
