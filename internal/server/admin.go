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
// project has substantial symbols but zero edges — the "ghost-edges"
// signature of #815 / #836 (zelosMCP user report). Extraction half-
// succeeded: symbols got persisted but the resolver phase never ran
// (or its writes got lost). At scale, the broken project still answers
// `search` happily, then `trace`/`query` over the symbol returns zero
// rows that look like a real empty result. Pre-#1009, doctor reported
// the totals as bare numbers — a 368k-symbol project with 0 edges
// looked identical to a healthy 368k/N project in the listing.
//
// Threshold = 1000 symbols. Below that, a true pure-config /
// pure-docs repo can legitimately land at 0 edges. At 1000+ symbols
// the project almost certainly contains code files, and code without
// edges means the resolver lost its work.
func ghostProjectAdvisory(projects []doctorProjectSummary) string {
	const symThreshold = 1000
	var ghosts []doctorProjectSummary
	for _, p := range projects {
		if p.Symbols >= symThreshold && p.Edges == 0 {
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
		names = append(names, fmt.Sprintf("%q (%d symbols, %d files)", g.Name, g.Symbols, g.Files))
	}
	msg := fmt.Sprintf("project%s with substantial symbols but ZERO edges (ghost-extraction signature, #815): %s. ",
		pluralS(len(ghosts)), strings.Join(names, "; "))
	msg += "Symbols were extracted but the resolver phase produced no graph — `trace` / `query` over these projects will silently return zero rows. " +
		"Remediation: re-index from a fresh CWD (`pincher index <path>`) and check `doctor`'s extraction_failures list for the underlying cause."
	return msg
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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
	// #1016: upper-bound clamp. top governs three list caps (projects,
	// extraction_failures, slow_queries) AND the per-project
	// ListExtractionFailures call. Pre-fix top=99999 on a multi-project
	// install produced a 506 KB response that blew the MCP per-call
	// token cap — agent saw a truncation error with no recovery path.
	// Same shape as search (#532) / neighborhood (#1013): 500 ceiling
	// with a clamp warning so the caller knows they hit it.
	const maxTop = 500
	if top > maxTop {
		clampWarnings = append(clampWarnings,
			fmt.Sprintf("doctor: top=%d clamped to %d (max). Use offset-style follow-up calls if you need to enumerate more.", rawTop, maxTop))
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
	// #1009: ghost-project advisory. Walks the full project list (not
	// the `top`-truncated one) so a ghost project ranked below `top` by
	// symbol count is still surfaced. Re-fetch the full list cheaply —
	// the previous ListProjects round-trip is shadowed by truncation.
	{
		all := []doctorProjectSummary{}
		for _, p := range plist {
			all = append(all, doctorProjectSummary{
				Name:    p.Name,
				Files:   p.FileCount,
				Symbols: p.SymCount,
				Edges:   p.EdgeCount,
			})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Symbols > all[j].Symbols })
		if a := ghostProjectAdvisory(all); a != "" {
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
	failuresTruncated := 0
collect:
	for _, p := range plist {
		// #575: keep pulling per-project for visibility across
		// projects (a single noisy project shouldn't crowd out
		// signals from healthier ones), but enforce `top` as a GLOBAL
		// cap. Pre-fix the per-project loop made the response grow
		// linearly with project count → 125 projects × 10 failures
		// blew the MCP token cap. Track how many we dropped so the
		// caller knows the count is capped.
		fails, err := s.store.ListExtractionFailures(p.ID, top)
		if err != nil {
			continue
		}
		for _, f := range fails {
			if f.LastSeenAt.Before(cutoff) {
				continue
			}
			if len(failures) >= top {
				failuresTruncated++
				continue
			}
			failures = append(failures, failureRow{
				Project:    p.Name,
				File:       f.FilePath,
				Language:   f.Language,
				Reason:     f.Reason,
				Details:    f.Details,
				LastSeenAt: f.LastSeenAt.Format(time.RFC3339),
			})
		}
		// Continue iterating to count the trailing rows we're
		// dropping (so the truncated count is honest), but bail once
		// we've quantified the overflow at >5× cap to avoid pointless
		// DB churn on enormous installs.
		if failuresTruncated > top*5 {
			break collect
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
