package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
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
	fix := fs.Bool("fix", false, "Auto-resolve the safe subset of advisories (currently: VACUUM the DB when >50 MB of reclaimable space; prune extraction_failures rows whose last_seen_at predates their project's indexed_at). Destructive remediations (project deletion, force-reindex) stay explicit-action — run their targeted subcommands. (#1260 §3, #1382/#1386)")
	projectFilter := fs.String("project", "", "#1401 — restrict the projects + extraction_failures sections to projects whose name or id contains this case-insensitive substring (same shape as `pincher project rm` / `pincher verify --project`). Empty = all projects. Database-level advisories (large-DB / WAL / ghost / nested / branch drift) remain project-wide.")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pincher doctor [--json] [--fix] [--data-dir DIR] [--lookback HOURS] [--top N] [--project NAME|ID|SUBSTR]")
		fmt.Fprintln(os.Stderr, "  Prints a diagnostic report from the local pincher database.")
		fmt.Fprintln(os.Stderr, "  Pass --fix to auto-resolve the safe subset of advisories (VACUUM bloated DB, prune stale extraction_failures).")
		fmt.Fprintln(os.Stderr, "  Pass --project to scope the projects + extraction_failures sections to a single project.")
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

	// #1260 §3: --fix bypasses the diagnose path and runs the safe-
	// action allowlist directly. Mutually exclusive with the diagnose
	// modes; fix shares the same DB open + close.
	if *fix {
		runDoctorFixCLI(dir, *asJSON)
		return
	}

	store, err := db.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher: failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	report, err := buildDoctorReport(store, dir, *lookbackHours, *top, *projectFilter)
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
	// IsStale fires when last_seen_at predates the project's current
	// indexed_at — meaning the row was recorded in a prior pass but the
	// most-recent re-extraction of the same (project, file, reason) did
	// NOT re-record it. PruneExtractionFailuresForFile (#1319) handles
	// that case for files the indexer touched, but a project whose
	// binary was upgraded and never re-indexed since still surfaces
	// pre-upgrade rows here. Pre-fix the user couldn't distinguish
	// \"happening now\" from \"awaiting re-index to clear\" — high
	// alarm-fatigue surface in the doctor display. #1382.
	IsStale bool `json:"is_stale,omitempty"`
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
//
// projectFilter (#1401) is an optional case-insensitive substring
// matched against project name OR id (same shape as `pincher project
// rm` and `pincher verify --project`). Empty = include every project.
// Filtering applies to the projects + extraction_failures sections;
// the database-level advisories (large-DB / WAL / ghost / nested /
// branch drift) deliberately keep walking every project — they're
// store-wide signals, not per-project diagnostics.
func buildDoctorReport(store *db.Store, dir string, lookbackHours, top int, projectFilter string) (*DoctorReport, error) {
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
	// #1401 / #1404: pre-build the matched-id set so the projects loop
	// AND the extraction_failures section share one membership predicate.
	// Tiered resolution (exact id → exact name → substring) mirrors
	// `pincher project rm` and protects against the over-match #1404
	// surfaced (substring "pincher-repo" hit every nested corpus project
	// whose id contains the parent path). nil means "no filter — include
	// everything"; non-nil empty means "filter applied, nothing matched".
	matchedProjectIDs := matchedProjectIDsForFilter(projects, projectFilter)
	if err == nil {
		current := db.CurrentSchemaVersion()
		for _, p := range projects {
			if matchedProjectIDs != nil {
				if _, ok := matchedProjectIDs[p.ID]; !ok {
					continue
				}
			}
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
		if matchedProjectIDs != nil {
			if _, ok := matchedProjectIDs[p.ID]; !ok {
				continue
			}
		}
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
				IsStale:    isStaleFailure(f.LastSeenAt, p.IndexedAt),
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
	// #1303 Phase 2a: branch-drift advisory. Mirrors the MCP doctor's
	// logic in internal/server/admin.go — the two copies must stay
	// behaviourally identical (per CLAUDE.md bounded-duplication
	// convention).
	if plist, err := store.ListProjects(); err == nil {
		if a := branchDriftAdvisory(plist); a != "" {
			r.Advisories = append(r.Advisories, a)
		}
	}

	// #1507 v0.83: three advisories that landed first on the MCP doctor
	// side (#815/#1009 ghost-project, #1206 wal-bloat, #1209 nested-project)
	// are now mirrored to the CLI. Pre-fix `pincher doctor --json`
	// silently omitted them — a user running the CLI saw a different
	// advisory set than an agent calling the same logic via MCP. Each
	// advisory is a byte-for-byte port of internal/server/admin.go's
	// copy per the bounded-duplication convention.
	if a := ghostProjectAdvisory(r.Projects); a != "" {
		r.Advisories = append(r.Advisories, a)
	}
	if a := walBloatAdvisory(r.DBSizeBytes, r.WALSizeBytes); a != "" {
		r.Advisories = append(r.Advisories, a)
	}
	if a := nestedProjectAdvisory(r.Projects); a != "" {
		r.Advisories = append(r.Advisories, a)
	}
	// #1635 v0.85: PreToolUse hook missing. When Claude Code looks
	// present (.claude/ in cwd, or ~/.claude/settings.json exists) AND
	// no `pincher hook-check` PreToolUse entry is wired up in any known
	// settings location, the policy in CLAUDE.md is enforced only by
	// agent self-policing. The runtime redirect is the load-bearing
	// mechanism for measurable usage growth — surface its absence so
	// users can run `pincher init --target=claude`.
	if a := hookInstallAdvisory(); a != "" {
		r.Advisories = append(r.Advisories, a)
	}
	// #1612 v0.87: FTS5 fragmentation advisory. Mirrors the MCP doctor's
	// copy in internal/server/admin.go per the bounded-duplication
	// convention. Best-effort: missing shadow tables (pre-v9 schema or
	// partial migration) return no rows and the advisory stays silent.
	if fragRows, err := store.FTS5Fragmentation(); err == nil {
		if a := fts5FragmentationAdvisory(fragRows); a != "" {
			r.Advisories = append(r.Advisories, a)
		}
	}

	return r, nil
}

// branchDriftAdvisory mirrors internal/server/admin.go's copy.
// Bounded-duplication convention documented on largeDBAdvisory above.
func branchDriftAdvisory(projects []db.Project) string {
	const perProjectTimeout = 1 * time.Second
	const maxProjectsToProbe = 30
	const maxToShow = 5

	type drift struct {
		Name        string
		LastIndexed string
		OnDisk      string
	}
	var drifts []drift

	probed := 0
	for _, p := range projects {
		if probed >= maxProjectsToProbe {
			break
		}
		if p.CurrentBranch == "" {
			continue
		}
		// #1665 v0.87: suppress drift on testdata corpora / worktree
		// staging dirs. Mirrors internal/server/admin.go (#1644 +
		// #1665) per the bounded-duplication convention.
		if branchDriftSuppressedPath(p.Path) {
			continue
		}
		// #1667 v0.87: skip projects whose on-disk path no longer
		// exists OR no longer points to a directory. Both routes
		// are dead-path ghosts for `list prune_dead=true`, not
		// for `pincher index`. The IsDir check catches the case
		// where a project's path got replaced by a file of the
		// same name.
		if fi, err := os.Stat(p.Path); err != nil || !fi.IsDir() {
			continue
		}
		probed++
		ctx, cancel := context.WithTimeout(context.Background(), perProjectTimeout)
		out, err := exec.CommandContext(ctx, "git", "-C", p.Path, "rev-parse", "--abbrev-ref", "HEAD").Output()
		cancel()
		if err != nil {
			continue
		}
		onDisk := strings.TrimSpace(string(out))
		if onDisk == "" || onDisk == p.CurrentBranch {
			continue
		}
		if onDisk == "HEAD" {
			ctx2, cancel2 := context.WithTimeout(context.Background(), perProjectTimeout)
			sha, err := exec.CommandContext(ctx2, "git", "-C", p.Path, "rev-parse", "--short", "HEAD").Output()
			cancel2()
			if err != nil {
				continue
			}
			onDisk = strings.TrimSpace(string(sha))
			if onDisk == p.CurrentBranch {
				continue
			}
		}
		drifts = append(drifts, drift{Name: p.Name, LastIndexed: p.CurrentBranch, OnDisk: onDisk})
	}

	if len(drifts) == 0 {
		return ""
	}

	sort.Slice(drifts, func(i, j int) bool { return drifts[i].Name < drifts[j].Name })

	var b strings.Builder
	b.WriteString("Branch drift on ")
	if len(drifts) == 1 {
		b.WriteString("1 project: ")
	} else {
		fmt.Fprintf(&b, "%d projects: ", len(drifts))
	}
	show := drifts
	extra := 0
	if len(show) > maxToShow {
		extra = len(show) - maxToShow
		show = show[:maxToShow]
	}
	for i, d := range show {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s (indexed=%s, on-disk=%s)", d.Name, d.LastIndexed, d.OnDisk)
	}
	if extra > 0 {
		fmt.Fprintf(&b, "; +%d more", extra)
	}
	b.WriteString(". The on-disk branch differs from the one the project was last indexed against, " +
		"so symbol byte-offsets may point at the wrong spans for the current working tree. " +
		"Run `pincher index <path>` against each drifted project to refresh. See #1303.")
	return b.String()
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

// ghostProjectAdvisory mirrors internal/server/admin.go's copy (#815/#1009).
// Surfaces projects with substantial symbols but vanishingly few edges —
// extraction ran but the resolver phase produced no graph, so `trace` /
// `query` over these projects silently return zero rows.
//
// Thresholds (kept identical to the MCP copy): symThreshold = 1000 symbols
// (small projects with no edges aren't usually ghosts — they're just small),
// minHealthyRatio = 0.001 edges/symbol (two orders of magnitude below the
// worst-case healthy project's ratio, ~0.04 on the dogfood box).
//
// Deliberately duplicated per CLAUDE.md bounded-duplication convention.
func ghostProjectAdvisory(projects []DoctorProjectSummary) string {
	const symThreshold = 1000
	const minHealthyRatio = 0.001
	var ghosts []DoctorProjectSummary
	for _, p := range projects {
		if p.Symbols < symThreshold {
			continue
		}
		if p.Edges == 0 {
			ghosts = append(ghosts, p)
			continue
		}
		if float64(p.Edges)/float64(p.Symbols) < minHealthyRatio {
			ghosts = append(ghosts, p)
		}
	}
	if len(ghosts) == 0 {
		return ""
	}
	if len(ghosts) > 3 {
		ghosts = ghosts[:3]
	}
	var names []string
	for _, g := range ghosts {
		if g.Edges == 0 {
			names = append(names, fmt.Sprintf("%q (%d symbols, %d files, 0 edges)",
				g.Name, g.Symbols, g.Files))
		} else {
			names = append(names, fmt.Sprintf("%q (%d symbols, %d files, %d edges — ratio %.6f)",
				g.Name, g.Symbols, g.Files, g.Edges,
				float64(g.Edges)/float64(g.Symbols)))
		}
	}
	msg := fmt.Sprintf("project%s with substantial symbols but vanishingly few edges (ghost-extraction signature, #815): %s. ",
		pluralS(len(ghosts)), strings.Join(names, "; "))
	msg += "Symbols were extracted but the resolver phase produced no real graph — `trace` / `query` over these projects will silently return zero rows. " +
		"Remediation: re-index from a fresh CWD (`pincher index <path>`) and check `doctor`'s extraction_failures list for the underlying cause."
	return msg
}

// fts5FragmentationAdvisory mirrors internal/server/admin.go's copy
// (#1612 v0.87). Surfaces a `rebuild_fts` advisory when any per-corpus
// FTS5 vtab's data/idx ratio crosses the threshold. Deliberately
// duplicated per CLAUDE.md bounded-duplication convention — the CLI
// lives in package main and can't import the server package.
func fts5FragmentationAdvisory(rows []db.FTS5CorpusFragmentation) string {
	type fragged struct {
		corpus string
		ratio  float64
		data   int64
		idx    int64
	}
	var bad []fragged
	for _, r := range rows {
		if r.NeedsRebuild {
			bad = append(bad, fragged{r.Corpus, r.Ratio, r.DataRows, r.IdxRows})
		}
	}
	if len(bad) == 0 {
		return ""
	}
	var parts []string
	for _, b := range bad {
		parts = append(parts, fmt.Sprintf("%s corpus is %.1fx fragmented (%d data rows / %d index rows)",
			b.corpus, b.ratio, b.data, b.idx))
	}
	joined := strings.Join(parts, "; ")
	return fmt.Sprintf(
		"FTS5 fragmentation high — %s. Healthy ratio is 1–3x; values above %.0fx indicate accumulated "+
			"micro-segments that slow BM25 ranking. Remediation: `pincher rebuild_fts` (or call the "+
			"`rebuild_fts` MCP tool) — drops and rebuilds every per-corpus vtab from `symbols` in a "+
			"single transaction. Cost scales with symbol count: seconds on small repos, minutes on "+
			"large ones. Safe to run during normal operation — readers are blocked only for the "+
			"transaction's duration. See #1612.",
		joined, db.FTS5FragmentationThreshold)
}

// walBloatAdvisory mirrors internal/server/admin.go's copy (#1206).
// Fires when the WAL file is past the 256 MB journal_size_limit soft
// cap (absolute >= 512 MiB) OR when the WAL is more than 10% of a
// DB-of-at-least-100-MiB. Below 100 MiB DB size, WAL ratios aren't
// meaningful (test DBs and fresh installs land at a few KB).
//
// Deliberately duplicated per CLAUDE.md bounded-duplication convention.
func walBloatAdvisory(dbSizeBytes, walSizeBytes int64) string {
	const absThreshold = 512 << 20 // 512 MiB
	const minDBForPercent = 100 << 20 // 100 MiB
	percentBloat := dbSizeBytes >= minDBForPercent && walSizeBytes*10 > dbSizeBytes
	if walSizeBytes < absThreshold && !percentBloat {
		return ""
	}
	walMB := float64(walSizeBytes) / (1 << 20)
	msg := fmt.Sprintf("WAL file is %.0f MB — past the 256 MB journal_size_limit soft cap.", walMB)
	if dbSizeBytes > 0 {
		msg += fmt.Sprintf(" That's %.0f%% of the DB (%.1f GB).",
			float64(walSizeBytes)/float64(dbSizeBytes)*100,
			float64(dbSizeBytes)/(1<<30))
	}
	msg += " Under sustained indexing pressure, checkpoints can't always truncate the WAL " +
		"(busy readers pin pages across the cycle). Every tool call touches the WAL first, " +
		"so a large WAL inflates latency on otherwise-cheap reads. Remediation: run `pincher vacuum` " +
		"to force a checkpoint + truncate + reclaim cycle. See #1206."
	return msg
}

// nestedProjectAdvisory mirrors internal/server/admin.go's copy (#1209).
// Detects inner projects indexed under an outer project's path — both
// indexed separately, same source files double-counted, surfacing
// duplicates in cross-project queries. Names the inner (redundant) one.
//
// Path-separator-after-parent guard avoids `warp_rc` matching
// `warp_rc_fork`. Caps at worst 3 pairs by inner-project symbol count.
//
// Deliberately duplicated per CLAUDE.md bounded-duplication convention.
func nestedProjectAdvisory(projects []DoctorProjectSummary) string {
	type nestedPair struct {
		inner DoctorProjectSummary
		outer DoctorProjectSummary
	}
	var pairs []nestedPair
	normalized := make([]string, len(projects))
	for i, p := range projects {
		normalized[i] = normalizePathForNesting(p.Path)
	}
	for i, inner := range projects {
		ni := normalized[i]
		if ni == "" {
			continue
		}
		var bestOuter *DoctorProjectSummary
		var bestOuterLen int
		for j, outer := range projects {
			if i == j {
				continue
			}
			no := normalized[j]
			if no == "" {
				continue
			}
			if len(ni) > len(no)+1 &&
				strings.HasPrefix(ni, no+"/") &&
				len(no) > bestOuterLen {
				outerCopy := outer
				bestOuter = &outerCopy
				bestOuterLen = len(no)
			}
		}
		if bestOuter != nil {
			// #1644: mirrors internal/server/admin.go's intentional-
			// nesting suppression. Same patterns, same rationale —
			// testdata/corpus pinned fixtures + .atrium/work worktrees
			// are deliberately nested.
			if isIntentionallyNested(normalized[i], normalizePathForNesting(bestOuter.Path)) {
				continue
			}
			pairs = append(pairs, nestedPair{inner: inner, outer: *bestOuter})
		}
	}
	if len(pairs) == 0 {
		return ""
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].inner.Symbols > pairs[j].inner.Symbols
	})
	if len(pairs) > 3 {
		pairs = pairs[:3]
	}
	var names []string
	for _, p := range pairs {
		names = append(names, fmt.Sprintf("%q (%d symbols) is indexed inside %q",
			p.inner.Name, p.inner.Symbols, p.outer.Name))
	}
	msg := fmt.Sprintf("nested project registration%s: %s. ",
		pluralS(len(pairs)), strings.Join(names, "; "))
	msg += "Same source files are indexed under both project IDs, " +
		"doubling DB load and surfacing duplicates in cross-project queries. " +
		"Remediation: choose one root (usually the outer), `list prune_dead=true` " +
		"after deleting the redundant project from disk, or use `pincher project rm` " +
		"on the inner project if it remains on disk. See #1209."
	return msg
}

// branchDriftSuppressedPath mirrors internal/server/admin.go's copy
// (#1665 v0.87). Returns true when a project's own path lives under
// a convention where branch drift is meaningless (testdata corpora,
// worktree staging). Deliberately duplicated per CLAUDE.md
// bounded-duplication convention.
func branchDriftSuppressedPath(path string) bool {
	norm := normalizePathForNesting(path)
	for _, seg := range []string{
		"/testdata/corpus/", "testdata/corpus/",
		"/testdata/__fixtures__/", "testdata/__fixtures__/",
		"/.atrium/work/", ".atrium/work/",
	} {
		if strings.Contains(norm, seg) {
			return true
		}
	}
	return false
}

// isIntentionallyNested mirrors internal/server/admin.go's copy (#1644).
// Returns true when the inner project's relative path under the outer
// contains a known-intentional segment (testdata/corpus pinned fixtures,
// .atrium/work worktree convention). The advisory's remediation —
// `pincher project rm <inner>` — would be wrong in those cases.
//
// Both arguments must already be normalised by normalizePathForNesting.
//
// Deliberately duplicated per CLAUDE.md bounded-duplication convention.
func isIntentionallyNested(innerNorm, outerNorm string) bool {
	if !strings.HasPrefix(innerNorm, outerNorm+"/") {
		return false
	}
	rel := innerNorm[len(outerNorm)+1:]
	if rel == "" {
		return false
	}
	rel = "/" + rel + "/"
	suppressSegments := []string{
		"/testdata/corpus/",
		"/testdata/__fixtures__/",
		"/.atrium/work/",
	}
	for _, seg := range suppressSegments {
		if strings.Contains(rel, seg) {
			return true
		}
	}
	return false
}

// normalizePathForNesting mirrors internal/server/admin.go's copy.
// Lowercases, normalises path separators to forward slashes, and trims
// a trailing slash so the prefix check is unambiguous.
func normalizePathForNesting(path string) string {
	if path == "" {
		return ""
	}
	low := strings.ToLower(path)
	low = strings.ReplaceAll(low, `\`, "/")
	return strings.TrimRight(low, "/")
}

// pluralS mirrors internal/server/admin.go's copy. Returns "s" for any
// count other than 1 so messages can say "project" / "projects" via
// `project` + pluralS(n).
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// hookInstallAdvisory (#1635 v0.85) fires when Claude Code looks
// present in the environment AND the `pincher hook-check` PreToolUse
// hook isn't installed in any settings location we know to inspect.
// Returns "" (silent) when the hook is wired up, when Claude Code
// isn't detected at all, or when the inspection itself fails.
//
// Why this matters: pincher's Read/Grep → search redirect (#627) is
// the load-bearing mechanism for measurable usage growth — without it,
// agent compliance with the CLAUDE.md policy is the only enforcement,
// and `hook_invocations` (v24, #626) stays empty. A user can call
// `pincher stats` and see 158M tokens saved with 4258 calls but ZERO
// hook redirects, which is the silent failure mode this advisory
// surfaces. Pair with #1632's zero-result split to close the
// observability loop.
//
// Detection rule: settings files searched, in order — `.claude/settings.json`
// in cwd, then `~/.claude/settings.json`. Hook detected when any
// PreToolUse entry contains a command string with the substring
// `pincher hook-check` (matches the same idempotency check
// mergePincherHook uses, so `pincher hook-check --debug` etc. count).
//
// "Claude Code present" rule: at least one of (.claude/ directory in
// cwd, ~/.claude/settings.json exists, CLAUDE_CONFIG_DIR env set).
// Without ANY of those signals the advisory is silent — pincher
// shouldn't nag users running under a different agent harness.
//
// Mirrors a CLI-only check today. The MCP doctor side (admin.go)
// gets the same advisory in a follow-up PR; the CLI ships first
// because that's where dogfood-flagged the absence.
func hookInstallAdvisory() string {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	projectSettings := filepath.Join(cwd, ".claude", "settings.json")
	var homeSettings string
	if home != "" {
		homeSettings = filepath.Join(home, ".claude", "settings.json")
	}

	// "Claude Code present" gate. Three independent signals; any one
	// is enough. Empty home dir + empty cwd + no env var = silent.
	claudePresent := false
	if cwd != "" {
		if _, err := os.Stat(filepath.Join(cwd, ".claude")); err == nil {
			claudePresent = true
		}
	}
	if !claudePresent && homeSettings != "" {
		if _, err := os.Stat(homeSettings); err == nil {
			claudePresent = true
		}
	}
	if !claudePresent && os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		claudePresent = true
	}
	if !claudePresent {
		return ""
	}

	if hookFoundInSettings(projectSettings) || hookFoundInSettings(homeSettings) {
		return ""
	}

	return "PreToolUse hook is NOT installed. Pincher's Read/Grep → search redirect (#627) " +
		"is currently OFF; agent compliance with the policy in CLAUDE.md is the only enforcement. " +
		"Run `pincher init --target=claude` from this project's root to install the hook. " +
		"Once installed, `pincher hook-stats` will surface the redirect / conversion rate, and " +
		"`hook_invocations` accumulates per-call telemetry. See #1635."
}

// hookFoundInSettings returns true when the JSON file at path contains
// a PreToolUse hook whose command references `pincher hook-check`.
// Missing / unreadable / unparseable files return false silently —
// the advisory's "present?" check should never fail the doctor run.
func hookFoundInSettings(path string) bool {
	if path == "" {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return false
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	preToolUse, _ := hooks["PreToolUse"].([]any)
	for _, raw := range preToolUse {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		entryHooks, _ := entry["hooks"].([]any)
		for _, h := range entryHooks {
			cmd, _ := h.(map[string]any)
			if cmd == nil {
				continue
			}
			if c, _ := cmd["command"].(string); strings.Contains(c, "pincher hook-check") {
				return true
			}
		}
	}
	return false
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

// matchedProjectIDsForFilter implements the doctor/verify --project
// substring matching, but TIERED to match `pincher project rm`'s
// behavior (#1404 fix). Pre-fix the flat "name OR id substring" check
// over-matched on nested project IDs — e.g. `--project pincher-repo`
// returned every corpus sub-project under `pincher-repo/testdata/`
// because their full IDs contain the substring "pincher-repo".
//
// Tiered resolution:
//  1. Exact id match → that one project (case-sensitive — IDs are stable
//     paths, scripted callers benefit from determinism).
//  2. Exact (case-insensitive) name match → all hits (1 expected; >1
//     would be a legitimate name collision and we surface all of them).
//  3. Substring on name OR id → fallback when neither exact tier hit.
//
// Returns nil when filter == "" (no filter applied; caller treats nil
// as "no filter — include every project"). Returns a non-nil empty map
// when filter is set but nothing matched (filter applied, all projects
// excluded — distinct outcome from no-filter).
//
// Mirrors the tiered match in cmd/pinch/project.go::matchProject; the
// shape differs slightly (this returns a set, that returns slice + status)
// because the filter callers don't need disambiguation UX.
func matchedProjectIDsForFilter(ps []db.Project, filter string) map[string]struct{} {
	if filter == "" {
		return nil
	}
	hits := make(map[string]struct{})
	// 1. Exact id match (case-sensitive).
	for _, p := range ps {
		if p.ID == filter {
			hits[p.ID] = struct{}{}
			return hits
		}
	}
	// 2. Exact (case-insensitive) name match.
	for _, p := range ps {
		if strings.EqualFold(p.Name, filter) {
			hits[p.ID] = struct{}{}
		}
	}
	if len(hits) > 0 {
		return hits
	}
	// 3. Substring on name OR id.
	low := strings.ToLower(filter)
	for _, p := range ps {
		if strings.Contains(strings.ToLower(p.Name), low) || strings.Contains(strings.ToLower(p.ID), low) {
			hits[p.ID] = struct{}{}
		}
	}
	return hits
}

