package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/kwad77/pincher/internal/db"
)

// handleNeighborhood implements #247 #1: given a seed symbol ID,
// return every symbol in the same file ordered by byte offset.
//
// Why this exists: in-file refactor planning frequently touches multiple
// symbols (e.g. updating a struct, its constructor, and its methods all
// at once). The pre-fix options were:
//   - N `symbol` calls (one per neighbor) → multi-RTT, more tokens
//   - One `Read` of the whole file → reads the whole file even though
//     the agent only needs the symbol shapes
//
// `neighborhood` returns the same compact summaries `search` does for
// every symbol in the file in one round-trip. By default `source` is
// excluded so the response stays cheap; pass `include_source=true` to
// also fetch each neighbor's body via the existing byte-offset path.
func (s *Server) handleNeighborhood(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	id := str(args, "id")
	if id == "" {
		// #712: failure-as-pedagogy — the caller has no id; the way to
		// get one is `search` for the symbol by name.
		return s.errResultRich("id is required", []map[string]string{
			{"tool": "search", "args": `{"query":"<symbol-name>"}`,
				"why": "search returns symbol IDs — pass one back as neighborhood's id"},
		}), nil
	}
	projectArg := str(args, "project")
	includeSource := boolArg(args, "include_source") // default false
	includeSelf := boolArg(args, "include_self")     // default false
	// #1228 (thin-client umbrella PR 4): include_fixtures defaults to
	// false. Mirrors trace #1225 + query #1212 fixture-filter behaviour.
	// When the seed's file_path matches isTestFixturePath (testdata/,
	// __fixtures__/, test-fixtures/, etc.) and the caller didn't pass
	// include_fixtures=true, return a rich-error explaining why — the
	// seed is in a pinned-corpus fixture and the agent likely meant a
	// real source-tree symbol. Opt-out via include_fixtures=true.
	includeFixtures := boolArg(args, "include_fixtures")
	// #293: pagination. The pre-fix tool dumped every symbol in the
	// file, which blew the response budget on big files
	// (server.go = 114 symbols → 54KB). Default to 50 so a
	// signature-only response stays around 5-10KB; offset lets the
	// agent page when the count exceeds the limit.
	limit := intArg(args, "limit", 50)
	offset := intArg(args, "offset", 0)
	// #712: surface input clamps in _meta.warnings — same treatment as
	// search/list/trace. A silently-coerced negative offset or
	// non-positive limit leaves the caller thinking it got what it
	// asked for.
	var clampWarnings []string
	if limit <= 0 {
		clampWarnings = append(clampWarnings,
			fmt.Sprintf("neighborhood: limit=%d clamped to 50 (must be positive)", limit))
		limit = 50
	}
	// #1013: cap the upper bound. Pre-fix a caller passing limit=99999 got
	// "every symbol in the file" — on big files (server.go's 200+ symbols)
	// the response blew the per-call token cap and the agent saw an MCP
	// truncation error with no recovery path. Same shape as search's
	// #532 limit=500 cap with a clamp-warning so the caller knows they
	// hit it. 500 is the same ceiling search uses; neighborhood scopes to
	// one file so 500 will always be enough for real files.
	const maxLimit = 500
	if limit > maxLimit {
		clampWarnings = append(clampWarnings,
			fmt.Sprintf("neighborhood: limit=%d clamped to %d (max). Use offset to page deeper.", limit, maxLimit))
		limit = maxLimit
	}
	if offset < 0 {
		clampWarnings = append(clampWarnings,
			fmt.Sprintf("neighborhood: offset=%d clamped to 0 (must be >= 0)", offset))
		offset = 0
	}

	// Resolve project the same way handleSymbol does so a request
	// authenticated for project A can't accidentally read project B.
	// #1025: surface a clamp warning when the caller passed a project
	// name that doesn't resolve. Pre-fix, neighborhood silently fell
	// back to a global symbol lookup and returned siblings from
	// WHATEVER project happened to own the seed id — typo'd project
	// names looked successful. Same silent-fallback shape as #1023
	// (health) and #1024 (stats).
	var resolvedProjectID string
	// #1038: capture the project-resolve failure in a dedicated var so
	// it can be stacked into the not-found error message below. Pre-fix
	// the warning only surfaced in _meta.warnings on success — when
	// both the project AND the id were bogus, the error reported only
	// the id miss, and the agent didn't learn their project name was
	// also wrong until their next call. Mirrors #1037 (symbol).
	//
	// #1048: skip resolution when projectArg=="*" — that's the
	// documented cross-project sentinel (search/query accept it).
	// Neighborhood scopes by file, so cross-project is a no-op
	// semantically — but pre-fix `*` was treated as an unknown
	// project name and produced a "did not resolve" warning even
	// though the unscoped lookup returned the right answer. Same
	// fix applied to symbol + context.
	var projectResolveWarning string
	if projectArg != "" && projectArg != "*" {
		if pid, err := s.resolveProjectID(projectArg); err == nil {
			resolvedProjectID = pid
		} else {
			projectResolveWarning = fmt.Sprintf(
				"neighborhood: project %q did not resolve — falling back to global symbol lookup. Call `list` to see indexed projects.",
				projectArg)
			clampWarnings = append(clampWarnings, projectResolveWarning)
		}
	}

	var seed *db.Symbol
	var err error
	if resolvedProjectID != "" {
		seed, err = s.store.GetSymbolScoped(resolvedProjectID, id)
	} else {
		seed, err = s.store.GetSymbol(id)
	}
	if err != nil {
		return errResult(fmt.Sprintf("db error: %v", err)), nil
	}
	if seed == nil {
		// Same stale-ID redirect path handleSymbol uses.
		if newID, ok := s.store.ResolveStaleID(s.sessionID, id); ok {
			if resolvedProjectID != "" {
				seed, err = s.store.GetSymbolScoped(resolvedProjectID, newID)
			} else {
				seed, err = s.store.GetSymbol(newID)
			}
			if err != nil {
				return errResult(fmt.Sprintf("db error resolving stale id: %v", err)), nil
			}
		}
	}
	if seed == nil {
		// #704: not-found path carries `search` + `list` remediation,
		// matching handleSymbol and handleTrace. Stale-ID redirect via
		// symbol_moves was already attempted above; if it failed, the
		// next move is name-based discovery.
		//
		// #1038: stack the project-resolve warning into the error when
		// both args were bogus — same shape as #1037 (symbol).
		errMsg := fmt.Sprintf("symbol %q not found", id)
		if projectResolveWarning != "" {
			errMsg = projectResolveWarning + " ALSO: " + errMsg
		}
		return s.errResultRich(
			errMsg,
			[]map[string]string{
				{"tool": "search", "args": fmt.Sprintf(`{"query":%q}`, shortNameFromID(id)),
					"why": "id resolution failed (also tried symbol_moves redirect) — search by short name"},
				{"tool": "list", "args": "{}",
					"why": "if no project matches, the right project may not be indexed"},
			},
		), nil
	}

	// All symbols in the file the seed lives in. Already ordered by
	// byte offset per GetSymbolsForFile's contract.
	projectID := seed.ProjectID
	if resolvedProjectID != "" {
		projectID = resolvedProjectID
	}
	// #1232 (neighborhood arm): silent-cross-project default flipped
	// to strict-error. neighborhood is the most dangerous shape of
	// the three (symbol / context / neighborhood) — it returns up to
	// 500 in-file siblings, every one of them belonging to the
	// cross-project tree if the seed resolved off-session. Agents
	// using the neighbor list to plan an in-file refactor are planning
	// against the wrong file. The opt-in escape hatch is cross_project=
	// true; "*" and explicit-project bypass the strict guard.
	crossProjectAllowed := boolArg(args, "cross_project")
	if projectArg == "" && s.sessionID != "" && seed.ProjectID != s.sessionID && !crossProjectAllowed {
		return s.errResultRich(
			fmt.Sprintf(
				"seed %q is not in the session project %q — it exists only in project %q. Pre-#1232 neighborhood returned every in-file sibling from that other project silently (with a warning string); now it errors so callers using the neighbor list to plan in-file refactors can't accidentally edit the wrong tree. Re-issue with project=%q to use that project's data, or pass cross_project=true to opt back into the legacy silent-fallback behaviour.",
				id, s.sessionID, seed.ProjectID, seed.ProjectID,
			),
			[]map[string]string{
				{"tool": "neighborhood", "args": fmt.Sprintf(`{"id":%q,"project":%q}`, id, seed.ProjectID),
					"why": "use the project where this seed actually lives"},
				{"tool": "search", "args": fmt.Sprintf(`{"query":%q,"project":%q}`, shortNameFromID(id), s.sessionID),
					"why": "look for the equivalent symbol in the session project (which is where you probably meant to be)"},
				{"tool": "neighborhood", "args": fmt.Sprintf(`{"id":%q,"cross_project":true}`, id),
					"why": "opt into legacy silent-fallback behaviour — same data as before, no shape change"},
			},
		), nil
	}
	// #1051 (legacy path, now only reached when cross_project=true):
	// preserve the warning string so the opt-in caller still gets the
	// programmatic signal that the lookup crossed project boundaries.
	if projectArg == "" && s.sessionID != "" && seed.ProjectID != s.sessionID {
		// crossProjectAllowed must be true at this point.
		clampWarnings = append(clampWarnings, fmt.Sprintf(
			"seed %q resolved from project %q rather than the session project %q (cross_project=true). The neighbor list + every file_path below belongs to the off-tree project. Re-issue with project=%q to pin scope, or project=%q if you intended that source.",
			id, seed.ProjectID, s.sessionID, s.sessionID, seed.ProjectID,
		))
	}
	// #1228: seed-in-fixture rich-error. neighborhood scopes by file,
	// so a fixture-path seed means the entire neighbor list lives in
	// a pinned-corpus fixture. The agent likely meant a real source-
	// tree symbol; refuse cleanly with the opt-in remediation rather
	// than silently returning a list of test inputs.
	if !includeFixtures && isTestFixturePath(seed.FilePath) {
		return s.errResultRich(
			fmt.Sprintf(
				"seed %q lives in a pinned-corpus fixture path (%q). neighborhood scopes to that file, so the neighbor list would be entirely fixture inputs — likely not what you meant. Pass include_fixtures=true to opt in, or pick a seed from real source code.",
				id, seed.FilePath,
			),
			[]map[string]string{
				{"tool": "neighborhood", "args": fmt.Sprintf(`{"id":%q,"include_fixtures":true}`, id),
					"why": "opt into surfacing fixture-path neighbors"},
				{"tool": "search", "args": fmt.Sprintf(`{"query":%q}`, shortNameFromID(id)),
					"why": "find an equivalent symbol outside the fixture corpus"},
			},
		), nil
	}
	siblings, err := s.store.GetSymbolsForFile(projectID, seed.FilePath)
	if err != nil {
		return errResult(fmt.Sprintf("db error fetching siblings: %v", err)), nil
	}

	// Resolve project root once for source reads (only if requested).
	root := ""
	if includeSource {
		if r, rerr := s.resolveProjectRoot(projectID); rerr == nil {
			root = r
		}
	}

	// totalNeighbors is the count after the include_self filter but
	// before pagination. Returned as `count` so the caller can decide
	// whether to page; the slice we ship back is `neighbors`.
	var filtered []db.Symbol
	for _, sym := range siblings {
		if !includeSelf && sym.ID == seed.ID {
			continue
		}
		filtered = append(filtered, sym)
	}
	totalNeighbors := len(filtered)

	// Apply offset / limit window. Out-of-range offsets clamp to a
	// zero-length slice rather than erroring — caller can still see
	// `count` and adjust. #1035: capture the original offset BEFORE the
	// silent clamp so the overshoot diagnosis below can name what the
	// caller actually passed.
	origOffset := offset
	end := offset + limit
	if offset > totalNeighbors {
		offset = totalNeighbors
	}
	if end > totalNeighbors {
		end = totalNeighbors
	}
	page := filtered[offset:end]

	neighbors := make([]map[string]any, 0, len(page))
	for _, sym := range page {
		entry := map[string]any{
			"id":                    sym.ID,
			"name":                  sym.Name,
			"qualified_name":        sym.QualifiedName,
			"kind":                  sym.Kind,
			"start_line":            sym.StartLine,
			"end_line":              sym.EndLine,
			"start_byte":            sym.StartByte,
			"end_byte":              sym.EndByte,
			"signature":             sym.Signature,
			"is_exported":           sym.IsExported,
			"extraction_confidence": sym.ExtractionConfidence,
		}
		if includeSource && root != "" {
			if src, _ := readSymbolSourceForNeighbor(root, sym); src != "" {
				entry["source"] = src
			}
		}
		neighbors = append(neighbors, entry)
	}

	// Tokens saved estimate: the alternative is a Read of the whole
	// file. Use the file's actual size so the metric reflects the real
	// per-call benefit on large files (where neighborhood pays off most).
	// On repeat access this session, the file is already in context —
	// charge 0 (#478).
	if root == "" {
		if r, rerr := s.resolveProjectRoot(projectID); rerr == nil {
			root = r
		}
	}
	tokensSaved := 0
	if s.markFileAccessed(projectID, seed.FilePath) {
		fileSizeBytes := avgFileSize
		if root != "" {
			if fi, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(seed.FilePath))); statErr == nil {
				fileSizeBytes = int(fi.Size())
			}
		}
		// The neighborhood response itself approximates file size only
		// when include_source=true; for the default (signatures only)
		// the response is much smaller, so the saving is
		// correspondingly large.
		responseBytes := 0
		for _, n := range neighbors {
			if sig, ok := n["signature"].(string); ok {
				responseBytes += len(sig)
			}
			if src, ok := n["source"].(string); ok {
				responseBytes += len(src)
			}
		}
		tokensSaved = max(0, fileSizeBytes-responseBytes) / charsPerToken
	}

	data := map[string]any{
		"seed_id":   seed.ID,
		"file_path": seed.FilePath,
		"language":  seed.Language,
		"neighbors": neighbors,
		// `count` is the total in the file (after the include_self
		// filter), not the page length — agents can compare against
		// `len(neighbors)` to decide whether to page (#293).
		"count": totalNeighbors,
		"page":  map[string]any{"limit": limit, "offset": offset, "returned": len(neighbors)},
	}
	// Surface a single staleness warning when source bytes were
	// requested and the file has been edited since indexing. Every
	// neighbor shares the same file, so one warning per call is the
	// right shape (vs N per-symbol warnings). Skipped when sources
	// weren't requested — the staleness only bites the byte-offset
	// read path, signatures alone stay correct. Same silent-
	// confidently-wrong family as #317/#960/#978.
	if includeSource && root != "" {
		s.attachStalenessWarning(data, projectID, seed, root)
	}
	// #293: when the response is a partial window, surface the next
	// page in _meta.next_steps so the agent doesn't need to compute
	// pagination math themselves. #1021: merge into existing _meta —
	// outright assignment clobbered the staleness warning that
	// attachStalenessWarning may have attached just above (same shape
	// of bug as #1020 in handleList).
	if end < totalNeighbors {
		meta, _ := data["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["next_steps"] = []map[string]string{
			{
				"tool": "neighborhood",
				"args": fmt.Sprintf(`{"id":"%s","limit":%d,"offset":%d}`, seed.ID, limit, end),
				"why": fmt.Sprintf("file has %d neighbors total; you've seen %d-%d. Page to see the rest.",
					totalNeighbors, offset+1, end),
			},
		}
		data["_meta"] = meta
	}
	// #1035: pagination-overshoot diagnosis. Pre-fix neighborhood
	// silently clamped offset to totalNeighbors and returned an empty
	// page with no signal the offset overshot. Agents reading
	// `count: 224, page.returned: 0` couldn't distinguish "file has
	// neighbors but I paged past them" from a degenerate seed. Same
	// shape as #1033 (search) / #1034 (list).
	if totalNeighbors > 0 && origOffset >= totalNeighbors {
		meta, _ := data["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		stampEmpty(meta, EmptyReasonCapDroppedAll, fmt.Sprintf(
			"offset=%d is past the end of the neighbor list (total=%d) — pagination overshoot. Pass offset=0 (or omit) to start at the top of the file.",
			origOffset, totalNeighbors))
		meta["next_steps"] = []map[string]string{
			{"tool": "neighborhood", "args": fmt.Sprintf(`{"id":%q}`, seed.ID),
				"why": fmt.Sprintf("retry without offset — the file has %d neighbor(s) at offset=0", totalNeighbors)},
		}
		data["_meta"] = meta
	}
	// #712: merge input-clamp warnings into _meta.warnings without
	// clobbering the next_steps the pagination branch may have set.
	if len(clampWarnings) > 0 {
		meta, _ := data["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			data["_meta"] = meta
		}
		existing, _ := meta["warnings"].([]string)
		meta["warnings"] = append(existing, clampWarnings...)
	}
	// #1145 (extends #858 honesty surface): when the project's edge
	// graph is empty because its dominant language has no cross-file
	// resolution, neighborhood returns same-file siblings only —
	// which can read like "this symbol is a leaf in the graph." Flag
	// the actual cause so the agent knows the file-scope view is the
	// complete picture, not a degenerate slice of a larger graph.
	if lang, gap := s.edgeGraphEmptyForLanguage(projectID); gap {
		meta, _ := data["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			data["_meta"] = meta
		}
		existing, _ := meta["warnings"].([]string)
		meta["warnings"] = append(existing, fmt.Sprintf(
			"This project is predominantly %s and its cross-file edge graph is empty (Go/Python only have edge resolution; tracked in #858). The %d same-file neighbors above are the complete structural view available — there is no graph-traversal layer to expand into.",
			lang, totalNeighbors))
	}
	return s.jsonResultWithMeta(data, start, tool, args, tokensSaved), nil
}

// readSymbolSourceForNeighbor wraps the byte-offset source read so the
// neighborhood handler doesn't have to reach into the index package for
// it. Mirrors handleSymbol's path; failure is silent (caller falls back
// to omitting the source field, not failing the whole call).
func readSymbolSourceForNeighbor(root string, sym db.Symbol) (string, error) {
	abs := filepath.Join(root, filepath.FromSlash(sym.FilePath))
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Seek(int64(sym.StartByte), 0); err != nil {
		return "", err
	}
	length := sym.EndByte - sym.StartByte
	if length <= 0 {
		return "", nil
	}
	buf := make([]byte, length)
	n, err := f.Read(buf)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(buf[:n]), "\r\n", "\n"), nil
}
