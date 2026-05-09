package main

import (
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pincherMCP/pincher/internal/db"
)

//go:embed policy.md
var pincherPolicyMarkdown string

const (
	// pincherInitMarkerStart and pincherInitMarkerEnd bracket the
	// pincher-managed section of CLAUDE.md so re-running `pincher init`
	// can replace the block in place rather than duplicating content.
	// They're HTML comments so they render as nothing in Markdown viewers
	// but are trivially round-trippable via simple string scanning.
	pincherInitMarkerStart = "<!-- pincher:start -->"
	pincherInitMarkerEnd   = "<!-- pincher:end -->"

	// pincherInitBlockHeader is the human-readable preface that appears
	// inside the marker block. It explains where the content came from so
	// a reader who lands on CLAUDE.md without context still understands.
	pincherInitBlockHeader = "<!-- Managed by `pincher init`. Edit `pincher init` to change this block,\n     or delete the markers below to opt out of future updates. -->\n\n"
)

// runInitCLI implements `pincher init [--global] [--dry-run] [--force]`.
//
// Writes (or replaces, in place) a pincher usage policy block in either
// the project-local CLAUDE.md (default) or the global ~/.claude/CLAUDE.md
// (when --global is set). The block is wrapped in
// `<!-- pincher:start --> ... <!-- pincher:end -->` markers so a future
// `pincher init` run can update it without leaving stale duplicates.
//
// After writing, prints a starter recipe (analogous to the `guide` MCP
// tool) and the URL of any running pincher HTTP dashboard discovered via
// the sessions table — so the user sees where to go next, on the same
// terminal, without needing to remember a separate `pincher web` call.
func runInitCLI(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	global := fs.Bool("global", false, "Write to ~/.claude/CLAUDE.md instead of ./CLAUDE.md")
	dryRun := fs.Bool("dry-run", false, "Print what would be written; do not modify any file")
	force := fs.Bool("force", false, "Overwrite the marker block without prompting (default behavior anyway, kept for explicit scripted use)")
	dataDir := fs.String("data-dir", "", "Override data directory (used to discover the running HTTP dashboard URL)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pincher init [--global] [--dry-run] [--force]")
		fmt.Fprintln(os.Stderr, "  Inserts a pincher usage policy block into CLAUDE.md (idempotent; replace-in-place via marker comments).")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	_ = force // kept for future "do nothing if a non-pincher block exists at that path" semantics

	target, err := resolveCLAUDEPath(*global)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher init: %v\n", err)
		os.Exit(1)
	}

	existing := readFileIfExists(target)
	updated, action := mergePolicyBlock(existing, pincherPolicyMarkdown)

	if *dryRun {
		fmt.Fprintf(os.Stdout, "pincher init: would %s %s\n\n", action, target)
		fmt.Fprintln(os.Stdout, "--- new file content ---")
		fmt.Fprintln(os.Stdout, updated)
		return
	}

	if err := writeFileEnsuringDir(target, updated); err != nil {
		fmt.Fprintf(os.Stderr, "pincher init: write %s: %v\n", target, err)
		os.Exit(1)
	}

	out := os.Stdout
	fmt.Fprintf(out, "pincher init: %s %s\n", action, target)
	printNextSteps(out, *dataDir)
}

// resolveCLAUDEPath returns the absolute path to the CLAUDE.md that
// `pincher init` should write to.
func resolveCLAUDEPath(global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("user home dir: %w", err)
		}
		return filepath.Join(home, ".claude", "CLAUDE.md"), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cwd: %w", err)
	}
	return filepath.Join(cwd, "CLAUDE.md"), nil
}

func readFileIfExists(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func writeFileEnsuringDir(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// mergePolicyBlock inserts or replaces the pincher policy block in
// existing. Returns (updated, action) where action is "wrote", "updated"
// or "appended". Behavior:
//
//   - existing is empty → emit a complete CLAUDE.md (h1 header + block)
//   - existing has both markers → replace content between them
//   - existing has neither marker → append block at end with a leading blank line
//   - existing has malformed markers (only start, only end) → append a
//     fresh block; we don't attempt automatic recovery because the cause
//     is more often "user edited the markers" than "tool corrupted them"
func mergePolicyBlock(existing, policy string) (string, string) {
	block := buildPolicyBlock(policy)

	if existing == "" {
		header := "# CLAUDE.md\n\nThis file provides guidance to Claude Code (claude.ai/code) when working with this project.\n\n"
		return header + block + "\n", "wrote"
	}

	startIdx := strings.Index(existing, pincherInitMarkerStart)
	endIdx := strings.Index(existing, pincherInitMarkerEnd)
	if startIdx >= 0 && endIdx > startIdx {
		// Replace inclusive of both markers.
		before := existing[:startIdx]
		afterIdx := endIdx + len(pincherInitMarkerEnd)
		after := existing[afterIdx:]
		// Trim a trailing newline from `before` so we don't accumulate
		// blank lines on every re-run, and ensure exactly one blank line
		// before/after the block.
		before = strings.TrimRight(before, "\n")
		after = strings.TrimLeft(after, "\n")

		var b strings.Builder
		b.WriteString(before)
		if before != "" {
			b.WriteString("\n\n")
		}
		b.WriteString(block)
		if after != "" {
			b.WriteString("\n\n")
			b.WriteString(after)
		} else {
			b.WriteString("\n")
		}
		return b.String(), "updated"
	}

	// Append a new block. Ensure there's a single trailing newline on existing
	// and one blank line before the new block.
	trimmed := strings.TrimRight(existing, "\n")
	return trimmed + "\n\n" + block + "\n", "appended"
}

// buildPolicyBlock wraps policy in the start/end markers plus the
// "managed by pincher init" header comment.
func buildPolicyBlock(policy string) string {
	var b strings.Builder
	b.WriteString(pincherInitMarkerStart)
	b.WriteString("\n")
	b.WriteString(pincherInitBlockHeader)
	b.WriteString(strings.TrimRight(policy, "\n"))
	b.WriteString("\n\n")
	b.WriteString(pincherInitMarkerEnd)
	return b.String()
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

// errEmptyPolicy is exported via test helpers so unit tests can
// assert that an empty embed never reaches mergePolicyBlock.
var errEmptyPolicy = errors.New("embedded pincher policy is empty")

// init validates the embed at package init so a build-time mistake
// (empty policy.md, missing file) surfaces immediately rather than
// at first `pincher init` call. The policy is embedded via go:embed;
// missing files would fail at compile time, but an accidentally
// emptied file would compile and fail at runtime — this gate keeps
// the failure adjacent to the binary's startup.
func init() {
	if bytes.TrimSpace([]byte(pincherPolicyMarkdown)) == nil {
		// Panic in init is intentional: an empty policy means the binary
		// is broken at distribution time. Better to crash loudly than
		// write an empty pincher block to every user's CLAUDE.md.
		panic(errEmptyPolicy)
	}
}
