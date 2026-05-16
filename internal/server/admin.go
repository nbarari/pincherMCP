package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kwad77/pincher/internal/db"
	"github.com/kwad77/pincher/internal/index"
)

// admin.go hosts the three operations that previously had only a CLI
// surface (`pincher doctor`, `rebuild-fts`, `self-test`). Promoting them
// to MCP tools (#558 phase 2) means they're also reachable over HTTP
// via the `/v1/<tool>` dispatcher built in #560 — dashboards and ops
// automations can poll them without shelling out.
//
// Implementation note: the CLI commands live in package main, so we
// can't share code by import. The duplication is bounded (~80 LOC per
// op) and the CLI implementations are tested separately. The ground
// truth is the Store APIs both surfaces call (`ListProjects`,
// `ListExtractionFailures`, `ListSlowQueries`, `RebuildFTS`).

// doctorProjectSummary is one row of the doctor report's projects list.
// Package-level (not function-local) so largeDBAdvisory can take a slice
// of them.
type doctorProjectSummary struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Path                 string `json:"path"`
	Files                int    `json:"files"`
	Symbols              int    `json:"symbols"`
	Edges                int    `json:"edges"`
	IndexedAt            string `json:"indexed_at"`
	SchemaVersionAtIndex *int   `json:"schema_version_at_index,omitempty"`
	BinaryVersion        string `json:"binary_version,omitempty"`
}

// ghostProjectAdvisory returns a human-readable health advisory when a
// project has substantial symbols but zero (or vanishingly few) edges —
// the "ghost-edges" signature of #815 / #836 (zelosMCP user report).
// Extraction half-succeeded: symbols got persisted but the resolver
// phase never ran (or its writes got lost). At scale, the broken
// project still answers `search` happily, then `trace`/`query` over
// the symbol returns zero rows that look like a real empty result.
// Pre-#1009, doctor reported the totals as bare numbers — a 368k-symbol
// project with 0 edges looked identical to a healthy 368k/N project in
// the listing.
//
// Two thresholds, both anchored in dogfood-observed numbers:
//
//   - Strict (Symbols >= 1000 && Edges == 0): the original #1009 gate.
//     Below 1000 symbols a true pure-config / pure-docs repo can
//     legitimately land at 0 edges. At 1000+ symbols the project
//     almost certainly contains code files, and code without edges
//     means the resolver lost its work.
//
//   - Ratio (Symbols >= 1000 && Edges/Symbols < 0.001): the #1010
//     extension. Some ghost projects leak a handful of edges but the
//     resolver-failure shape is the same. Empirical: warp_rc indexed
//     at 1.4M symbols / 247 edges (ratio 0.000175) — clearly ghost
//     but the strict gate missed it. Healthy projects on the same DB
//     sit at >= 0.01 edges/symbol (Codex hits 0.040 in worst case);
//     the 0.001 floor is two orders of magnitude below the lowest
//     healthy ratio.
func ghostProjectAdvisory(projects []doctorProjectSummary) string {
	const symThreshold = 1000
	// #1010: ratio floor for the "vanishingly few edges" arm. Picked
	// to sit two orders of magnitude below the worst-case healthy
	// project's ratio (~0.04 on the dogfood box). Tightening below
	// 0.001 risks tripping on legitimate config-heavy repos with a
	// small Go cmd directory; widening above risks missing the
	// ratio-class ghosts the strict gate already misses.
	const minHealthyRatio = 0.001
	var ghosts []doctorProjectSummary
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
	// Limit to the worst 3 by symbol count so the advisory stays
	// scannable. projects is pre-sorted by symbol count desc, but
	// ghosts inherits that ordering since we walked in order.
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

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// walBloatAdvisory returns a human-readable health advisory when the
// pincher WAL has grown well past its configured 256 MiB
// journal_size_limit, or "" when it's fine. #1206 v0.66 DOGFOOD: WAL
// bloat is the hidden symptom of checkpoint failure under sustained
// concurrent indexing pressure (readers pin the WAL across the
// truncate). The user doesn't see it until vacuum or until a tool
// call latency blows out (the WAL has to be touched on every read).
//
// Threshold: 512 MiB OR > 10% of the DB. Either is large enough to
// merit surfacing; smaller WALs aren't a problem worth a warning.
// Remediation: `pincher vacuum` runs a wal_checkpoint(TRUNCATE)
// + VACUUM cycle, which reclaims the WAL space. Tracking issue
// #1206 / related #1149 (vacuum's own wal_reader_busy advisory).
func walBloatAdvisory(dbSizeBytes, walSizeBytes int64) string {
	const absThreshold = 512 << 20 // 512 MiB
	// The percent rule (WAL > 10% of DB) is only meaningful once the
	// DB itself is large enough that the ratio reflects real bloat.
	// Test DBs and fresh installs land at a few KB of data; 1 MB WAL
	// is normal there and not worth surfacing. Skip the percent rule
	// below 100 MiB DB size.
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

// largeDBAdvisory returns a human-readable health advisory when the
// pincher DB is pathologically large, or "" when it's fine. #732:
// failure-as-pedagogy for the diagnostic itself — `doctor` used to
// report db_size_bytes as a bare number, so a 4.7 GB store looked no
// different from a 4.7 MB one. A multi-GB store is almost always stale
// projects accumulated by old binaries, not live data. `projects` must
// be sorted by symbol count descending (handleDoctor already does this)
// so projects[0] is the heaviest — naming it makes the advisory
// concrete.
func largeDBAdvisory(dbSizeBytes int64, projects []doctorProjectSummary) string {
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

// nestedProjectAdvisory returns a human-readable advisory when one
// indexed project's path is a strict subdirectory of another's. Same
// source files end up indexed under two different project IDs, which
// (a) doubles disk pressure on the symbols/edges/FTS5 tables, and (b)
// makes audit-shape queries against the parent project surface symbols
// that the user thinks of as belonging to the child project — even
// after the per-project collision detector (#1209 part 1) correctly
// keeps the rows distinct, the user-visible duplication is the
// confusing part.
//
// Discovery context: v0.66 DOGFOOD found `warp_rc` (1.45M syms) and
// `warp-fork` (1.07M syms, located at `warp_rc/warp-fork/`) both
// registered. The user did not realize the inner project was a strict
// subdir of the outer; doctor's advisory list said nothing about it.
//
// Algorithm: for every pair (A, B) where A != B, B is nested under A
// iff normalize(B.Path) has prefix normalize(A.Path)+sep. Normalize
// case-folds and forward-slash-normalizes for Windows portability.
// Path-separator-after-parent guard avoids `warp_rc` falsely matching
// `warp_rc_fork`. The advisory names the inner project (the
// "redundant" one); the outer was almost always indexed first and is
// the one the user wants to keep.
//
// Caps at the worst 3 pairs by inner-project symbol count so the
// advisory stays scannable. #1209.
func nestedProjectAdvisory(projects []doctorProjectSummary) string {
	type nestedPair struct {
		inner doctorProjectSummary
		outer doctorProjectSummary
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
		// Find the most-specific outer that contains inner. Multiple
		// nested levels are possible (`a/`, `a/b/`, `a/b/c/`); naming
		// the immediate parent (`a/b/` for `a/b/c/`) is most actionable.
		var bestOuter *doctorProjectSummary
		var bestOuterLen int
		for j, outer := range projects {
			if i == j {
				continue
			}
			no := normalized[j]
			if no == "" {
				continue
			}
			// Strict prefix with trailing separator to avoid
			// `warp_rc` matching `warp_rc_fork`. ni and no are
			// already lowercased + forward-slash; the trailing
			// "/" sentinel is the inner's separator boundary.
			if len(ni) > len(no)+1 &&
				strings.HasPrefix(ni, no+"/") &&
				len(no) > bestOuterLen {
				outerCopy := outer
				bestOuter = &outerCopy
				bestOuterLen = len(no)
			}
		}
		if bestOuter != nil {
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

// normalizePathForNesting lowercases and forward-slash-normalizes a
// project path for case-insensitive prefix comparison. Windows uses
// `D:\ClaudeCode\warp_rc` while macOS / Linux use `/home/u/warp_rc`;
// both need to compare against their nested children under one rule.
// Returns "" for empty input so the caller can skip projects with no
// recorded path.
func normalizePathForNesting(path string) string {
	if path == "" {
		return ""
	}
	low := strings.ToLower(path)
	low = strings.ReplaceAll(low, `\`, "/")
	// Trim trailing slash so the prefix-with-"/"+sep check below is
	// unambiguous regardless of whether the stored path ended in a
	// separator.
	return strings.TrimRight(low, "/")
}

// handleDoctor builds a structured diagnostic report from the live DB:
// schema version, file sizes, project staleness, recent extraction
// failures, recent slow queries. Read-only — no DB mutations.
func (s *Server) handleDoctor(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)
	rawLookback := intArg(args, "lookback_hours", 168)
	rawTop := intArg(args, "top", 10)
	lookbackHours := rawLookback
	top := rawTop
	// #1015: surface input clamps so caller sees what defaults landed.
	// Same shape as search (#879) / neighborhood (#1013) — silent clamps
	// leave the caller thinking they got what they asked for.
	var clampWarnings []string
	if lookbackHours <= 0 {
		if _, present := args["lookback_hours"]; present {
			clampWarnings = append(clampWarnings,
				fmt.Sprintf("doctor: lookback_hours=%d clamped to 168 (must be positive)", rawLookback))
		}
		lookbackHours = 168
	}
	if top <= 0 {
		if _, present := args["top"]; present {
			clampWarnings = append(clampWarnings,
				fmt.Sprintf("doctor: top=%d clamped to 10 (must be positive)", rawTop))
		}
		top = 10
	}
	// #1016 / #1054: upper-bound clamp. top governs three list caps
	// (projects, extraction_failures, slow_queries) AND the per-project
	// ListExtractionFailures call. Pre-#1016 top=99999 on a multi-project
	// install produced a 506 KB response that blew the MCP per-call
	// token cap. #1016 capped at 500 to mirror search/neighborhood, but
	// doctor returns three sections at `top` each PLUS per-row detail
	// (file paths, multi-line stack traces) — at top=500 the response
	// still ran ~218 KB and exceeded the MCP cap. Dogfood-discovered:
	// the agent gets "result exceeds maximum allowed tokens" with no
	// recovery affordance. Drop to 50 — three sections × 50 rows with
	// ~400-byte rows lands well inside the per-call budget. For deeper
	// enumeration use `list` (per-project pagination) or pinchQL
	// queries against extraction_failures / slow_queries directly.
	const maxTop = 50
	if top > maxTop {
		clampWarnings = append(clampWarnings,
			fmt.Sprintf("doctor: top=%d clamped to %d (max). The response carries 3 sections (projects, extraction_failures, slow_queries) each capped at `top` — higher caps blow the MCP per-call token budget. Use `list` for paginated project enumeration if you need more.", rawTop, maxTop))
		top = maxTop
	}

	data := map[string]any{
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
		"binary_version": s.version,
		"lookback_hours": lookbackHours,
	}
	for _, w := range clampWarnings {
		attachWarning(data, w)
	}

	var schemaVersion int
	if err := s.store.DB().QueryRow(`SELECT version FROM schema_version`).Scan(&schemaVersion); err != nil {
		schemaVersion = 0
	}
	data["schema_version"] = schemaVersion

	dbPath := s.store.Path
	var dbSizeBytes, walSizeBytes int64
	if info, err := os.Stat(dbPath); err == nil {
		dbSizeBytes = info.Size()
	}
	if info, err := os.Stat(dbPath + "-wal"); err == nil {
		walSizeBytes = info.Size()
	}
	data["db_size_bytes"] = dbSizeBytes
	data["wal_size_bytes"] = walSizeBytes

	projects := []doctorProjectSummary{}
	plist, err := s.store.ListProjects()
	if err == nil {
		for _, p := range plist {
			projects = append(projects, doctorProjectSummary{
				ID:                   p.ID,
				Name:                 p.Name,
				Path:                 p.Path,
				Files:                p.FileCount,
				Symbols:              p.SymCount,
				Edges:                p.EdgeCount,
				IndexedAt:            p.IndexedAt.Format(time.RFC3339),
				SchemaVersionAtIndex: p.SchemaVersionAtIndex,
				BinaryVersion:        p.BinaryVersion,
			})
		}
		sort.Slice(projects, func(i, j int) bool {
			return projects[i].Symbols > projects[j].Symbols
		})
	}
	// #575: cap projects to `top` to bound the response size on
	// multi-project installs. Pre-fix a 125-project install + default
	// top=10 returned all 125 projects (≈19 KB) plus all 198 failures
	// (≈80 KB) for a total ≈119 KB — exceeded the MCP per-call cap.
	// Sorted-by-symbol-count-desc above means we keep the largest
	// (most likely to surface a problem) first.
	if len(projects) > top {
		data["projects_truncated"] = len(projects) - top
		projects = projects[:top]
	}
	data["projects"] = projects

	// #732: failure-as-pedagogy for the diagnostic itself. doctor used
	// to report db_size_bytes as a bare number — a 4.7 GB store looked
	// no different from a 4.7 MB one. Surface an actionable advisory
	// when the DB is pathologically large. `projects` is already sorted
	// by symbol count desc, so projects[0] is the global heaviest even
	// after the `top` truncation above.
	advisories := []string{}
	if a := largeDBAdvisory(dbSizeBytes, projects); a != "" {
		advisories = append(advisories, a)
	}
	// #1206 v0.66 DOGFOOD: WAL bloat advisory. Pincher's WAL is
	// supposed to be bounded by journal_size_limit=256 MiB (set in
	// the schema config) + CheckpointTruncate() on every Index()
	// tail. journal_size_limit is a SOFT cap: SQLite honors it
	// after a successful checkpoint, but if checkpoints can't
	// complete (busy readers pin the WAL across the truncate), the
	// WAL grows past the limit. Real-world observation on an 11GB
	// DB: WAL reached 2.3GB (9x the configured limit) — caller has
	// no signal until vacuum surfaces the wal_reader_busy bit
	// (#1149), too late to remediate proactively.
	//
	// Threshold: 512 MiB OR > 10% of the DB. Either is large
	// enough that the user should know; smaller WALs aren't a
	// problem worth surfacing. Bare number with a remediation
	// hint, same shape as largeDBAdvisory.
	if a := walBloatAdvisory(dbSizeBytes, walSizeBytes); a != "" {
		advisories = append(advisories, a)
	}
	// #1009: ghost-project advisory. Walks the full project list (not
	// the `top`-truncated one) so a ghost project ranked below `top` by
	// symbol count is still surfaced. Re-fetch the full list cheaply —
	// the previous ListProjects round-trip is shadowed by truncation.
	// #1209: nested-project advisory uses the same full-list pass —
	// the inner project is often ranked below the outer by symbol count
	// (the resolver phase failed on the inner so it shows up as a small
	// ghost), so any `top`-truncated view risks dropping the pair.
	{
		all := []doctorProjectSummary{}
		for _, p := range plist {
			all = append(all, doctorProjectSummary{
				Name:    p.Name,
				Path:    p.Path,
				Files:   p.FileCount,
				Symbols: p.SymCount,
				Edges:   p.EdgeCount,
			})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Symbols > all[j].Symbols })
		if a := ghostProjectAdvisory(all); a != "" {
			advisories = append(advisories, a)
		}
		if a := nestedProjectAdvisory(all); a != "" {
			advisories = append(advisories, a)
		}
	}
	data["advisories"] = advisories

	type failureRow struct {
		Project    string `json:"project"`
		File       string `json:"file"`
		Language   string `json:"language"`
		Reason     string `json:"reason"`
		Details    string `json:"details"`
		LastSeenAt string `json:"last_seen_at"`
	}
	failures := []failureRow{}
	cutoff := time.Now().Add(-time.Duration(lookbackHours) * time.Hour)
	// #1205: pre-fix doctor looped `ListExtractionFailures(p.ID, top)` per
	// project — N round-trips on a 130-project install, each scanning the
	// per-project segment of the `(project_id, last_seen_at DESC)` index.
	// On an 11GB DB with WAL contention the loop alone burned ~60s.
	// Replace with one cross-project SELECT capped at `top`; an honest
	// truncation count comes from a cheap second COUNT against the same
	// cutoff. Project-name join happens in-memory from `plist` (already
	// fetched above).
	projectName := make(map[string]string, len(plist))
	for _, p := range plist {
		projectName[p.ID] = p.Name
	}
	cutoffUnix := cutoff.Unix()
	recentFails, err := s.store.ListRecentExtractionFailuresAcrossProjects(cutoffUnix, top)
	if err == nil {
		for _, f := range recentFails {
			failures = append(failures, failureRow{
				Project:    projectName[f.ProjectID],
				File:       f.FilePath,
				Language:   f.Language,
				Reason:     f.Reason,
				Details:    f.Details,
				LastSeenAt: f.LastSeenAt.Format(time.RFC3339),
			})
		}
	}
	failuresTruncated := 0
	if len(failures) >= top {
		if total, cerr := s.store.CountRecentExtractionFailuresAcrossProjects(cutoffUnix); cerr == nil && total > len(failures) {
			failuresTruncated = total - len(failures)
		}
	}
	data["extraction_failures"] = failures
	if failuresTruncated > 0 {
		data["extraction_failures_truncated"] = failuresTruncated
	}

	type slowRow struct {
		Tool       string `json:"tool"`
		Project    string `json:"project,omitempty"`
		DurationMS int64  `json:"duration_ms"`
		Arguments  string `json:"arguments"`
		OccurredAt string `json:"occurred_at"`
	}
	slow := []slowRow{}
	if rows, err := s.store.ListSlowQueries(top * 5); err == nil {
		for _, sq := range rows {
			if sq.OccurredAt.Before(cutoff) {
				continue
			}
			slow = append(slow, slowRow{
				Tool:       sq.Tool,
				Project:    sq.ProjectID,
				DurationMS: sq.DurationMS,
				Arguments:  sq.Arguments,
				OccurredAt: sq.OccurredAt.Format(time.RFC3339),
			})
			if len(slow) >= top {
				break
			}
		}
	}
	data["slow_queries"] = slow

	return s.jsonResultWithMeta(data, start, tool, args, 0), nil
}

// handleRebuildFTS rebuilds every FTS5 index from the canonical symbols
// table. Mutating; requires confirm=true to actually run. Without
// confirm, returns the projected work (current FTS row count) so callers
// can preview before triggering.
func (s *Server) handleRebuildFTS(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)
	confirm := boolArg(args, "confirm")

	if !confirm {
		var rows int64
		_ = s.store.DB().QueryRow(`SELECT COUNT(*) FROM symbols`).Scan(&rows)
		return s.jsonResultWithMeta(map[string]any{
			"dry_run":               true,
			"would_reindex_symbols": rows,
			"hint":                  "pass confirm=true to perform the rebuild",
		}, start, tool, args, 0), nil
	}

	t0 := time.Now()
	rows, err := s.store.RebuildFTS()
	if err != nil {
		return errResult(fmt.Sprintf("rebuild_fts: %v", err)), nil
	}
	return s.jsonResultWithMeta(map[string]any{
		"dry_run":          false,
		"rebuilt_rows":     rows,
		"duration_ms":      time.Since(t0).Milliseconds(),
	}, start, tool, args, 0), nil
}

// handleSelfTest exercises the open-DB → index → search → byte-offset
// retrieval loop against a synthetic Go project in a temp directory.
// Returns per-step pass/fail. The temp project is cleaned up before
// return; the caller's primary DB is untouched (we open a fresh DB in
// the temp dir so we don't pollute project counts on healthy installs).
func (s *Server) handleSelfTest(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	type stepResult struct {
		Label      string `json:"label"`
		OK         bool   `json:"ok"`
		DurationMS int64  `json:"duration_ms"`
		Error      string `json:"error,omitempty"`
	}
	steps := []stepResult{}
	allOK := true
	addStep := func(label string, dur time.Duration, err error) bool {
		ok := err == nil
		errMsg := ""
		if !ok {
			errMsg = err.Error()
			allOK = false
		}
		steps = append(steps, stepResult{
			Label:      label,
			OK:         ok,
			DurationMS: dur.Milliseconds(),
			Error:      errMsg,
		})
		return ok
	}

	tmp, err := os.MkdirTemp("", "pincher-selftest-*")
	if err != nil {
		return errResult(fmt.Sprintf("self_test: tmp dir: %v", err)), nil
	}
	defer os.RemoveAll(tmp)

	dataDir := filepath.Join(tmp, "data")
	projDir := filepath.Join(tmp, "project")

	// Step 1: open a fresh DB in tmp.
	t0 := time.Now()
	if err := os.MkdirAll(dataDir, 0o755); err == nil {
		_, err = db.Open(dataDir)
		if err == nil {
			// Reopen handle for subsequent steps.
		}
	}
	store, openErr := db.Open(dataDir)
	if !addStep("1/5 open database", time.Since(t0), openErr) {
		return s.jsonResultWithMeta(map[string]any{"ok": allOK, "steps": steps}, start, tool, args, 0), nil
	}
	defer store.Close()
	idx := index.New(store)

	// Step 2: synth project.
	t0 = time.Now()
	src := []byte(`package selftest

func SelfTestProbe(x int) int { return x + 1 }
`)
	if err := os.MkdirAll(projDir, 0o755); err == nil {
		err = os.WriteFile(filepath.Join(projDir, "main.go"), src, 0o644)
		if !addStep("2/5 create synthetic project", time.Since(t0), err) {
			return s.jsonResultWithMeta(map[string]any{"ok": allOK, "steps": steps}, start, tool, args, 0), nil
		}
	}

	// Step 3: index it.
	t0 = time.Now()
	res, err := idx.Index(ctx, projDir, false)
	if err == nil && res.Symbols == 0 {
		err = fmt.Errorf("indexer reported 0 symbols on a project with one Go function")
	}
	if !addStep("3/5 index the project", time.Since(t0), err) {
		return s.jsonResultWithMeta(map[string]any{"ok": allOK, "steps": steps}, start, tool, args, 0), nil
	}
	pid := db.ProjectIDFromPath(projDir)

	// Step 4: search.
	t0 = time.Now()
	results, err := store.SearchSymbolsByCorpus(pid, "SelfTestProbe", "", "", "code", 5)
	if err == nil && len(results) == 0 {
		err = fmt.Errorf("search for SelfTestProbe returned 0 results — FTS5 trigger may not be firing")
	}
	if !addStep("4/5 search for known symbol", time.Since(t0), err) {
		return s.jsonResultWithMeta(map[string]any{"ok": allOK, "steps": steps}, start, tool, args, 0), nil
	}
	symID := results[0].Symbol.ID

	// Step 5: byte-offset retrieval.
	t0 = time.Now()
	sym, err := store.GetSymbol(symID)
	if err == nil && sym == nil {
		err = fmt.Errorf("GetSymbol returned nil for ID %q surfaced by search", symID)
	}
	if err == nil {
		var srcOut string
		srcOut, err = index.ReadSymbolSource(projDir, *sym)
		if err == nil && srcOut == "" {
			err = fmt.Errorf("byte-offset retrieval returned empty string for non-empty symbol")
		}
	}
	addStep("5/5 retrieve symbol source via byte offsets", time.Since(t0), err)

	return s.jsonResultWithMeta(map[string]any{
		"ok":    allOK,
		"steps": steps,
	}, start, tool, args, 0), nil
}
