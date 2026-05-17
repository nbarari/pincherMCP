package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #1261: tests for the git-hooks install path. Covers idempotency,
// non-pincher-hook protection, --force backup behavior, and the
// not-a-git-repo skip.

// makeGitRepo creates a tmp directory with a minimal .git/hooks layout
// so installGitHooks treats it as a real repo. Returns the project dir.
func makeGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("mkdir .git/hooks: %v", err)
	}
	return dir
}

// TestInstallGitHooks_WritesThreeHooks_OnEmptyRepo pins the positive
// path: a repo with no existing hooks gets all three written, each
// carrying the managed marker.
func TestInstallGitHooks_WritesThreeHooks_OnEmptyRepo(t *testing.T) {
	dir := makeGitRepo(t)
	var out bytes.Buffer
	if err := installGitHooks(&out, dir, false, false); err != nil {
		t.Fatalf("installGitHooks: %v", err)
	}
	for _, name := range []string{"post-checkout", "post-merge", "post-rewrite"} {
		p := filepath.Join(dir, ".git", "hooks", name)
		body, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("expected hook at %s: %v", p, err)
			continue
		}
		if !strings.Contains(string(body), gitHookMarker) {
			t.Errorf("%s missing marker %q in body:\n%s", name, gitHookMarker, body)
		}
		if !strings.Contains(string(body), `pincher index "$REPO_ROOT" --force`) {
			t.Errorf("%s missing eager-reindex command; body:\n%s", name, body)
		}
	}
	if !strings.Contains(out.String(), "post-checkout") {
		t.Errorf("output didn't mention post-checkout install; got:\n%s", out.String())
	}
}

// TestInstallGitHooks_NotAGitRepo_SkipsWithoutError pins the safety
// branch: running on a non-git directory must NOT error (so the
// install stays safe for loose Claude Code workspaces).
func TestInstallGitHooks_NotAGitRepo_SkipsWithoutError(t *testing.T) {
	dir := t.TempDir() // no .git inside
	var out bytes.Buffer
	if err := installGitHooks(&out, dir, false, false); err != nil {
		t.Fatalf("installGitHooks on non-git dir errored: %v", err)
	}
	if !strings.Contains(out.String(), "not a git repository") {
		t.Errorf("expected 'not a git repository' skip message; got:\n%s", out.String())
	}
}

// TestInstallGitHooks_IdempotentReinstall pins that re-running on an
// already-pincher-managed repo reports no-change (no churn on commit
// timestamps, no spurious diff in workflow).
func TestInstallGitHooks_IdempotentReinstall(t *testing.T) {
	dir := makeGitRepo(t)
	var out1, out2 bytes.Buffer
	if err := installGitHooks(&out1, dir, false, false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := installGitHooks(&out2, dir, false, false); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	if !strings.Contains(out2.String(), "already up to date") {
		t.Errorf("re-install should report 'already up to date'; got:\n%s", out2.String())
	}
}

// TestInstallGitHooks_RefusesNonPincherHook pins the safety guard:
// a hand-written user hook (no marker) must NOT be silently clobbered.
// Without --force, the install logs a skip + continues to the other
// hook files.
func TestInstallGitHooks_RefusesNonPincherHook(t *testing.T) {
	dir := makeGitRepo(t)
	userHook := filepath.Join(dir, ".git", "hooks", "post-checkout")
	userBody := "#!/bin/sh\n# user's existing hook — must not be clobbered\necho hello\n"
	if err := os.WriteFile(userHook, []byte(userBody), 0o755); err != nil {
		t.Fatalf("seed user hook: %v", err)
	}
	var out bytes.Buffer
	if err := installGitHooks(&out, dir, false, false); err != nil {
		t.Fatalf("installGitHooks: %v", err)
	}
	body, _ := os.ReadFile(userHook)
	if string(body) != userBody {
		t.Errorf("user hook was clobbered without --force; body now:\n%s", body)
	}
	if !strings.Contains(out.String(), "not pincher-managed") {
		t.Errorf("expected refusal message; got:\n%s", out.String())
	}
	// Other two hooks (post-merge, post-rewrite) should still install
	// — refusal on one file shouldn't block the others.
	if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", "post-merge")); err != nil {
		t.Errorf("post-merge should still install when post-checkout was refused: %v", err)
	}
}

// TestInstallGitHooks_ForceBacksUpAndReplaces pins --force behavior:
// existing non-pincher hook gets backed up to .pincher-backup,
// pincher hook gets written in its place.
func TestInstallGitHooks_ForceBacksUpAndReplaces(t *testing.T) {
	dir := makeGitRepo(t)
	userHook := filepath.Join(dir, ".git", "hooks", "post-checkout")
	userBody := "#!/bin/sh\necho user-hook\n"
	if err := os.WriteFile(userHook, []byte(userBody), 0o755); err != nil {
		t.Fatalf("seed user hook: %v", err)
	}
	var out bytes.Buffer
	if err := installGitHooks(&out, dir, false, true); err != nil {
		t.Fatalf("installGitHooks --force: %v", err)
	}
	backup := userHook + ".pincher-backup"
	if body, err := os.ReadFile(backup); err != nil {
		t.Errorf("expected backup at %s: %v", backup, err)
	} else if string(body) != userBody {
		t.Errorf("backup content mismatch; got:\n%s", body)
	}
	body, _ := os.ReadFile(userHook)
	if !strings.Contains(string(body), gitHookMarker) {
		t.Errorf("post-force hook should be pincher-managed; body:\n%s", body)
	}
}

// TestInstallGitHooks_DryRunWritesNothing pins the preview branch:
// --dry-run prints the would-write summary but the filesystem is
// unchanged.
func TestInstallGitHooks_DryRunWritesNothing(t *testing.T) {
	dir := makeGitRepo(t)
	var out bytes.Buffer
	if err := installGitHooks(&out, dir, true, false); err != nil {
		t.Fatalf("installGitHooks --dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "would write") {
		t.Errorf("dry-run should preview with 'would write'; got:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", "post-checkout")); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote a hook to disk: %v", err)
	}
}

// TestPincherGitHookBody_PostCheckoutNoOpShortcuts pins the #1303 §2a
// behavior: post-checkout's body must include both no-op shortcuts
// (file-checkout and same-HEAD), and must apply ONLY to post-checkout
// — post-merge and post-rewrite always fire (no useful no-op signals
// in their arg shapes).
func TestPincherGitHookBody_PostCheckoutNoOpShortcuts(t *testing.T) {
	postCheckout := pincherGitHookBody("post-checkout")
	if !strings.Contains(postCheckout, `"${3:-1}" = "0"`) {
		t.Error("post-checkout must skip on $3=0 (file checkout) — #1303 §2a")
	}
	if !strings.Contains(postCheckout, `"$1" = "$2"`) {
		t.Error("post-checkout must skip when prev_HEAD == new_HEAD — #1303 §2a")
	}
	// Each shortcut is a separate exit 0; verify both fire before the
	// reindex line so the early-return wiring is correct.
	exitIdx := strings.Index(postCheckout, "exit 0")
	pincherIdx := strings.Index(postCheckout, "pincher index")
	if exitIdx < 0 || pincherIdx < 0 || exitIdx > pincherIdx {
		t.Errorf("post-checkout no-op exits must precede the pincher index call; got exit@%d index@%d", exitIdx, pincherIdx)
	}

	postMerge := pincherGitHookBody("post-merge")
	postRewrite := pincherGitHookBody("post-rewrite")
	for _, body := range []string{postMerge, postRewrite} {
		if strings.Contains(body, `"${3:-1}" = "0"`) {
			t.Error("only post-checkout should carry $3 no-op shortcut; post-merge/post-rewrite must not")
		}
		if strings.Contains(body, `"$1" = "$2"`) {
			t.Error("only post-checkout should carry same-HEAD no-op; post-merge/post-rewrite must not")
		}
		if !strings.Contains(body, "pincher index") {
			t.Error("post-merge / post-rewrite must always fire the reindex (no useful no-op signals in their arg shapes)")
		}
	}
}

// TestPincherGitHookBody_StructuralInvariants pins three properties
// of the generated hook body that downstream tooling depends on:
//   - leading shebang for POSIX exec
//   - marker substring for identifiability
//   - command -v guard so missing pincher doesn't break git
func TestPincherGitHookBody_StructuralInvariants(t *testing.T) {
	body := pincherGitHookBody("post-checkout")
	if !strings.HasPrefix(body, "#!/bin/sh\n") {
		t.Error("hook body must start with #!/bin/sh")
	}
	if !strings.Contains(body, gitHookMarker) {
		t.Errorf("hook body must contain marker %q", gitHookMarker)
	}
	if !strings.Contains(body, "command -v pincher") {
		t.Error("hook body must guard with command -v pincher so missing binary doesn't break git workflow")
	}
	if !strings.Contains(body, "git rev-parse --show-toplevel") {
		t.Error("hook body must locate repo root via git rev-parse")
	}
	if !strings.Contains(body, "&\n") {
		t.Error("hook body must background the indexer so git operation doesn't block")
	}
}
