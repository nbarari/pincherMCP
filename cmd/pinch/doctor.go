package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// runDoctorCLI implements `pincher doctor [--json] [--data-dir DIR]
// [--lookback HOURS] [--top N]`.
//
// Reads from the diagnostic tables landed by #42 parts 1 + 2:
//   - extraction_failures (file-level parse / heuristic flags)
//   - slow_queries (per-call latency over threshold)
//
// Plus existing state: schema_version, projects, DB / WAL file sizes.
//
// Output is human-readable Markdown by default; --json emits a structured
// object for CI / dashboard consumption.
//
// This is the user-visible payoff for the data the indexer + server have
// been collecting. No new schema, no new server-side capture — just a
// reader.
func runDoctorCLI(args []string) {
	log.SetOutput(io.Discard)

	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Override data directory")
	asJSON := fs.Bool("json", false, "Emit structured JSON instead of Markdown")
	lookbackHours := fs.Int("lookback", 168, "Hours of history to include in failure / slow-query lists (default 168 = 7 days)")
	top := fs.Int("top", 10, "Maximum number of failures / slow queries to list per section")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pincher doctor [--json] [--data-dir DIR] [--lookback HOURS] [--top N]")
		fmt.Fprintln(os.Stderr, "  Prints a diagnostic report from the local pincher database.")
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

	report, err := buildDoctorReport(store, dir, *lookbackHours, *top)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher: doctor failed: %v\n", err)
		os.Exit(1)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return
	}
	fmt.Print(formatDoctorMarkdown(report))
}

// DoctorReport is the structured form `pincher doctor --json` emits.
// Field names are stable across releases — dashboards and CI scripts
// can rely on them.
type DoctorReport struct {
	GeneratedAt          string                  `json:"generated_at"`
	BinaryVersion        string                  `json:"binary_version"`
	SchemaVersion        int                     `json:"schema_version"`
	BinarySupportsSchema bool                    `json:"binary_supports_schema"`
	DBSizeBytes          int64                   `json:"db_size_bytes"`
	WALSizeBytes         int64                   `json:"wal_size_bytes"`
	Projects             []DoctorProjectSummary  `json:"projects"`
	ExtractionFailures   []DoctorFailureRow      `json:"extraction_failures"`
	SlowQueries          []DoctorSlowQueryRow    `json:"slow_queries"`
	LookbackHours        int                     `json:"lookback_hours"`
	// Advisories are human-readable health warnings (#732). Always
	// present — empty slice when healthy — so JSON consumers can
	// iterate without a null check. Mirrors the MCP doctor tool's
	// `advisories` field.
	Advisories []string `json:"advisories"`
}

type DoctorProjectSummary struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Path                 string `json:"path"`
	Files                int    `json:"files"`
	Symbols              int    `json:"symbols"`
	Edges                int    `json:"edges"`
	IndexedAt            string `json:"indexed_at"`
	SchemaVersionAtIndex *int   `json:"schema_version_at_index,omitempty"`
	Stale                bool   `json:"stale,omitempty"`
	StaleReason          string `json:"stale_reason,omitempty"`
}

type DoctorFailureRow struct {
	Project    string `json:"project"`
	File       string `json:"file"`
	Language   string `json:"language"`
	Reason     string `json:"reason"`
	Details    string `json:"details"`
	LastSeenAt string `json:"last_seen_at"`
}

type DoctorSlowQueryRow struct {
	Tool       string `json:"tool"`
	Project    string `json:"project,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Arguments  string `json:"arguments"`
	OccurredAt string `json:"occurred_at"`
}

// buildDoctorReport gathers all facts. Each section degrades gracefully
// — a failed read on (say) extraction_failures doesn't kill the whole
// report; we emit a partial one so the user still sees what worked.
func buildDoctorReport(store *db.Store, dir string, lookbackHours, top int) (*DoctorReport, error) {
	r := &DoctorReport{
		GeneratedAt:          time.Now().UTC().Format(time.RFC3339),
		BinaryVersion:        version,
		LookbackHours:        lookbackHours,
		BinarySupportsSchema: true, // assumption: the binary that opened the DB supports its schema
		// JSON slice invariant: these are append-only below and must
		// marshal to `[]`, not `null`, when empty so consumers can
		// iterate without a null-check. Advisories is initialised the
		// same way further down. ExtractionFailures/SlowQueries were
		// missed in #832; Projects was missed there too — a clean
		// install (no indexed projects) marshalled `"projects": null`.
		Projects:           []DoctorProjectSummary{},
		ExtractionFailures: []DoctorFailureRow{},
		SlowQueries:        []DoctorSlowQueryRow{},
	}

	// Schema version
	if err := store.DB().QueryRow(`SELECT version FROM schema_version`).Scan(&r.SchemaVersion); err != nil {
		// Non-fatal — pre-versioning DB, or migration in progress.
		r.SchemaVersion = 0
	}

	// DB + WAL file sizes (best-effort — missing files are reported as 0)
	dbPath := filepath.Join(dir, "pincher.db")
	if info, err := os.Stat(dbPath); err == nil {
		r.DBSizeBytes = info.Size()
	}
	if info, err := os.Stat(dbPath + "-wal"); err == nil {
		r.WALSizeBytes = info.Size()
	}

	// Projects
	projects, err := store.ListProjects()
	if err == nil {
		current := db.CurrentSchemaVersion()
		for _, p := range projects {
			summary := DoctorProjectSummary{
				ID:                   p.ID,
				Name:                 p.Name,
				Path:                 p.Path,
				Files:                p.FileCount,
				Symbols:              p.SymCount,
				Edges:                p.EdgeCount,
				IndexedAt:            p.IndexedAt.Format(time.RFC3339),
				SchemaVersionAtIndex: p.SchemaVersionAtIndex,
			}
			summary.Stale, summary.StaleReason = staleness(p.SchemaVersionAtIndex, current)
			r.Projects = append(r.Projects, summary)
		}
	}

	// Extraction failures (#42 part 1) — across all projects, filtered by
	// lookback window, capped at top N per project.
	cutoff := time.Now().Add(-time.Duration(lookbackHours) * time.Hour)
	for _, p := range projects {
		fails, err := store.ListExtractionFailures(p.ID, top)
		if err != nil {
			continue
		}
		for _, f := range fails {
			if f.LastSeenAt.Before(cutoff) {
				continue
			}
			r.ExtractionFailures = append(r.ExtractionFailures, DoctorFailureRow{
				Project:    p.Name,
				File:       f.FilePath,
				Language:   f.Language,
				Reason:     f.Reason,
				Details:    f.Details,
				LastSeenAt: f.LastSeenAt.Format(time.RFC3339),
			})
		}
	}

	// Slow queries (#42 part 2) — most-recent N within lookback.
	slow, err := store.ListSlowQueries(top * 5) // pull a few more, filter by cutoff
	if err == nil {
		for _, sq := range slow {
			if sq.OccurredAt.Before(cutoff) {
				continue
			}
			r.SlowQueries = append(r.SlowQueries, DoctorSlowQueryRow{
				Tool:       sq.Tool,
				Project:    sq.ProjectID,
				DurationMS: sq.DurationMS,
				Arguments:  sq.Arguments,
				OccurredAt: sq.OccurredAt.Format(time.RFC3339),
			})
			if len(r.SlowQueries) >= top {
				break
			}
		}
	}

	// Sort projects by symbol count desc (largest projects first — the
	// most likely place a problem would show up).
	sort.Slice(r.Projects, func(i, j int) bool {
		return r.Projects[i].Symbols > r.Projects[j].Symbols
	})

	// #732: health advisories. Always a non-nil slice (JSON invariant).
	r.Advisories = []string{}
	if a := largeDBAdvisory(r.DBSizeBytes, r.Projects); a != "" {
		r.Advisories = append(r.Advisories, a)
	}
	// #635 v0.67 follow-up: payload-outlier advisory. Mirrors the
	// MCP doctor's logic in internal/server/admin.go — the two copies
	// must stay behaviourally identical.
	if payloadRows, err := store.ToolCallPayloadSizeByTool(7*24*60*60, 200); err == nil {
		if a := payloadOutlierAdvisory(payloadRows); a != "" {
			r.Advisories = append(r.Advisories, a)
		}
	}
	// #635 v0.67 follow-up: tool-mix entropy advisory. Mirrors the
	// MCP doctor's logic in internal/server/admin.go.
	if tallyRows, err := store.ToolCallStatsByTool(7*24*60*60, 200); err == nil {
		if a := toolMixStuckAdvisory(tallyRows); a != "" {
			r.Advisories = append(r.Advisories, a)
		}
	}

	return r, nil
}

// toolMixStuckAdvisory mirrors internal/server/admin.go's copy.
// Bounded-duplication convention documented on largeDBAdvisory.
func toolMixStuckAdvisory(rows []db.ToolCallTallyRow) string {
	const (
		minCalls     = int64(100)
		maxEntropy   = 1.0
		minTop1Share = 0.80
	)
	var total int64
	for _, r := range rows {
		total += r.CallCount
	}
	if total < minCalls || len(rows) == 0 {
		return ""
	}
	var entropy float64
	for _, r := range rows {
		p := float64(r.CallCount) / float64(total)
		if p > 0 {
			entropy += -p * (math.Log(p) / math.Ln2)
		}
	}
	top1Share := float64(rows[0].CallCount) / float64(total)
	if entropy >= maxEntropy || top1Share < minTop1Share {
		return ""
	}
	return fmt.Sprintf(
		"Tool-mix entropy is %.2f bits over %d calls — the agent is essentially repeating one tool "+
			"(%q at %.0f%% of all calls). Usually means a query that is not converging: the agent keeps "+
			"re-issuing the same shape rather than triangulating. Inspect via the dashboard's "+
			"Tool-Mix Health panel; consider whether the agent needs a wider initial probe "+
			"(architecture / search) before drilling.",
		entropy, total, rows[0].Tool, top1Share*100,
	)
}

// payloadOutlierAdvisory mirrors internal/server/admin.go's copy.
// The bounded-duplication convention is documented on largeDBAdvisory
// above: the CLI lives in package main and can't import the server
// package, so each shared advisory exists in both copies and must
// stay behaviourally identical.
func payloadOutlierAdvisory(rows []db.ToolCallPayloadRow) string {
	const (
		minRatio = 10.0
		minMax   = int64(100 * 1024)
	)
	type hit struct {
		tool  string
		ratio float64
		max   int64
		avg   float64
	}
	hits := []hit{}
	for _, r := range rows {
		if r.MaxBytes < minMax || r.AvgBytes <= 0 {
			continue
		}
		ratio := float64(r.MaxBytes) / r.AvgBytes
		if ratio < minRatio {
			continue
		}
		hits = append(hits, hit{tool: r.Tool, ratio: ratio, max: r.MaxBytes, avg: r.AvgBytes})
	}
	if len(hits) == 0 {
		return ""
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].ratio > hits[j].ratio })
	if len(hits) > 3 {
		hits = hits[:3]
	}
	parts := make([]string, 0, len(hits))
	for _, h := range hits {
		parts = append(parts, fmt.Sprintf("%s (max %s, avg %s, %.1f× spread)",
			h.tool, humanBytes(h.max), humanBytes(int64(h.avg)), h.ratio))
	}
	return "Payload outliers in the last 7 days — one or more tools occasionally returned a much larger " +
		"response than their average. These are the calls that blow up agent context windows: " +
		strings.Join(parts, "; ") +
		". Remediation: inspect the offending calls via /v1/tool-payload-stats or the dashboard's " +
		"Response Payload Size panel; consider narrowing the query (lower min_confidence, smaller k, " +
		"specific path filter) on calls to that tool to bound payload size."
}

// largeDBAdvisory returns a health advisory when the pincher DB is
// pathologically large, or "" when it's fine. #732: failure-as-pedagogy
// for the diagnostic itself — a multi-GB store is almost always stale
// projects accumulated by old binaries, not live data. `projects` must
// be sorted by symbol count descending so projects[0] is the heaviest.
//
// Deliberately duplicated from internal/server/admin.go's copy: the CLI
// lives in package main and can't import the server package. The admin.go
// header documents this bounded-duplication convention; the two copies
// must stay behaviourally identical.
func largeDBAdvisory(dbSizeBytes int64, projects []DoctorProjectSummary) string {
	const threshold = 1 << 30 // 1 GiB
	if dbSizeBytes <= threshold {
		return ""
	}
	msg := fmt.Sprintf("database is %.1f GB — unusually large. This is almost always "+
		"stale projects accumulated by older binaries, not live data.",
		float64(dbSizeBytes)/(1<<30))
	if len(projects) > 0 {
		h := projects[0]
		msg += fmt.Sprintf(" Heaviest project: %q (%d symbols across %d files).",
			h.Name, h.Symbols, h.Files)
	}
	msg += " Remediation: `list prune_dead=true` removes projects whose on-disk path is gone; " +
		"`pincher project prune-stale` drops projects indexed by an old schema and untouched for " +
		"30+ days. SQLite does not shrink the file on row deletion, so run `pincher vacuum` " +
		"afterward to actually reclaim the space."
	return msg
}

// formatDoctorMarkdown renders the report as a human-readable terminal
// summary. Compact-but-complete — fits in one screen for healthy installs,
// expands cleanly when there's something to surface.
func formatDoctorMarkdown(r *DoctorReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pincherMCP doctor — %s\n\n", r.GeneratedAt)

	// Binary + storage. Binary version surfaces in support paste-ins
	// without a separate `pincher --version` invocation; suppressed when
	// blank so a directly-built binary (no -ldflags) doesn't print
	// "Binary:           v" with an empty string after the v.
	if r.BinaryVersion != "" {
		fmt.Fprintf(&b, "Binary:           v%s\n", r.BinaryVersion)
	}
	fmt.Fprintf(&b, "Schema:           v%d\n", r.SchemaVersion)
	fmt.Fprintf(&b, "Database size:    %s\n", humanBytes(r.DBSizeBytes))
	fmt.Fprintf(&b, "WAL size:         %s\n", humanBytes(r.WALSizeBytes))
	fmt.Fprintf(&b, "Projects:         %d\n\n", len(r.Projects))

	// #732: health advisories — surfaced prominently right after the
	// storage block, since today's only advisory is about DB size.
	if len(r.Advisories) > 0 {
		fmt.Fprintln(&b, "⚠ Advisories:")
		for _, a := range r.Advisories {
			fmt.Fprintf(&b, "  - %s\n", a)
		}
		fmt.Fprintln(&b)
	}

	// Project list (top-N by symbol count, just the names)
	if len(r.Projects) > 0 {
		fmt.Fprintln(&b, "Projects (largest first):")
		for _, p := range r.Projects {
			marker := ""
			if p.Stale {
				marker = " [stale]"
			}
			fmt.Fprintf(&b, "  %-30s  files=%-6d  symbols=%-8d  edges=%d%s\n",
				truncMid(p.Name, 30), p.Files, p.Symbols, p.Edges, marker)
		}
		fmt.Fprintln(&b)
	}

	// Stale-projects roll-up (#236). Names every project that predates
	// the current binary's max-known schema version with the precise
	// reason ("indexed at v12, current is v15") so users know which to
	// re-index. Pre-v15 rows render as "predates the column" with no
	// version pinpoint since we genuinely don't know.
	var stale []DoctorProjectSummary
	for _, p := range r.Projects {
		if p.Stale {
			stale = append(stale, p)
		}
	}
	if len(stale) > 0 {
		fmt.Fprintf(&b, "Stale projects (would benefit from re-index): %d\n", len(stale))
		for _, p := range stale {
			fmt.Fprintf(&b, "  %-30s  %s\n", truncMid(p.Name, 30), p.StaleReason)
		}
		fmt.Fprintf(&b, "  Re-index a project: pincher index <path>\n\n")
	}

	// Extraction failures
	fmt.Fprintf(&b, "Extraction failures (last %d hours):  %d\n", r.LookbackHours, len(r.ExtractionFailures))
	for _, f := range r.ExtractionFailures {
		fmt.Fprintf(&b, "  [%s/%s] %s — %s\n", f.Project, f.Language, f.File, f.Reason)
		if f.Details != "" {
			fmt.Fprintf(&b, "      %s\n", truncEnd(f.Details, 80))
		}
	}
	// Roll-up by reason once the per-file list is too long to scan visually.
	// Surfaces the dominant failure mode at a glance — agents and humans
	// alike usually want "is this 30 of the same problem or 30 different
	// problems" before drilling in.
	if len(r.ExtractionFailures) > 5 {
		fmt.Fprintf(&b, "  → by reason: %s\n", summarizeByReason(r.ExtractionFailures))
	}
	if len(r.ExtractionFailures) > 0 {
		fmt.Fprintln(&b)
	}

	// Slow queries
	fmt.Fprintf(&b, "Slow queries (last %d hours):         %d\n", r.LookbackHours, len(r.SlowQueries))
	for _, sq := range r.SlowQueries {
		proj := sq.Project
		if proj == "" {
			proj = "(cross-project)"
		}
		fmt.Fprintf(&b, "  %-12s  %5dms  project=%s\n", sq.Tool, sq.DurationMS, proj)
	}
	if len(r.SlowQueries) > 0 {
		fmt.Fprintln(&b)
	}

	// Healthy-state footer when nothing's wrong
	if len(r.ExtractionFailures) == 0 && len(r.SlowQueries) == 0 {
		fmt.Fprintln(&b, "No diagnostic issues to report. ✓")
	}

	return b.String()
}

// humanBytes formats a byte count as KB/MB/GB.
func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k))
	}
}

func truncMid(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max < 8 {
		return s[:max]
	}
	half := (max - 1) / 2
	return s[:half] + "…" + s[len(s)-half:]
}

// summarizeByReason rolls up a failure list into "N reason1, M reason2"
// format, sorted by descending count then alphabetically for stable output.
// Returns "(none)" for an empty list — callers should already gate on
// length, but defensive output is cheap.
func summarizeByReason(rows []DoctorFailureRow) string {
	if len(rows) == 0 {
		return "(none)"
	}
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Reason]++
	}
	type pair struct {
		reason string
		count  int
	}
	pairs := make([]pair, 0, len(counts))
	for r, c := range counts {
		pairs = append(pairs, pair{r, c})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].reason < pairs[j].reason
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("%d %s", p.count, p.reason)
	}
	return strings.Join(parts, ", ")
}

func truncEnd(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
