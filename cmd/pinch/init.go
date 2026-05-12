package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kwad77/pincher/internal/db"
	pinit "github.com/kwad77/pincher/internal/init"
)

// runInitCLI implements `pincher init [--global] [--dry-run] [--force]`.
//
// Writes (or replaces, in place) a pincher usage policy block in
// either the project-local CLAUDE.md (default) or the global
// ~/.claude/CLAUDE.md (when --global is set). The block is wrapped
// in `<!-- pincher:start --> ... <!-- pincher:end -->` markers so a
// future `pincher init` run can update it without leaving stale
// duplicates.
//
// The pure planning + merge logic lives in internal/init (#253);
// this function is the CLI orchestration layer.
func runInitCLI(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	global := fs.Bool("global", false, "Write the global rules file (target-dependent; e.g. ~/.claude/CLAUDE.md for claude)")
	dryRun := fs.Bool("dry-run", false, "Print what would be written; do not modify any file")
	force := fs.Bool("force", false, "Overwrite the marker block without prompting (default behavior anyway, kept for explicit scripted use)")
	dataDir := fs.String("data-dir", "", "Override data directory (used to discover the running HTTP dashboard URL)")
	targetFlag := fs.String("target", "claude", "Editor target: "+strings.Join(pinit.TargetNames(), ", "))
	noHook := fs.Bool("no-hook", false, "(claude target only) Skip writing the .claude/settings.json PreToolUse hook. Default false — the hook is what closes the Read/Grep → pincher gap at runtime.")
	quiet := fs.Bool("quiet", false, "Suppress the per-language extraction-tier profile printed after the wiring step (#631). The wiring itself still runs.")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pincher init [--target=NAME] [--global] [--dry-run] [--force]")
		fmt.Fprintln(os.Stderr, "  Seed a pincher usage policy file for an editor or agent (idempotent; replace-in-place via marker comments).")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  Targets:")
		for _, t := range pinit.AllTargets {
			fmt.Fprintf(os.Stderr, "    %-14s %s\n", t.Name, t.Describe)
		}
		fmt.Fprintln(os.Stderr, "    detect         Pick every target whose marker file exists under cwd")
		fmt.Fprintln(os.Stderr, "    all            Write every project-scoped target")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	_ = force

	out := os.Stdout
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher init: cwd: %v\n", err)
		os.Exit(1)
	}
	targets, err := pinit.ResolveTargets(*targetFlag, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher init: %v\n", err)
		os.Exit(1)
	}

	for _, t := range targets {
		if err := runInitTarget(out, t, cwd, *global, *dryRun); err != nil {
			fmt.Fprintf(os.Stderr, "pincher init: %v\n", err)
			os.Exit(1)
		}
		// #627: when target=claude (and we're not running a global
		// install — hooks are project-scoped), wire the PreToolUse hook
		// so that Read/Grep on indexed files redirects to pincher
		// equivalents at runtime. Without this, the CLAUDE.md policy
		// is the only nudge — and instruction-layer nudges plateau.
		if t.Name == "claude" && !*global && !*noHook {
			if err := installClaudeHook(out, cwd, *dryRun); err != nil {
				fmt.Fprintf(os.Stderr, "pincher init: hook install: %v\n", err)
				os.Exit(1)
			}
		}
	}

	if !*dryRun {
		// #631: print the per-language extraction-tier profile so the
		// user sees "Ruby is regex-tier, Scala is stub-tier" before
		// they run their first session and conclude pincher doesn't
		// work. --quiet suppresses for CI/scripted installs. Profile
		// failures are non-fatal — install already succeeded.
		if !*quiet {
			if profile, err := pinit.ProfileDir(cwd); err == nil {
				pinit.PrintProfile(out, profile)
			}
		}
		printNextSteps(out, *dataDir)
	}
}

// runInitTarget writes (or dry-runs) a single target. global is the
// user's --global flag; for targets that don't support it, the
// underlying Plan call silently ignores rather than errors so that
// --target=all keeps working with --global set.
func runInitTarget(out io.Writer, t pinit.Target, cwd string, global, dryRun bool) error {
	plan, err := pinit.Plan(t, cwd, global)
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Fprintf(out, "pincher init [%s]: would %s %s\n\n", plan.Target, plan.Action, plan.Path)
		fmt.Fprintln(out, "--- new file content ---")
		fmt.Fprintln(out, plan.Updated)
		return nil
	}

	if err := pinit.WriteFileEnsuringDir(plan.Path, plan.Updated); err != nil {
		return fmt.Errorf("[%s] write %s: %w", plan.Target, plan.Path, err)
	}
	fmt.Fprintf(out, "pincher init [%s]: %s %s\n", plan.Target, plan.Action, plan.Path)
	return nil
}

// printNextSteps emits a guide-style recipe + the URL of any running
// HTTP dashboard. Failures are non-fatal — the init succeeded by the
// time we get here, and a missing data dir or empty sessions table
// just means we have nothing to add to the recipe.
func printNextSteps(out io.Writer, dataDirOverride string) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Next steps:")
	fmt.Fprintln(out, "  1. Run `pincher index` from this directory to build the symbol graph.")
	fmt.Fprintln(out, "  2. Connect your MCP client (Claude Code, Cursor, etc.) to `pincher`.")
	fmt.Fprintln(out, "  3. Or open the dashboard: `pincher web`")

	dir := dataDirOverride
	if dir == "" {
		var err error
		dir, err = db.DataDir()
		if err != nil {
			return
		}
	}
	store, err := db.Open(dir)
	if err != nil {
		return
	}
	defer store.Close()

	if base, _, ok := findLiveHTTPServer(store); ok {
		fmt.Fprintf(out, "\nLive dashboard: %s\n", dashboardURL(base))
	}
}
