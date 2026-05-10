package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/kwad77/pincher/internal/db"
)

// runStatsCLI implements `pincher stats [--json] [--reset] [--data-dir DIR]`.
//
// Without flags: prints a human-readable summary of the persisted savings
// across every recorded session, plus a per-project file/symbol/edge
// breakdown. Sources:
//   - GetAllTimeSavings — sum across the sessions table
//   - ListProjects — per-project counts
//
// `--json` emits the same data as a structured object suitable for
// dashboards / shell pipelines (`pincher stats --json | jq ...`).
//
// `--reset` wipes the sessions table — the pre-existing path was direct
// SQLite via the user's own `sqlite3` invocation, which is brittle and
// requires knowing the schema. Returns the count of rows deleted.
//
// CLI-only (no MCP exposure) by deliberate choice: stats wipe is a
// destructive admin action. An LLM agent should never wipe stats without
// explicit human invocation.
func runStatsCLI(args []string) {
	log.SetOutput(io.Discard)

	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Override data directory")
	asJSON := fs.Bool("json", false, "Emit structured JSON instead of human-readable text")
	reset := fs.Bool("reset", false, "Wipe the sessions table (clears all-time totals); symbol / edge / project data is unaffected")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pincher stats [--json] [--reset] [--data-dir DIR]")
		fmt.Fprintln(os.Stderr, "  Prints persisted session savings (cost avoided, tokens saved, call count)")
		fmt.Fprintln(os.Stderr, "  plus per-project file/symbol/edge counts. --reset wipes session history")
		fmt.Fprintln(os.Stderr, "  without touching symbol data; back up first via `--json > file.json`.")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	dir := *dataDir
	if dir == "" {
		var err error
		dir, err = db.DataDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pincher: failed to determine data directory: %v\n", err)
			os.Exit(1)
		}
	}

	store, err := db.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher: failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	if *reset {
		runStatsReset(store, dir, *asJSON)
		return
	}

	report, err := buildStatsReport(store, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher: stats failed: %v\n", err)
		os.Exit(1)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return
	}
	fmt.Print(formatStatsText(report))
}

// runStatsReset performs the wipe and emits a one-line confirmation
// (text mode) or a structured object (JSON mode).
func runStatsReset(store *db.Store, dir string, asJSON bool) {
	rows, err := store.ResetSessions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher: stats reset failed: %v\n", err)
		os.Exit(1)
	}

	if asJSON {
		out := map[string]any{
			"reset":        true,
			"rows_deleted": rows,
			"data_dir":     dir,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}
	fmt.Printf("Wiped %d session row(s) from %s\n", rows, filepath.Join(dir, "pincher.db"))
}

// StatsReport is the structured form `pincher stats --json` emits.
type StatsReport struct {
	DataDir   string         `json:"data_dir"`
	DBSizeKB  int64          `json:"db_size_kb"`
	AllTime   AllTimeSavings `json:"all_time"`
	Projects  []ProjectStats `json:"projects"`
}

// AllTimeSavings is the sum across every persisted session row.
type AllTimeSavings struct {
	Calls        int64   `json:"calls"`
	TokensUsed   int64   `json:"tokens_used"`
	TokensSaved  int64   `json:"tokens_saved"`
	CostAvoided  float64 `json:"cost_avoided"`
}

// ProjectStats is a per-project file/symbol/edge breakdown.
type ProjectStats struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Files    int    `json:"files"`
	Symbols  int    `json:"symbols"`
	Edges    int    `json:"edges"`
	// SchemaVersionAtIndex is the schema version current when this
	// project was last indexed (#236). Pointer-to-int so we can
	// distinguish nil ("indexed before the column existed — pre-v15")
	// from a recorded value. Omitted from JSON when nil.
	SchemaVersionAtIndex *int `json:"schema_version_at_index,omitempty"`
	// Stale is true when the project's schema version is below the
	// current binary's max-known schema version (or unknown / pre-v15).
	// Computed at render time, not persisted. JSON consumers can use
	// this directly; the CLI table appends a `[stale]` marker.
	Stale bool `json:"stale,omitempty"`
	// StaleReason carries a short human-readable hint surfaced in
	// `pincher doctor` ("indexed at v12, current is v15"). Empty when
	// Stale is false.
	StaleReason string `json:"stale_reason,omitempty"`
}

func buildStatsReport(store *db.Store, dir string) (*StatsReport, error) {
	calls, tokensUsed, tokensSaved, costAvoided, err := store.GetAllTimeSavings()
	if err != nil {
		return nil, fmt.Errorf("all-time savings: %w", err)
	}

	projects, err := store.ListProjects()
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	projOut := make([]ProjectStats, 0, len(projects))
	for _, p := range projects {
		projOut = append(projOut, ProjectStats{
			ID:      p.ID,
			Name:    p.Name,
			Path:    p.Path,
			Files:   p.FileCount,
			Symbols: p.SymCount,
			Edges:   p.EdgeCount,
		})
	}

	return &StatsReport{
		DataDir:  dir,
		DBSizeKB: dbFileSizeKB(dir),
		AllTime: AllTimeSavings{
			Calls:       calls,
			TokensUsed:  tokensUsed,
			TokensSaved: tokensSaved,
			CostAvoided: costAvoided,
		},
		Projects: projOut,
	}, nil
}

// dbFileSizeKB returns the size of pincher.db in KB, or 0 if stat fails.
func dbFileSizeKB(dir string) int64 {
	info, err := os.Stat(filepath.Join(dir, "pincher.db"))
	if err != nil {
		return 0
	}
	return info.Size() / 1024
}

// formatStatsText renders the report as a human-readable text block
// modeled after the MCP `stats` tool's box but without the "live SESSION"
// section since the CLI binary has no in-memory atomic counters.
func formatStatsText(r *StatsReport) string {
	var b strings.Builder
	const w = 44

	header := func(title string) string {
		pad := w - 2 - len(title)
		left := pad / 2
		right := pad - left
		return "│ " + strings.Repeat(" ", left) + title + strings.Repeat(" ", right) + " │\n"
	}
	line := func(label, value string) string {
		content := fmt.Sprintf("  %-20s %s", label, value)
		if len(content) < w {
			content += strings.Repeat(" ", w-len(content))
		}
		return "│" + content + "│\n"
	}
	sep := "├" + strings.Repeat("─", w) + "┤\n"
	commify := func(n int64) string {
		s := fmt.Sprintf("%d", n)
		for i := len(s) - 3; i > 0; i -= 3 {
			s = s[:i] + "," + s[i:]
		}
		return s
	}

	b.WriteString("┌" + strings.Repeat("─", w) + "┐\n")
	b.WriteString(header("ALL-TIME"))
	b.WriteString(line("Tool calls:", commify(r.AllTime.Calls)))
	b.WriteString(line("Tokens used:", commify(r.AllTime.TokensUsed)))
	b.WriteString(line("Tokens saved:", "~"+commify(r.AllTime.TokensSaved)))
	b.WriteString(line("Cost avoided:", fmt.Sprintf("$%.4f", r.AllTime.CostAvoided)))

	b.WriteString(sep)
	b.WriteString(header("STORAGE"))
	b.WriteString(line("Data dir:", truncMid(r.DataDir, 22)))
	b.WriteString(line("DB size:", commify(r.DBSizeKB)+" KB"))

	if len(r.Projects) > 0 {
		b.WriteString(sep)
		b.WriteString(header("PROJECTS"))
		// Two lines per project so symbol counts up to ~447k still fit
		// inside the 44-char box. Name on line 1, symbol/file counts on
		// line 2 indented under the name.
		for _, p := range r.Projects {
			b.WriteString(line(truncEnd(p.Name, 28)+":", ""))
			b.WriteString(line("",
				fmt.Sprintf("%s syms / %s files",
					commify(int64(p.Symbols)),
					commify(int64(p.Files)))))
		}
	}

	b.WriteString("└" + strings.Repeat("─", w) + "┘\n")
	return b.String()
}

// truncMid and truncEnd live in cmd/pinch/doctor.go (same `main` package);
// they're shared between the doctor and stats subcommands.
