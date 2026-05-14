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
	// #293: pagination. The pre-fix tool dumped every symbol in the
	// file, which blew the response budget on big files
	// (server.go = 114 symbols → 54KB). Default to 50 so a
	// signature-only response stays around 5-10KB; offset lets the
	// agent page when the count exceeds the limit.
	limit := intArg(args, "limit", 50)
	offset := intArg(args, "offset", 0)
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	// Resolve project the same way handleSymbol does so a request
	// authenticated for project A can't accidentally read project B.
	var resolvedProjectID string
	if projectArg != "" {
		if pid, err := s.resolveProjectID(projectArg); err == nil {
			resolvedProjectID = pid
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
		return s.errResultRich(
			fmt.Sprintf("symbol %q not found", id),
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
	// `count` and adjust.
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
	// #293: when the response is a partial window, surface the next
	// page in _meta.next_steps so the agent doesn't need to compute
	// pagination math themselves.
	if end < totalNeighbors {
		data["_meta"] = map[string]any{
			"next_steps": []map[string]string{
				{
					"tool": "neighborhood",
					"args": fmt.Sprintf(`{"id":"%s","limit":%d,"offset":%d}`, seed.ID, limit, end),
					"why": fmt.Sprintf("file has %d neighbors total; you've seen %d-%d. Page to see the rest.",
						totalNeighbors, offset+1, end),
				},
			},
		}
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
