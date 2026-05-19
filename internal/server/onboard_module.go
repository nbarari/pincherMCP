package server

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// #1391 v0.84: onboard_module — new-contributor orientation composite.
// Phase 4 composite #4.
//
// Today (atomic):
//
//   architecture(aspects=[entry_points,packages])  → orient
//   search(file_pattern=path/**, label=Function)   → enumerate
//   trace(seed=entry_point, dir=out, depth=3)      → see what entry points reach
//   context(seed=top_call_target) × N              → read the implementation
//
// Composite. `onboard_module(directory_path, depth=3)` returns the
// shape declared in `docs/integrations/composite-tool-roadmap.md`:
//
//   {
//     "scope": {"directory": "...", "file_count": N, "symbol_count": N},
//     "entry_points_local_to_scope": [...],
//     "external_dependencies": [...],   // calls leaving the scope
//     "external_consumers":     [...],  // calls into the scope from outside
//     "module_summary": {
//       "language_breakdown":     {"Go": 0.92, "YAML": 0.08},
//       "test_to_code_ratio":     0.71,
//       "exported_surface_count": 18
//     }
//   }
//
// Read-only; no internal MCP round-trips per the composite contract.
// Idempotent.

type onboardScopeSummary struct {
	Directory   string `json:"directory"`
	FileCount   int    `json:"file_count"`
	SymbolCount int    `json:"symbol_count"`
}

type onboardSymbolRef struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	FilePath      string `json:"file_path"`
	StartLine     int    `json:"start_line"`
	Language      string `json:"language,omitempty"`
	Exported      bool   `json:"exported,omitempty"`
	Signature     string `json:"signature,omitempty"`
}

type onboardEdgeRef struct {
	FromID      string `json:"from_id"`
	FromName    string `json:"from_name"`
	FromInScope bool   `json:"from_in_scope"`
	ToID        string `json:"to_id"`
	ToName      string `json:"to_name"`
	ToInScope   bool   `json:"to_in_scope"`
	Kind        string `json:"kind"`
}

type onboardModuleSummary struct {
	LanguageBreakdown    map[string]float64 `json:"language_breakdown"`
	TestToCodeRatio      float64            `json:"test_to_code_ratio"`
	ExportedSurfaceCount int                `json:"exported_surface_count"`
	EntryPointCount      int                `json:"entry_point_count"`
}

func (s *Server) handleOnboardModule(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)
	// #1579 v0.82: composite cancellation contract. Entry-point check.
	if err := ctx.Err(); err != nil {
		return s.errResultRich("onboard_module: ctx canceled before scope query", nil), nil
	}

	directory := str(args, "directory")
	if strings.TrimSpace(directory) == "" {
		return s.errResultRich(
			"onboard_module requires `directory` — pass a relative directory path inside the indexed project (e.g. `internal/auth/` or `cmd/pinch/`). Use a trailing slash or not — both work.",
			[]map[string]string{
				{"tool": "onboard_module", "args": `{"directory":"internal/auth/"}`,
					"why": "orient on a single sub-package"},
				{"tool": "onboard_module", "args": `{"directory":"cmd/pinch/","depth":2}`,
					"why": "orient on the main entry-point package; shallower depth keeps the call graph readable"},
			},
		), nil
	}

	projectID, errRes := s.mustProject(args)
	if errRes != nil {
		return errRes, nil
	}

	depth := intArg(args, "depth", 3)
	if depth < 1 {
		depth = 1
	}
	if depth > 4 {
		depth = 4
	}

	// Normalise the directory: strip leading "./", normalise backslashes
	// to forward slashes, ensure a trailing "/" so the LIKE prefix only
	// matches files inside the directory (not sibling files starting
	// with the same prefix).
	dir := strings.ReplaceAll(directory, `\`, "/")
	dir = strings.TrimPrefix(dir, "./")
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}

	// ── Step 1: enumerate in-scope symbols by file_path prefix ──────────
	rows, err := s.store.RO().Query(
		`SELECT id, name, qualified_name, kind, language, file_path, start_line,
		        is_exported, is_test, is_entry_point, signature
		   FROM symbols
		  WHERE project_id = ? AND file_path LIKE ? ESCAPE '\'
		  ORDER BY file_path, start_line`,
		projectID, sqlLikeEscape(dir)+"%")
	if err != nil {
		return s.errResultRich(
			fmt.Sprintf("onboard_module: scope query failed: %v", err),
			[]map[string]string{
				{"tool": "doctor", "args": `{}`,
					"why": "verify the project has indexed symbols at all"},
			},
		), nil
	}
	defer rows.Close()

	type scopeSym struct {
		id, name, qn, kind, lang, file string
		startLine                      int
		exported, isTest, isEntry      bool
		signature                      string
	}
	var scopeSyms []scopeSym
	scopeIDs := map[string]bool{}
	files := map[string]bool{}
	langCounts := map[string]int{}
	testSymCount := 0
	codeSymCount := 0
	scanned := 0
	for rows.Next() {
		// #1595: check ctx every 100 rows. Large modules can have
		// thousands of in-scope symbols; without this the scope scan
		// runs to completion after the client cancels.
		scanned++
		if scanned%100 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		var s scopeSym
		var exp, test, entry int
		if err := rows.Scan(&s.id, &s.name, &s.qn, &s.kind, &s.lang, &s.file,
			&s.startLine, &exp, &test, &entry, &s.signature); err != nil {
			continue
		}
		s.exported = exp == 1
		s.isTest = test == 1
		s.isEntry = entry == 1
		scopeSyms = append(scopeSyms, s)
		scopeIDs[s.id] = true
		files[s.file] = true
		langCounts[s.lang]++
		if s.isTest {
			testSymCount++
		} else {
			codeSymCount++
		}
	}
	if err := rows.Err(); err != nil {
		// #1581: a mid-iteration rows.Err means we have PARTIAL data, not
		// no data. The earlier `defer to empty-handling branch` comment was
		// wrong — empty handling only fires when len(scopeSyms) == 0, which
		// silently passed partial-scope results through as "complete." That
		// produces a misleading language breakdown, missing entry-points,
		// undercount in ExportedSurfaceCount, and stale next-step ids.
		// Bail out with a rich error so the agent knows to retry rather
		// than acting on undercounted scope data.
		return s.errResultRich(
			fmt.Sprintf("onboard_module: scope iteration failed after %d row(s) — partial data discarded: %v", len(scopeSyms), err),
			[]map[string]string{
				{"tool": "doctor", "args": `{}`,
					"why": "rows.Err often means a transient connection or disk problem — doctor will surface it"},
				{"tool": "onboard_module", "args": fmt.Sprintf(`{"directory":%q}`, directory),
					"why": "retry — many rows.Err conditions are transient"},
			},
		), nil
	}

	scope := onboardScopeSummary{
		Directory:   dir,
		FileCount:   len(files),
		SymbolCount: len(scopeSyms),
	}

	if len(scopeSyms) == 0 {
		meta := map[string]any{}
		stampEmpty(meta, EmptyReasonNoResultsInCorpus,
			fmt.Sprintf("directory %q did not match any indexed symbols in project %q — either the path isn't indexed, the directory is empty/extractor-skipped, or the prefix is wrong (try with/without a leading 'internal/' or 'cmd/'). Use `list` to see indexed project paths.",
				directory, projectID))
		meta["next_steps"] = []map[string]string{
			{"tool": "list", "args": `{}`,
				"why": "confirm the project is indexed + its root path"},
			{"tool": "search", "args": fmt.Sprintf(`{"query":%q}`, path.Base(strings.TrimSuffix(dir, "/"))),
				"why": "BM25 may surface the right directory if the prefix is wrong"},
		}
		data := map[string]any{
			"scope":                       scope,
			"entry_points_local_to_scope": []onboardSymbolRef{},
			"external_dependencies":       []onboardEdgeRef{},
			"external_consumers":          []onboardEdgeRef{},
			"module_summary":              onboardModuleSummary{LanguageBreakdown: map[string]float64{}},
			"_meta":                       meta,
		}
		return s.jsonResultWithMeta(data, start, tool, args, 0), nil
	}

	// ── Step 2: entry points + exported surface within scope ────────────
	entryPoints := []onboardSymbolRef{}
	exportedCount := 0
	for _, sym := range scopeSyms {
		if sym.exported {
			exportedCount++
		}
		if sym.isEntry {
			entryPoints = append(entryPoints, onboardSymbolRef{
				ID:            sym.id,
				Name:          sym.name,
				QualifiedName: sym.qn,
				Kind:          sym.kind,
				FilePath:      sym.file,
				StartLine:     sym.startLine,
				Language:      sym.lang,
				Exported:      sym.exported,
				Signature:     sym.signature,
			})
		}
	}
	sort.Slice(entryPoints, func(i, j int) bool {
		if entryPoints[i].FilePath != entryPoints[j].FilePath {
			return entryPoints[i].FilePath < entryPoints[j].FilePath
		}
		return entryPoints[i].StartLine < entryPoints[j].StartLine
	})

	// ── Step 3: external dependencies + external consumers ──────────────
	// One scan over the edges table per project, partitioning each edge
	// by whether from_id and to_id land inside the scope. We only care
	// about CALLS for this composite — IMPORTS/READS/WRITES tend to
	// blow up the consumer list with implementation-detail edges.
	externalDeps := []onboardEdgeRef{}
	externalConsumers := []onboardEdgeRef{}
	edgeRows, err := s.store.RO().Query(
		`SELECT e.from_id, e.to_id, e.kind,
		        fs.name AS from_name, ts.name AS to_name
		   FROM edges e
		   LEFT JOIN symbols fs ON fs.id = e.from_id
		   LEFT JOIN symbols ts ON ts.id = e.to_id
		  WHERE e.project_id = ?
		    AND e.kind = 'CALLS'`,
		projectID)
	if err == nil {
		defer edgeRows.Close()
		edgesScanned := 0
		for edgeRows.Next() {
			// #1595: check ctx every 100 rows. Full CALLS edge scan can
			// be tens of thousands of rows on a large project.
			edgesScanned++
			if edgesScanned%100 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			var fromID, toID, kind string
			var fromName, toName *string
			if err := edgeRows.Scan(&fromID, &toID, &kind, &fromName, &toName); err != nil {
				continue
			}
			fromIn := scopeIDs[fromID]
			toIn := scopeIDs[toID]
			if fromIn == toIn {
				continue // both in or both out — not a boundary crossing
			}
			edge := onboardEdgeRef{
				FromID:      fromID,
				ToID:        toID,
				Kind:        kind,
				FromInScope: fromIn,
				ToInScope:   toIn,
			}
			if fromName != nil {
				edge.FromName = *fromName
			}
			if toName != nil {
				edge.ToName = *toName
			}
			if fromIn && !toIn {
				externalDeps = append(externalDeps, edge)
			} else if !fromIn && toIn {
				externalConsumers = append(externalConsumers, edge)
			}
		}
	}

	// Bound the lists — onboard_module is for orientation; thousands of
	// edges would drown the agent.
	const maxEdgesPerList = 50
	sort.SliceStable(externalDeps, func(i, j int) bool {
		return externalDeps[i].ToName < externalDeps[j].ToName
	})
	if len(externalDeps) > maxEdgesPerList {
		externalDeps = externalDeps[:maxEdgesPerList]
	}
	sort.SliceStable(externalConsumers, func(i, j int) bool {
		return externalConsumers[i].FromName < externalConsumers[j].FromName
	})
	if len(externalConsumers) > maxEdgesPerList {
		externalConsumers = externalConsumers[:maxEdgesPerList]
	}

	// ── Step 4: module summary ──────────────────────────────────────────
	langBreakdown := make(map[string]float64, len(langCounts))
	total := float64(len(scopeSyms))
	if total > 0 {
		for lang, c := range langCounts {
			langBreakdown[lang] = float64(c) / total
		}
	}
	var testRatio float64
	if codeSymCount > 0 {
		testRatio = float64(testSymCount) / float64(codeSymCount)
	}
	summary := onboardModuleSummary{
		LanguageBreakdown:    langBreakdown,
		TestToCodeRatio:      testRatio,
		ExportedSurfaceCount: exportedCount,
		EntryPointCount:      len(entryPoints),
	}

	meta := map[string]any{}
	meta["depth"] = depth
	meta["next_steps"] = []map[string]string{}
	if len(entryPoints) > 0 {
		meta["next_steps"] = append(meta["next_steps"].([]map[string]string),
			map[string]string{
				"tool": "context", "args": fmt.Sprintf(`{"id":%q}`, entryPoints[0].ID),
				"why": "read the first entry point's implementation",
			})
	}
	if len(externalDeps) > 0 {
		meta["next_steps"] = append(meta["next_steps"].([]map[string]string),
			map[string]string{
				"tool": "trace", "args": fmt.Sprintf(`{"id":%q,"direction":"outbound","depth":2}`, scopeSyms[0].id),
				"why": "follow a representative call out of scope",
			})
	}

	data := map[string]any{
		"scope":                       scope,
		"entry_points_local_to_scope": entryPoints,
		"external_dependencies":       externalDeps,
		"external_consumers":          externalConsumers,
		"module_summary":              summary,
		"_meta":                       meta,
	}
	return s.jsonResultWithMeta(data, start, tool, args, 0), nil
}

// sqlLikeEscape escapes the three LIKE-pattern metacharacters so an
// unsanitised directory like `cmd/[pinch]/` doesn't match more than
// intended. Paired with `ESCAPE '\'` in the query.
func sqlLikeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}
