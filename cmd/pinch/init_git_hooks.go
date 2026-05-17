package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// installGitHooks writes (or replaces) pincher-managed git hooks into
// `.git/hooks/` so branch switches, fast-forward merges, and rebases
// fire an eager reindex on the running MCP. Without these, the
// `Watch()` poller catches the changes one diff-pass at a time, which
// is correct but inefficient AND leaves a window where the index
// reflects a mix of both branches' states (#1261 §1).
//
// Three hooks installed:
//   - post-checkout — fires on `git checkout` / `git switch`
//   - post-merge    — fires after `git pull` and `git merge`
//   - post-rewrite  — fires after `git rebase` / `git commit --amend`
//
// Each hook is a small shell script with a managed-by-pincher marker
// comment + a single `pincher index "$REPO_ROOT" --force` call in the
// background so the git operation doesn't block. The marker lets a
// future `pincher init --git-hooks` run safely replace its own hooks
// without clobbering hand-written user hooks — non-marker hooks are
// refused unless --force is set.
//
// Idempotent: replacing a previously-installed pincher hook surfaces
// as "no change" when the body matches. Newly-written hooks are made
// executable (0o755).
func installGitHooks(out io.Writer, projectDir string, dryRun, force bool) error {
	hooksDir := filepath.Join(projectDir, ".git", "hooks")
	if info, err := os.Stat(filepath.Join(projectDir, ".git")); err != nil || !info.IsDir() {
		// Not a git repo. The hook install is a no-op rather than an
		// error so `pincher init --git-hooks` stays safe to run on
		// loose directories (Claude Code workspaces aren't always
		// project roots).
		fmt.Fprintf(out, "pincher init [git-hooks]: %s is not a git repository — skipping\n", projectDir)
		return nil
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", hooksDir, err)
	}

	for _, name := range []string{"post-checkout", "post-merge", "post-rewrite"} {
		hookPath := filepath.Join(hooksDir, name)
		newBody := pincherGitHookBody(name)

		existing, err := os.ReadFile(hookPath)
		switch {
		case err == nil:
			if !strings.Contains(string(existing), gitHookMarker) {
				if !force {
					fmt.Fprintf(out, "pincher init [git-hooks]: %s already exists and is not pincher-managed — pass --force to replace\n", hookPath)
					continue
				}
				// --force mode: preserve the user's hook in a .bak
				// sibling so they can recover if the replacement
				// turns out to be a mistake. Better than silent
				// overwrite.
				backup := hookPath + ".pincher-backup"
				if werr := os.WriteFile(backup, existing, 0o644); werr != nil {
					return fmt.Errorf("backup %s → %s: %w", hookPath, backup, werr)
				}
				fmt.Fprintf(out, "pincher init [git-hooks]: backed up existing %s → %s\n", hookPath, backup)
			} else if string(existing) == newBody {
				fmt.Fprintf(out, "pincher init [git-hooks]: %s already up to date — no change\n", hookPath)
				continue
			}
		case !os.IsNotExist(err):
			return fmt.Errorf("read %s: %w", hookPath, err)
		}

		if dryRun {
			fmt.Fprintf(out, "pincher init [git-hooks]: would write %s\n", hookPath)
			fmt.Fprintln(out, "--- new hook content ---")
			fmt.Fprintln(out, newBody)
			continue
		}

		if err := os.WriteFile(hookPath, []byte(newBody), 0o755); err != nil {
			return fmt.Errorf("write %s: %w", hookPath, err)
		}
		fmt.Fprintf(out, "pincher init [git-hooks]: wrote %s\n", hookPath)
	}

	if !dryRun {
		fmt.Fprintln(out, "pincher init [git-hooks]: branch switches, fast-forward merges, and rebases will now fire an eager reindex.")
	}
	return nil
}

// gitHookMarker is the substring that identifies a pincher-managed
// hook. Future `pincher init --git-hooks` runs detect it to safely
// replace pincher hooks without clobbering hand-written user hooks.
//
// Embedded in every hook body's leading comment block so a one-line
// `grep` over .git/hooks/ tells the user which hooks pincher manages.
const gitHookMarker = "pincher.io/managed"

// pincherGitHookBody returns the shell-script body for the named git
// hook. Three hooks are supported (post-checkout, post-merge,
// post-rewrite); each one fires the same eager-reindex command —
// except post-checkout which respects git's no-op signals (#1303 §2a):
//
//   - post-checkout receives 3 args: $1=prev_HEAD, $2=new_HEAD, $3=flag
//     (1=branch checkout, 0=file checkout). File checkouts (`git
//     checkout README.md`) don't move HEAD and don't need a reindex —
//     skip when $3=0. Same-branch checkouts (`git checkout main` while
//     already on main) also don't change state — skip when $1==$2.
//   - post-merge / post-rewrite always fire (no useful no-op signals
//     in their arg shapes worth optimizing for in shell).
//
// The body uses POSIX sh + `git rev-parse` to locate the repo root,
// so the hook works regardless of where the developer's shell was
// launched. The `pincher index ... &` background runs the indexer
// without blocking the git operation — the user sees their `git
// checkout` complete immediately, with the reindex happening in the
// background. If pincher isn't on PATH, the hook silently no-ops
// (the `command -v` guard) — never breaks the user's git workflow.
func pincherGitHookBody(name string) string {
	header := `#!/bin/sh
# ` + gitHookMarker + `: pincher git hook — fires an eager reindex
# on ` + name + ` so the index doesn't lag the working tree after
# branch switches / pulls / rebases (#1261).
#
# Safe to delete: pincher's Watch() poller catches the changes
# eventually; this hook just collapses the window from seconds to
# milliseconds. Replace freely — pincher init --git-hooks will
# refuse to clobber non-pincher hooks (no marker comment).
`

	// #1303 §2a: post-checkout no-op shortcuts. File checkouts and
	// same-branch checkouts don't change the working tree's branch
	// state, so the reindex is pure overhead — skip them. Saves the
	// per-call BuildClosure cost (~500ms on pincher-repo) on every
	// `git checkout README.md` or repeated `git checkout main`.
	noopBranch := ""
	if name == "post-checkout" {
		noopBranch = `
# #1303 §2a: skip when this isn't actually a branch change.
#   $1 = ref of previous HEAD
#   $2 = ref of new HEAD
#   $3 = 1 if branch checkout, 0 if file checkout
if [ "${3:-1}" = "0" ]; then
    exit 0   # file checkout — working tree unchanged, no reindex needed
fi
if [ "$1" = "$2" ]; then
    exit 0   # same HEAD — re-checkout of current branch is a no-op
fi
`
	}

	return header + noopBranch + `
if command -v pincher >/dev/null 2>&1; then
    REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)"
    if [ -n "$REPO_ROOT" ]; then
        pincher index "$REPO_ROOT" --force >/dev/null 2>&1 &
    fi
fi
`
}
