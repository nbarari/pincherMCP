package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/kwad77/pincher/internal/db"
)

// runProjectCLI implements `pincher project <list|rm> ...`. It surfaces
// the existing HTTP `DELETE /v1/projects` and the `list` MCP tool as
// CLI verbs so users on the stdio binary don't need a SQL or curl
// one-liner to drop a stale project.
//
// Closes #202.
func runProjectCLI(args []string) {
	log.SetOutput(io.Discard)

	if len(args) == 0 {
		printProjectUsage(os.Stderr)
		os.Exit(2)
	}

	switch args[0] {
	case "list", "ls":
		runProjectListCLI(args[1:], os.Stdout, os.Stderr)
	case "rm", "remove", "delete":
		runProjectRMCLI(args[1:], os.Stdin, os.Stdout, os.Stderr)
	case "-h", "--help", "help":
		printProjectUsage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "pincher project: unknown verb %q\n", args[0])
		printProjectUsage(os.Stderr)
		os.Exit(2)
	}
}

func printProjectUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: pincher project <verb> [args]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Verbs:")
	fmt.Fprintln(out, "  list                List every indexed project (alias: ls)")
	fmt.Fprintln(out, "  rm <name|id|substr> Remove one project from the index (alias: remove, delete)")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Common flags:")
	fmt.Fprintln(out, "  --json              Emit structured output instead of human-readable text")
	fmt.Fprintln(out, "  --data-dir DIR      Override data directory")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "rm flags:")
	fmt.Fprintln(out, "  --force             Skip the Y/n confirmation prompt")
}

// ── list ─────────────────────────────────────────────────────────────────────

func runProjectListCLI(args []string, stdout, stderr io.Writer) {
	fs := flag.NewFlagSet("project list", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Override data directory")
	asJSON := fs.Bool("json", false, "Emit structured JSON instead of a table")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: pincher project list [--json] [--data-dir DIR]")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	store, dir, err := openProjectStore(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "pincher project list: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	_ = dir

	projects, err := store.ListProjects()
	if err != nil {
		fmt.Fprintf(stderr, "pincher project list: %v\n", err)
		os.Exit(1)
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(buildProjectListReport(projects))
		return
	}
	fmt.Fprint(stdout, formatProjectList(projects))
}

// projectListReport is the structured form `pincher project list --json`
// emits. Mirrors the MCP `list` tool output so CLI / MCP / HTTP all
// speak the same shape.
type projectListReport struct {
	Count    int            `json:"count"`
	Projects []ProjectStats `json:"projects"`
}

func buildProjectListReport(ps []db.Project) projectListReport {
	out := projectListReport{Count: len(ps), Projects: make([]ProjectStats, 0, len(ps))}
	for _, p := range ps {
		out.Projects = append(out.Projects, ProjectStats{
			ID:      p.ID,
			Name:    p.Name,
			Path:    p.Path,
			Files:   p.FileCount,
			Symbols: p.SymCount,
			Edges:   p.EdgeCount,
		})
	}
	return out
}

// formatProjectList renders the table form used by `pincher project list`.
func formatProjectList(ps []db.Project) string {
	if len(ps) == 0 {
		return "No projects indexed.\n\nRun `pincher index <path>` to add one.\n"
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })

	var b strings.Builder
	fmt.Fprintf(&b, "%-32s  %-10s  %-10s  %-10s  %s\n", "PROJECT", "FILES", "SYMBOLS", "EDGES", "PATH")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 100))
	for _, p := range ps {
		name := p.Name
		if name == "" {
			name = p.ID
		}
		if len(name) > 32 {
			name = name[:29] + "..."
		}
		fmt.Fprintf(&b, "%-32s  %-10d  %-10d  %-10d  %s\n", name, p.FileCount, p.SymCount, p.EdgeCount, p.Path)
	}
	fmt.Fprintf(&b, "\n%d project(s).\n", len(ps))
	return b.String()
}

// ── rm ───────────────────────────────────────────────────────────────────────

func runProjectRMCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) {
	fs := flag.NewFlagSet("project rm", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Override data directory")
	asJSON := fs.Bool("json", false, "Emit a structured JSON receipt")
	force := fs.Bool("force", false, "Skip the Y/n confirmation prompt")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: pincher project rm [--force] [--json] [--data-dir DIR] <name|id|substring>")
		fmt.Fprintln(stderr, "  Removes one indexed project. Substring matches disambiguate via a list when ambiguous.")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "pincher project rm: <name|id|substring> required")
		fs.Usage()
		os.Exit(2)
	}
	target := fs.Arg(0)

	store, dir, err := openProjectStore(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "pincher project rm: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	_ = dir

	projects, err := store.ListProjects()
	if err != nil {
		fmt.Fprintf(stderr, "pincher project rm: %v\n", err)
		os.Exit(1)
	}

	match, status := matchProject(projects, target)
	switch status {
	case matchNone:
		fmt.Fprintf(stderr, "pincher project rm: no project matches %q\n", target)
		os.Exit(1)
	case matchAmbiguous:
		fmt.Fprintf(stderr, "pincher project rm: %q matches multiple projects:\n\n", target)
		for _, p := range match {
			fmt.Fprintf(stderr, "  %-32s  %s\n", p.Name, p.Path)
		}
		fmt.Fprintln(stderr, "\nNarrow with a longer substring, the full name, or the project id.")
		os.Exit(1)
	}
	chosen := match[0]

	// JSON callers must pass --force explicitly. Without it, a scripted
	// `pincher project rm --json` invocation has no way to answer the
	// y/N prompt and would either hang or get silently deleted; both
	// are worse than an explicit error nudging the user toward --force.
	if *asJSON && !*force {
		fmt.Fprintln(stderr, "pincher project rm: --json requires --force (no interactive confirmation in JSON mode)")
		os.Exit(2)
	}
	if !*force {
		fmt.Fprintf(stdout, "Remove project %q (%s) — %d files, %d symbols, %d edges? [y/N]: ",
			chosen.Name, chosen.Path, chosen.FileCount, chosen.SymCount, chosen.EdgeCount)
		if !confirmYesFrom(stdin) {
			fmt.Fprintln(stdout, "Aborted.")
			return
		}
	}

	if err := store.DeleteProject(chosen.ID); err != nil {
		fmt.Fprintf(stderr, "pincher project rm: delete %s: %v\n", chosen.ID, err)
		os.Exit(1)
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"removed":  true,
			"id":       chosen.ID,
			"name":     chosen.Name,
			"path":     chosen.Path,
			"files":    chosen.FileCount,
			"symbols":  chosen.SymCount,
			"edges":    chosen.EdgeCount,
		})
		return
	}
	fmt.Fprintf(stdout, "Removed project %q (%s) — %d files, %d symbols, %d edges.\n",
		chosen.Name, chosen.Path, chosen.FileCount, chosen.SymCount, chosen.EdgeCount)
}

// confirmYesFrom reads one line from r and reports whether it starts
// with 'y' or 'Y'. EOF / read error returns false (treat as "no").
// Distinct from update.go's confirmYes() (which reads directly from
// os.Stdin) so this code path stays unit-testable.
func confirmYesFrom(r io.Reader) bool {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	return strings.EqualFold(line[:1], "y")
}

// ── matching ─────────────────────────────────────────────────────────────────

type matchStatus int

const (
	matchNone matchStatus = iota
	matchExact
	matchAmbiguous
)

// matchProject resolves a user-supplied target string against the
// project list. Resolution order:
//  1. Exact match on id (full and prefix-unique). IDs are stable, so
//     scripted callers benefit from the deterministic path.
//  2. Exact match on name (case-insensitive).
//  3. Substring match on name OR path (case-insensitive). One hit
//     returns matchExact; multiple returns matchAmbiguous and the
//     caller is expected to render a disambiguation list.
//
// Returned slice is non-empty for matchExact (length 1) and
// matchAmbiguous (length ≥ 2); empty for matchNone.
func matchProject(ps []db.Project, target string) ([]db.Project, matchStatus) {
	if target == "" {
		return nil, matchNone
	}
	low := strings.ToLower(target)

	// 1. Exact id match.
	for _, p := range ps {
		if p.ID == target {
			return []db.Project{p}, matchExact
		}
	}
	// 2. Exact (case-insensitive) name match.
	var nameHits []db.Project
	for _, p := range ps {
		if strings.EqualFold(p.Name, target) {
			nameHits = append(nameHits, p)
		}
	}
	if len(nameHits) == 1 {
		return nameHits, matchExact
	}
	if len(nameHits) > 1 {
		return nameHits, matchAmbiguous
	}
	// 3. Substring on name or path.
	var subHits []db.Project
	for _, p := range ps {
		if strings.Contains(strings.ToLower(p.Name), low) || strings.Contains(strings.ToLower(p.Path), low) {
			subHits = append(subHits, p)
		}
	}
	switch len(subHits) {
	case 0:
		return nil, matchNone
	case 1:
		return subHits, matchExact
	default:
		return subHits, matchAmbiguous
	}
}

// ── shared ───────────────────────────────────────────────────────────────────

func openProjectStore(dataDirOverride string) (*db.Store, string, error) {
	dir := dataDirOverride
	if dir == "" {
		var err error
		dir, err = db.DataDir()
		if err != nil {
			return nil, "", fmt.Errorf("data directory: %w", err)
		}
	}
	store, err := db.Open(dir)
	if err != nil {
		return nil, "", fmt.Errorf("open database: %w", err)
	}
	return store, dir, nil
}
