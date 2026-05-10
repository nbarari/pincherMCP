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
		return errResult("id is required"), nil
	}
	projectArg := str(args, "project")
	includeSource := boolArg(args, "include_source") // default false
	includeSelf := boolArg(args, "include_self")     // default false

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
		return errResult(fmt.Sprintf("symbol %q not found", id)), nil
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

	neighbors := make([]map[string]any, 0, len(siblings))
	for _, sym := range siblings {
		if !includeSelf && sym.ID == seed.ID {
			continue
		}
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
	fileSizeBytes := avgFileSize
	if root == "" {
		if r, rerr := s.resolveProjectRoot(projectID); rerr == nil {
			root = r
		}
	}
	if root != "" {
		if fi, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(seed.FilePath))); statErr == nil {
			fileSizeBytes = int(fi.Size())
		}
	}
	// The neighborhood response itself approximates file size only when
	// include_source=true; for the default (signatures only) the
	// response is much smaller, so the saving is correspondingly large.
	responseBytes := 0
	for _, n := range neighbors {
		if sig, ok := n["signature"].(string); ok {
			responseBytes += len(sig)
		}
		if src, ok := n["source"].(string); ok {
			responseBytes += len(src)
		}
	}
	tokensSaved := max(0, fileSizeBytes-responseBytes) / charsPerToken

	data := map[string]any{
		"seed_id":   seed.ID,
		"file_path": seed.FilePath,
		"language":  seed.Language,
		"neighbors": neighbors,
		"count":     len(neighbors),
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
