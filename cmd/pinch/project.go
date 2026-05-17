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
	"time"

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
	case "prune-stale":
		runProjectPruneStaleCLI(args[1:], os.Stdin, os.Stdout, os.Stderr)
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
	fmt.Fprintln(out, "  prune-stale         Drop projects indexed by an old schema and untouched for N days")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Common flags:")
	fmt.Fprintln(out, "  --json              Emit structured output instead of human-readable text")
	fmt.Fprintln(out, "  --data-dir DIR      Override data directory")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "rm flags:")
	fmt.Fprintln(out, "  --force             Skip the Y/n confirmation prompt")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "prune-stale flags:")
	fmt.Fprintln(out, "  --days N            Min idle days since last index (default 30)")
	fmt.Fprintln(out, "  --force             Skip the Y/n confirmation prompt")
	fmt.Fprintln(out, "  Run `pincher vacuum` afterward to reclaim the freed disk space.")
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
	current := db.CurrentSchemaVersion()
	out := projectListReport{Count: len(ps), Projects: make([]ProjectStats, 0, len(ps))}
	for _, p := range ps {
		stats := ProjectStats{
			ID:                   p.ID,
			Name:                 p.Name,
			Path:                 p.Path,
			Files:                p.FileCount,
			Symbols:              p.SymCount,
			Edges:                p.EdgeCount,
			SchemaVersionAtIndex: p.SchemaVersionAtIndex,
		}
		stats.Stale, stats.StaleReason = staleness(p.SchemaVersionAtIndex, current)
		out.Projects = append(out.Projects, stats)
	}
	return out
}

// staleness compares a project's schema_version_at_index against the
// running binary's max-known schema version. Returns (stale, reason).
//
// nil → pre-v15 row, treated as "stale (unknown)" — definitely older
// than the column itself, but we don't know how much older.
//
// non-nil and < current → stale with a precise reason.
// non-nil and >= current → fresh.
func staleness(at *int, current int) (bool, string) {
	if at == nil {
		return true, "predates schema_version_at_index column (pre-v15)"
	}
	if *at < current {
		return true, fmt.Sprintf("indexed at v%d, current is v%d", *at, current)
	}
	return false, ""
}

// isStaleFailure reports whether an extraction_failures row was recorded
// in a pre-current-indexed-at pass and is awaiting a fresh re-extraction
// to either re-record or implicitly clear (via PruneExtractionFailuresForFile,
// #1319). Cleared-but-not-re-indexed projects accumulate rows whose
// last_seen_at predates the project's most-recent indexed_at — these are
// what we surface with this tag (#1382). When indexedAt is the zero time
// (project record missing the column for some reason), conservatively
// return false so we don't false-positive.
//
// Mirrors `internal/server/admin.go::isStaleFailure` per the
// bounded-duplication convention; the two must stay byte-identical.
func isStaleFailure(failureLastSeen, indexedAt time.Time) bool {
	if indexedAt.IsZero() {
		return false
	}
	return failureLastSeen.Before(indexedAt)
}

// formatProjectList renders the table form used by `pincher project list`.
// Stale projects (schema_version_at_index < current, or NULL) get a `[stale]`
// suffix appended to their name so the gap nbarari hit (#236) is visible
// at a glance.
func formatProjectList(ps []db.Project) string {
	if len(ps) == 0 {
		return "No projects indexed.\n\nRun `pincher index <path>` to add one.\n"
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })

	current := db.CurrentSchemaVersion()
	staleCount := 0
	var b strings.Builder
	fmt.Fprintf(&b, "%-32s  %-10s  %-10s  %-10s  %s\n", "PROJECT", "FILES", "SYMBOLS", "EDGES", "PATH")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 100))
	for _, p := range ps {
		name := p.Name
		if name == "" {
			name = p.ID
		}
		stale, _ := staleness(p.SchemaVersionAtIndex, current)
		if stale {
			staleCount++
			name += " [stale]"
		}
		if len(name) > 32 {
			name = name[:29] + "..."
		}
		fmt.Fprintf(&b, "%-32s  %-10d  %-10d  %-10d  %s\n", name, p.FileCount, p.SymCount, p.EdgeCount, p.Path)
	}
	if staleCount > 0 {
		fmt.Fprintf(&b, "\n%d project(s), %d stale. Re-index a stale project with `pincher index <path>`.\n", len(ps), staleCount)
	} else {
		fmt.Fprintf(&b, "\n%d project(s).\n", len(ps))
	}
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
	positional := parseFlagsInterspersed(fs, args)

	// #801: every error path must honor --json. Pre-fix the no-match /
	// ambiguous / store-error paths printed plain text even under
	// --json, so a scripted `project rm --json` got unparseable output
	// on exactly the cases a script most needs to branch on. fail emits
	// a JSON error object in JSON mode, plain text otherwise.
	fail := func(code int, extra map[string]any, format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		if *asJSON {
			obj := map[string]any{"removed": false, "error": msg}
			for k, v := range extra {
				obj[k] = v
			}
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(obj)
		} else {
			fmt.Fprintln(stderr, "pincher project rm: "+msg)
		}
		os.Exit(code)
	}

	if len(positional) == 0 {
		if !*asJSON {
			fs.Usage()
		}
		fail(2, nil, "<name|id|substring> required")
	}
	target := positional[0]

	store, dir, err := openProjectStore(*dataDir)
	if err != nil {
		fail(1, nil, "%v", err)
	}
	defer store.Close()
	_ = dir

	projects, err := store.ListProjects()
	if err != nil {
		fail(1, nil, "%v", err)
	}

	match, status := matchProject(projects, target)
	switch status {
	case matchNone:
		fail(1, nil, "no project matches %q", target)
	case matchAmbiguous:
		candidates := make([]map[string]any, 0, len(match))
		for _, p := range match {
			candidates = append(candidates, map[string]any{"id": p.ID, "name": p.Name, "path": p.Path})
		}
		if !*asJSON {
			fmt.Fprintf(stderr, "pincher project rm: %q matches multiple projects:\n\n", target)
			for _, p := range match {
				fmt.Fprintf(stderr, "  %-32s  %s\n", p.Name, p.Path)
			}
			fmt.Fprintln(stderr, "\nNarrow with a longer substring, the full name, or the project id.")
			os.Exit(1)
		}
		fail(1, map[string]any{"matches": candidates},
			"%q matches multiple projects — narrow with a longer substring, the full name, or the project id", target)
	}
	chosen := match[0]

	// JSON callers must pass --force explicitly. Without it, a scripted
	// `pincher project rm --json` invocation has no way to answer the
	// y/N prompt and would either hang or get silently deleted; both
	// are worse than an explicit error nudging the user toward --force.
	if *asJSON && !*force {
		fail(2, nil, "--json requires --force (no interactive confirmation in JSON mode)")
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
		fail(1, map[string]any{"id": chosen.ID}, "delete %s: %v", chosen.ID, err)
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

// ── prune-stale ──────────────────────────────────────────────────────────────

// prunableStale reports whether a project is safe to drop in a bulk
// prune: it must be BOTH schema-stale (indexed by an older binary, or
// pre-v15) AND idle — not re-indexed since the cutoff. The schema check
// alone would also catch a project a user touched yesterday that just
// needs a re-index; pairing it with the idle cutoff scopes the prune to
// genuinely-abandoned projects (#732 design note).
func prunableStale(p db.Project, current int, cutoff time.Time) bool {
	stale, _ := staleness(p.SchemaVersionAtIndex, current)
	if !stale {
		return false
	}
	return p.IndexedAt.Before(cutoff)
}

func runProjectPruneStaleCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) {
	fs := flag.NewFlagSet("project prune-stale", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Override data directory")
	asJSON := fs.Bool("json", false, "Emit a structured JSON receipt")
	force := fs.Bool("force", false, "Skip the Y/n confirmation prompt")
	days := fs.Int("days", 30, "Minimum idle days since last index for a project to be prunable")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: pincher project prune-stale [--days N] [--force] [--json] [--data-dir DIR]")
		fmt.Fprintln(stderr, "  Drops every project that is BOTH schema-stale AND not re-indexed in --days days.")
		fmt.Fprintln(stderr, "  Run `pincher vacuum` afterward to reclaim the freed disk space.")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	fail := func(code int, format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]any{"pruned": false, "error": msg})
		} else {
			fmt.Fprintln(stderr, "pincher project prune-stale: "+msg)
		}
		os.Exit(code)
	}

	if *days < 0 {
		fail(2, "--days must be >= 0")
	}

	// JSON callers must pass --force — there's no way to answer an
	// interactive prompt in a scripted invocation. Checked up front
	// (before any DB work) so the contract is the same regardless of
	// whether the DB happens to have candidates. Same posture as
	// `project rm --json`.
	if *asJSON && !*force {
		fail(2, "--json requires --force (no interactive confirmation in JSON mode)")
	}

	store, _, err := openProjectStore(*dataDir)
	if err != nil {
		fail(1, "%v", err)
	}
	defer store.Close()

	projects, err := store.ListProjects()
	if err != nil {
		fail(1, "%v", err)
	}

	current := db.CurrentSchemaVersion()
	cutoff := time.Now().AddDate(0, 0, -*days)
	var candidates []db.Project
	for _, p := range projects {
		if prunableStale(p, current, cutoff) {
			candidates = append(candidates, p)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })

	if len(candidates) == 0 {
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]any{"pruned": true, "removed": []any{}, "count": 0})
			return
		}
		fmt.Fprintf(stdout, "No stale projects to prune (idle >= %d days AND schema-stale).\n", *days)
		return
	}

	if !*force {
		fmt.Fprintf(stdout, "%d stale project(s) eligible for prune (idle >= %d days):\n\n", len(candidates), *days)
		for _, p := range candidates {
			_, reason := staleness(p.SchemaVersionAtIndex, current)
			fmt.Fprintf(stdout, "  %-32s  %d symbols  indexed %s  (%s)\n",
				p.Name, p.SymCount, p.IndexedAt.Format("2006-01-02"), reason)
		}
		fmt.Fprintf(stdout, "\nRemove all %d? [y/N]: ", len(candidates))
		if !confirmYesFrom(stdin) {
			fmt.Fprintln(stdout, "Aborted.")
			return
		}
	}

	removed := make([]map[string]any, 0, len(candidates))
	var failures []string
	for _, p := range candidates {
		if delErr := store.DeleteProject(p.ID); delErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", p.ID, delErr))
			continue
		}
		removed = append(removed, map[string]any{
			"id": p.ID, "name": p.Name, "path": p.Path,
			"symbols": p.SymCount, "edges": p.EdgeCount,
		})
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		out := map[string]any{"pruned": true, "removed": removed, "count": len(removed)}
		if len(failures) > 0 {
			out["failures"] = failures
		}
		_ = enc.Encode(out)
		if len(failures) > 0 {
			os.Exit(1)
		}
		return
	}
	fmt.Fprintf(stdout, "Removed %d stale project(s).\n", len(removed))
	for _, f := range failures {
		fmt.Fprintln(stderr, "  failed: "+f)
	}
	if len(removed) > 0 {
		fmt.Fprintln(stdout, "Run `pincher vacuum` to reclaim the freed disk space.")
	}
	if len(failures) > 0 {
		os.Exit(1)
	}
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
