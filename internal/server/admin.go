package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// handleDoctor builds a structured diagnostic report from the live DB:
// schema version, file sizes, project staleness, recent extraction
// failures, recent slow queries. Read-only — no DB mutations.
func (s *Server) handleDoctor(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)
	lookbackHours := intArg(args, "lookback_hours", 168)
	top := intArg(args, "top", 10)
	if lookbackHours <= 0 {
		lookbackHours = 168
	}
	if top <= 0 {
		top = 10
	}

	data := map[string]any{
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
		"binary_version": s.version,
		"lookback_hours": lookbackHours,
	}

	var schemaVersion int
	if err := s.store.DB().QueryRow(`SELECT version FROM schema_version`).Scan(&schemaVersion); err != nil {
		schemaVersion = 0
	}
	data["schema_version"] = schemaVersion

	dbPath := s.store.Path
	if info, err := os.Stat(dbPath); err == nil {
		data["db_size_bytes"] = info.Size()
	} else {
		data["db_size_bytes"] = int64(0)
	}
	if info, err := os.Stat(dbPath + "-wal"); err == nil {
		data["wal_size_bytes"] = info.Size()
	} else {
		data["wal_size_bytes"] = int64(0)
	}

	type projectSummary struct {
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
	projects := []projectSummary{}
	plist, err := s.store.ListProjects()
	if err == nil {
		for _, p := range plist {
			projects = append(projects, projectSummary{
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
