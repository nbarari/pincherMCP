package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// #1259: context_for_task — the composite-context MCP tool.
//
// Why this exists (positioning frame): pincher's atomic tools (search /
// symbol / context / trace / changes) require the agent to assemble the
// investigation workflow itself. Token savings is the front-door pitch;
// composite-context is the *retention* lever — once an agent loop uses
// this tool, the loop rewires its planning to "ask pincher to compose
// the investigation" rather than "compose it manually from atomic
// tools." That's the routing-enablement story made concrete.
//
// One call replaces what was typically 5-10 atomic calls:
//   search(task)          → top-N seeds
//   context(seed) × N     → source + direct deps for each
//   trace(seed) × N       → callers + callees up to depth=2 each
//   changes overlap       → recent edits intersecting the seeds
//
// Returns one envelope: {seeds, neighbors, callers, callees,
// recent_changes}. `_meta.empty_reason` (#1252 enum) stamped when no
// seeds resolve.

func (s *Server) handleContextForTask(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	// #1590: FILE-H cancellation contract. The composite fans out per-seed
	// trace + neighborhood lookups; in deep traces that's 20+ DB calls.
	// Without ctx.Err() checks the handler runs to completion even after
	// the client cancelled, wasting work the caller already abandoned.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	task := str(args, "task")
	seedID := str(args, "seed_id")
	if task == "" && seedID == "" {
		// #712: failure-as-pedagogy — the caller passed neither anchor.
		// Show both shapes so they don't have to guess.
		return s.errResultRich(
			"context_for_task requires either `task` (free-form description) or `seed_id` (anchor symbol)",
			[]map[string]string{
				{"tool": "context_for_task", "args": `{"task":"fix the login retry bug"}`,
					"why": "describe the investigation in plain words — runs search then composes context+trace+changes"},
				{"tool": "context_for_task", "args": `{"seed_id":"internal/auth/login.go::auth.Retry#Function"}`,
					"why": "anchor on a known symbol — skips the search step and goes straight to context+trace+changes"},
			},
		), nil
	}
	if task != "" && seedID != "" {
		// Both passed — mutually exclusive per the InputSchema. Don't
		// silently pick one; surface the conflict so the caller learns
		// the contract.
		return s.errResultRich(
			"task and seed_id are mutually exclusive — pass one or the other",
			[]map[string]string{
				{"tool": "context_for_task", "args": fmt.Sprintf(`{"task":%q}`, task),
					"why": "drop seed_id to let search resolve seeds from the task description"},
				{"tool": "context_for_task", "args": fmt.Sprintf(`{"seed_id":%q}`, seedID),
					"why": "drop task to anchor strictly on the seed symbol"},
			},
		), nil
	}

	projectID, errRes := s.mustProject(args)
	if errRes != nil {
		return errRes, nil
	}

	maxSeeds := intArg(args, "max_seeds", 3)
	if maxSeeds <= 0 {
		maxSeeds = 3
	}
	if maxSeeds > 10 {
		maxSeeds = 10
	}
	traceDepth := intArg(args, "trace_depth", 2)
	if traceDepth <= 0 {
		traceDepth = 2
	}
	if traceDepth > 4 {
		traceDepth = 4
	}
	// include_changes defaults to true; intArg-style helper for bool
	// defaults isn't shared, so use boolArg and flip the absence default.
	includeChanges := true
	if v, ok := args["include_changes"]; ok {
		if b, ok := v.(bool); ok {
			includeChanges = b
		}
	}

	// ── Step 1: resolve seeds ──────────────────────────────────────────
	type seedRow struct {
		ID            string  `json:"id"`
		Name          string  `json:"name"`
		QualifiedName string  `json:"qualified_name"`
		Kind          string  `json:"kind"`
		FilePath      string  `json:"file_path"`
		StartLine     int     `json:"start_line"`
		EndLine       int     `json:"end_line"`
		Signature     string  `json:"signature,omitempty"`
		Score         float64 `json:"score,omitempty"`
	}
	seeds := []seedRow{}

	if seedID != "" {
		sym, err := s.store.GetSymbolScoped(projectID, seedID)
		if err == nil && sym != nil {
			seeds = append(seeds, seedRow{
				ID:            sym.ID,
				Name:          sym.Name,
				QualifiedName: sym.QualifiedName,
				Kind:          sym.Kind,
				FilePath:      sym.FilePath,
				StartLine:     sym.StartLine,
				EndLine:       sym.EndLine,
				Signature:     sym.Signature,
			})
		}
	} else {
		// Task-driven: BM25 search the code corpus with a coarse query.
		// The task string is freeform prose; SearchSymbolsByCorpus does
		// FTS5 BM25 ranking against name + qualified_name + signature +
		// docstring, which handles prose well enough for the
		// composite's purposes. Bias toward callable kinds since the
		// investigation target is almost always a Function/Method/
		// Class — Settings or Sections rarely anchor a bug-fix loop.
		//
		// #1440: prose tasks routinely contain too many tokens for
		// FTS5's default AND semantics — `classify a corpus file as
		// code or config` reads as "find symbols whose text contains
		// every word," which matches no single symbol. handleSearch
		// has an AND→OR fallback (server.go:5469); apply the same
		// recovery here so the composite doesn't fail to seed for
		// queries that plain `search` handles fine. Without this,
		// the composite's whole promise (one call replaces
		// search→context×N→trace×N) breaks for the natural prose
		// shape the tool was designed for.
		results, err := s.store.SearchSymbolsByCorpus(projectID, task, "", "", "code", maxSeeds*3)
		if err == nil && len(results) == 0 && !strings.Contains(task, `"`) {
			tokens := strings.Fields(task)
			if len(tokens) > 1 && !containsBareFTS5Operator(tokens) {
				sanitised := make([]string, len(tokens))
				for i, t := range tokens {
					sanitised[i] = wrapTokenIfNeeded(t)
				}
				orQuery := strings.Join(sanitised, " OR ")
				if orResults, orErr := s.store.SearchSymbolsByCorpus(projectID, orQuery, "", "", "code", maxSeeds*3); orErr == nil {
					results = orResults
				}
			}
		}
		if err == nil {
			for _, r := range results {
				if len(seeds) >= maxSeeds {
					break
				}
				// Filter to callable kinds for the composite. Document/
				// Setting hits in a code-corpus query are usually noise
				// when the caller named a fix/investigation task.
				switch r.Symbol.Kind {
				case "Function", "Method", "Class", "Interface", "Type":
					seeds = append(seeds, seedRow{
						ID:            r.Symbol.ID,
						Name:          r.Symbol.Name,
						QualifiedName: r.Symbol.QualifiedName,
						Kind:          r.Symbol.Kind,
						FilePath:      r.Symbol.FilePath,
						StartLine:     r.Symbol.StartLine,
						EndLine:       r.Symbol.EndLine,
						Signature:     r.Symbol.Signature,
						Score:         r.Score,
					})
				}
			}
		}
	}

	// ── Empty-seed exit path ──────────────────────────────────────────
	if len(seeds) == 0 {
		meta := map[string]any{}
		var diagnosis, reason string
		if seedID != "" {
			// #1591: seed_id didn't resolve is target-not-resolved, not
			// no-results-in-corpus. Aligns with v0.82's #1578 fix to
			// plan_change / investigate_failure: a missing anchor and an
			// empty corpus query are distinct failure shapes the
			// why_empty catalog answers differently.
			reason = EmptyReasonTargetNotResolved
			diagnosis = fmt.Sprintf("seed_id %q did not resolve in project %q — either the id is stale (try `search` for the symbol's name) or the wrong project is scoped", seedID, projectID)
		} else {
			reason = EmptyReasonNoResultsInCorpus
			diagnosis = fmt.Sprintf("task %q matched no callable symbols (Function/Method/Class/Interface/Type) in project %q — try a more specific keyword from a known symbol name, or call `search` directly to widen the corpus", task, projectID)
		}
		stampEmpty(meta, reason, diagnosis)
		meta["next_steps"] = []map[string]string{
			{"tool": "search", "args": fmt.Sprintf(`{"query":%q}`, taskOrSeedQuery(task, seedID)),
				"why": "run search directly to see what BM25 surfaces — context_for_task narrows to callable kinds, search doesn't"},
			{"tool": "guide", "args": fmt.Sprintf(`{"task":%q}`, task),
				"why": "guide returns 2-3 starter recommendations from a task description — use when the right vocabulary isn't obvious"},
		}
		data := map[string]any{
			"task":           task,
			"seed_id":        seedID,
			"seeds":          seeds,
			"neighbors":      []any{},
			"callers":        []any{},
			"callees":        []any{},
			"recent_changes": []any{},
			"_meta":          meta,
		}
		return s.jsonResultWithMeta(data, start, tool, args, 0), nil
	}

	// ── Step 2: per-seed callers + callees via trace BFS ───────────────
	// Each seed gets one outbound trace (callees) and one inbound trace
	// (callers), capped at traceDepth. Results carry a `via_seed` field
	// so the caller can attribute each hop to its anchor.
	type hopRow struct {
		ViaSeed       string `json:"via_seed"`
		ID            string `json:"id"`
		Name          string `json:"name"`
		QualifiedName string `json:"qualified_name"`
		Kind          string `json:"kind"`
		FilePath      string `json:"file_path"`
		Depth         int    `json:"depth"`
		ViaKind       string `json:"via_kind"`
	}
	callers := []hopRow{}
	callees := []hopRow{}

	// De-dupe across seeds — a callee reached from two seeds at different
	// depths only appears once at its min depth. Keyed by (id, direction)
	// because the same symbol may appear as both caller-of-A and
	// callee-of-B and that's two distinct hops.
	seenCaller := map[string]int{}
	seenCallee := map[string]int{}

	for _, seed := range seeds {
		// #1590: bail between seeds when the caller cancels — each seed
		// triggers 2 BFS traces + N GetSymbol calls.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Outbound = what the seed calls (callees).
		out, err := s.store.TraceViaCTEScoped(projectID, seed.ID, "outbound", []string{"CALLS"}, traceDepth)
		if err == nil {
			for _, h := range out {
				if h.SymbolID == seed.ID {
					continue
				}
				if prev, ok := seenCallee[h.SymbolID]; ok && prev <= h.Depth {
					continue
				}
				seenCallee[h.SymbolID] = h.Depth
				if sym, _ := s.store.GetSymbolScoped(projectID, h.SymbolID); sym != nil {
					callees = append(callees, hopRow{
						ViaSeed:       seed.ID,
						ID:            sym.ID,
						Name:          sym.Name,
						QualifiedName: sym.QualifiedName,
						Kind:          sym.Kind,
						FilePath:      sym.FilePath,
						Depth:         h.Depth,
						ViaKind:       h.ViaKind,
					})
				}
			}
		}
		// Inbound = who calls the seed (callers).
		in, err := s.store.TraceViaCTEScoped(projectID, seed.ID, "inbound", []string{"CALLS"}, traceDepth)
		if err == nil {
			for _, h := range in {
				if h.SymbolID == seed.ID {
					continue
				}
				if prev, ok := seenCaller[h.SymbolID]; ok && prev <= h.Depth {
					continue
				}
				seenCaller[h.SymbolID] = h.Depth
				if sym, _ := s.store.GetSymbolScoped(projectID, h.SymbolID); sym != nil {
					callers = append(callers, hopRow{
						ViaSeed:       seed.ID,
						ID:            sym.ID,
						Name:          sym.Name,
						QualifiedName: sym.QualifiedName,
						Kind:          sym.Kind,
						FilePath:      sym.FilePath,
						Depth:         h.Depth,
						ViaKind:       h.ViaKind,
					})
				}
			}
		}
	}

	// ── Step 3: neighbors — same-file siblings of each seed ────────────
	// One per-seed neighborhood lookup. De-duped across seeds by symbol
	// id. Caps at the first 50 to keep the composite payload bounded;
	// callers wanting full neighborhood paginate via the atomic tool.
	type neighborRow struct {
		ViaSeed       string `json:"via_seed"`
		ID            string `json:"id"`
		Name          string `json:"name"`
		QualifiedName string `json:"qualified_name"`
		Kind          string `json:"kind"`
		FilePath      string `json:"file_path"`
		StartLine     int    `json:"start_line"`
	}
	neighbors := []neighborRow{}
	seenNeighbor := map[string]bool{}
	const maxNeighbors = 50
	for _, seed := range seeds {
		// #1590: bail between seeds — neighbor pass after the trace pass.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(neighbors) >= maxNeighbors {
			break
		}
		siblings, err := s.store.GetSymbolsForFile(projectID, seed.FilePath)
		if err != nil {
			continue
		}
		for _, sib := range siblings {
			if len(neighbors) >= maxNeighbors {
				break
			}
			if sib.ID == seed.ID || seenNeighbor[sib.ID] {
				continue
			}
			seenNeighbor[sib.ID] = true
			neighbors = append(neighbors, neighborRow{
				ViaSeed:       seed.ID,
				ID:            sib.ID,
				Name:          sib.Name,
				QualifiedName: sib.QualifiedName,
				Kind:          sib.Kind,
				FilePath:      sib.FilePath,
				StartLine:     sib.StartLine,
			})
		}
	}

	// ── Step 4: recent_changes overlap (optional) ──────────────────────
	// Git-diff intersection against the seed file paths. include_changes
	// defaults true; callers can flip it off when the working tree is
	// known-clean or the composite is being used for read-only navigation.
	type changeRow struct {
		FilePath string `json:"file_path"`
		Hunks    int    `json:"hunks"`
	}
	recentChanges := []changeRow{}
	if includeChanges {
		// Collect distinct seed file paths to scope the diff intersection.
		seedFiles := map[string]bool{}
		for _, seed := range seeds {
			seedFiles[seed.FilePath] = true
		}
		// changedFiles is computed via the same path handleChanges uses.
		// On a clean working tree this returns nil; on a project with
		// no .git root it returns nil with no error — both cases yield
		// an empty recent_changes array, which is the correct signal.
		if proj, err := s.store.GetProject(projectID); err == nil && proj != nil {
			if raw, derr := runGitDiff(proj.Path, "all"); derr == nil && raw != "" {
				for _, fp := range parseGitDiffFiles(raw) {
					normalised := strings.ReplaceAll(fp, "\\", "/")
					if seedFiles[normalised] {
						recentChanges = append(recentChanges, changeRow{
							FilePath: normalised,
							// Hunks count requires a deeper diff parse;
							// the composite is signal-shaped, not a
							// replacement for `changes` proper. Leave
							// at 0 — callers wanting hunk detail call
							// `changes` directly.
							Hunks: 0,
						})
					}
				}
			}
		}
	}

	// ── Step 5: composite envelope + meta ─────────────────────────────
	data := map[string]any{
		"task":           task,
		"seed_id":        seedID,
		"seeds":          seeds,
		"neighbors":      neighbors,
		"callers":        callers,
		"callees":        callees,
		"recent_changes": recentChanges,
	}
	// Note: jsonResultWithMeta stamps the standard envelope. The composite
	// doesn't need its own empty_reason on the success path — the empty
	// branch above stamps it when seeds is empty.
	return s.jsonResultWithMeta(data, start, tool, args, 0), nil
}

// taskOrSeedQuery picks the more searchable of the two anchors for the
// next-step search suggestion in the empty path. Prefers the task
// string when present (more BM25-friendly); falls back to the seed_id's
// short-name (everything after the last "::").
func taskOrSeedQuery(task, seedID string) string {
	if task != "" {
		return task
	}
	return shortNameFromID(seedID)
}
