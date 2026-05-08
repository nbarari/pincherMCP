// Package index implements the pincherMCP file indexer.
//
// Pipeline per file:
//  1. Content-hash check (xxh3) — skip if file unchanged
//  2. AST extraction — symbols + edges with byte offsets
//  3. DB upsert — symbols into symbols table (triggers FTS5 sync)
//  4. DB upsert — edges into edges table
//  5. Hash update — record new hash for next incremental run
//
// All three indexes (byte-offset, graph, FTS5) are populated in a single pass.
package index

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boyter/gocodewalker"
	"github.com/pincherMCP/pincher/internal/ast"
	"github.com/pincherMCP/pincher/internal/db"
	"github.com/zeebo/xxh3"
)

// IndexProgress tracks live file-processing progress for a running index job.
type IndexProgress struct {
	FilesDone  atomic.Int64
	FilesTotal atomic.Int64
}

// Indexer manages repository indexing for pincherMCP.
type Indexer struct {
	store    *db.Store
	mu       sync.Mutex
	active   map[string]bool         // projectID → indexing in progress
	progress sync.Map                // projectID → *IndexProgress
}

// New creates a new Indexer.
func New(store *db.Store) *Indexer {
	return &Indexer{
		store:  store,
		active: make(map[string]bool),
	}
}

// GetProgress returns current file progress for the given project ID.
// Returns (done, total, active). If not currently indexing, active=false.
func (idx *Indexer) GetProgress(projectID string) (done, total int64, active bool) {
	idx.mu.Lock()
	active = idx.active[projectID]
	idx.mu.Unlock()
	if v, ok := idx.progress.Load(projectID); ok {
		p := v.(*IndexProgress)
		return p.FilesDone.Load(), p.FilesTotal.Load(), active
	}
	return 0, 0, active
}

// IndexResult summarises a completed indexing run.
type IndexResult struct {
	ProjectID  string
	Project    string
	Path       string
	Files      int
	Symbols    int
	Edges      int
	Skipped    int // files skipped (unchanged hash)
	Blocked    int // files refused by ast.ShouldSkip (lockfiles, minified bundles, source maps)
	DurationMS int64
}

// Index indexes a repository at the given path (incremental by default).
// If force=true, all files are re-parsed regardless of content hash.
func (idx *Indexer) Index(ctx context.Context, repoPath string, force bool) (*IndexResult, error) {
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}

	projectID := db.ProjectIDFromPath(absPath)
	projectName := db.ProjectNameFromPath(absPath)

	// Serialise per-project (in-process).
	idx.mu.Lock()
	if idx.active[projectID] {
		idx.mu.Unlock()
		return nil, fmt.Errorf("project %q is already being indexed", projectName)
	}
	idx.active[projectID] = true
	idx.mu.Unlock()
	prog := &IndexProgress{}
	idx.progress.Store(projectID, prog)
	defer func() {
		idx.mu.Lock()
		delete(idx.active, projectID)
		idx.mu.Unlock()
		idx.progress.Delete(projectID)
	}()

	// Serialise per-project (cross-process). Prevents two pincher processes
	// (e.g. MCP server + manual CLI, or two Claude Code sessions on the
	// same project) from running heavy index transactions in parallel.
	releaseLock, err := acquireProjectLock(filepath.Dir(idx.store.Path), projectID)
	if err != nil {
		return nil, err
	}
	defer releaseLock()

	start := time.Now()

	// Ensure project record exists
	if err := idx.store.UpsertProject(db.Project{
		ID:        projectID,
		Path:      absPath,
		Name:      projectName,
		IndexedAt: start,
	}); err != nil {
		return nil, fmt.Errorf("upsert project: %w", err)
	}

	// Best-effort Go module path (from go.mod). Used by the Go extractor to
	// rewrite intra-module imports to within-module paths so IMPORTS edges
	// can resolve across files. Missing or malformed go.mod just disables
	// the rewrite — external imports stay unresolved as before.
	modulePath := readGoModulePath(absPath)

	// Walk source files using gocodewalker (respects .gitignore)
	fileListQueue := make(chan *gocodewalker.File, 256)
	walker := gocodewalker.NewFileWalker(absPath, fileListQueue)
	walker.ExcludeDirectory = skippedDirSlice()

	// Start walker in background; gocodewalker closes the channel when done.
	go func() {
		if err := walker.Start(); err != nil {
			slog.Debug("pincher.walk.start.err", "err", err)
		}
	}()

	var (
		totalFiles    int
		totalSymbols  int
		totalEdges    int
		totalSkipped  int
		totalBlocked  int
		wg            sync.WaitGroup
		symBuf        []db.Symbol
		edgeBuf       []db.Edge
		pendingImport []ast.ExtractedEdge // deferred IMPORTS: resolved globally after full pass
		pendingCalls  []ast.ExtractedEdge // deferred Go CALLS: resolved globally after full pass
		bufMu         sync.Mutex
	)

	// Process files
	for fileJob := range fileListQueue {
		// Respect context cancellation (e.g. graceful shutdown).
		// Drain the remaining channel items so the walker goroutine can exit.
		if ctx.Err() != nil {
			go func() { for range fileListQueue {} }()
			wg.Wait()
			return nil, ctx.Err()
		}

		path := fileJob.Location
		if skip, reason := ast.ShouldSkip(path); skip {
			totalBlocked++
			slog.Debug("pincher.index.blocked", "path", path, "reason", reason)
			continue
		}
		if !ast.IsSourceFile(path) {
			continue
		}

		// Read file
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			slog.Debug("pincher.index.skip", "path", path, "err", readErr)
			continue
		}

		// Content-hash check (xxh3 — fastest non-crypto hash)
		hash := fmt.Sprintf("%x", xxh3.Hash(content))
		relPath, _ := filepath.Rel(absPath, path)
		relPath = filepath.ToSlash(relPath)

		if !force {
			stored := idx.store.GetFileHash(projectID, relPath)
			if stored == hash {
				totalSkipped++
				continue
			}
		}

		totalFiles++
		prog.FilesTotal.Add(1)
		wg.Add(1)
		go func(path, relPath, hash string, content []byte) {
			defer func() { prog.FilesDone.Add(1); wg.Done() }()

			lang := ast.DetectLanguage(path)
			if lang == "" {
				return
			}

			// Hash-skip above guarantees we only reach here when this file's
			// content actually changed (or force=true). Clear the file's
			// existing symbols and edges before re-emitting, so symbols
			// removed in the new version don't linger as orphans (they'd
			// otherwise return stale snippets for queries that name them, or
			// haunt trace/changes blast-radius). DeleteSymbolsForFile cascades
			// to edges with either endpoint in this file. Errors are logged
			// but non-fatal — emitting fresh symbols on top of a stale set is
			// less wrong than emitting nothing at all.
			if delErr := idx.store.DeleteSymbolsForFile(projectID, relPath); delErr != nil {
				slog.Warn("pincher.index.delete_stale.err", "err", delErr, "file", relPath)
			}

			// Three-layer extraction in one pass
			result := ast.ExtractWithModule(content, lang, relPath, modulePath)
			if result == nil || len(result.Symbols) == 0 {
				return
			}

			// Convert to DB types
			syms := make([]db.Symbol, 0, len(result.Symbols))
			for _, s := range result.Symbols {
				sym := db.Symbol{
					ID:            db.MakeSymbolID(relPath, s.QualifiedName, s.Kind),
					ProjectID:     projectID,
					FilePath:      relPath,
					Name:          s.Name,
					QualifiedName: s.QualifiedName,
					Kind:          s.Kind,
					Language:      lang,
					StartByte:     s.StartByte,
					EndByte:       s.EndByte,
					StartLine:     s.StartLine,
					EndLine:       s.EndLine,
					Signature:     s.Signature,
					ReturnType:    s.ReturnType,
					Docstring:     s.Docstring,
					Parent:        s.Parent,
					Complexity:    s.Complexity,
					IsExported:    s.IsExported,
					IsTest:               s.IsTest,
					IsEntryPoint:         s.IsEntryPoint,
					FileHash:             hash,
					ExtractionConfidence: s.ExtractionConfidence,
				}
				syms = append(syms, sym)
			}

			// Build a quick name→id map for edge resolution
			nameToID := make(map[string]string, len(syms))
			for _, sym := range syms {
				nameToID[sym.Name] = sym.ID
				nameToID[sym.QualifiedName] = sym.ID
			}

			// Convert extracted edges to DB edges. IMPORTS edges cross file
			// boundaries so local nameToID can't resolve them — defer to the
			// global post-pass below. Go CALLS that fail per-file resolution
			// are also deferred so cross-file calls (e.g. db.Open from a
			// test file) get resolved against the full project symbol table.
			// Non-Go regex extractors emit noisy CALLS targets — leave the
			// per-file drop in place to avoid creating false-positive edges.
			edges := make([]db.Edge, 0, len(result.Edges))
			deferredImports := make([]ast.ExtractedEdge, 0)
			deferredCalls := make([]ast.ExtractedEdge, 0)
			for _, e := range result.Edges {
				if e.Kind == "IMPORTS" {
					deferredImports = append(deferredImports, e)
					continue
				}
				fromID := nameToID[e.FromQN]
				if fromID == "" {
					// Try fuzzy: last component of FromQN
					parts := strings.Split(e.FromQN, ".")
					if len(parts) > 0 {
						fromID = nameToID[parts[len(parts)-1]]
					}
				}
				toID := nameToID[e.ToName]
				if fromID == "" || toID == "" {
					if e.Kind == "CALLS" && lang == "Go" {
						deferredCalls = append(deferredCalls, e)
					}
					continue
				}
				edges = append(edges, db.Edge{
					ProjectID:  projectID,
					FromID:     fromID,
					ToID:       toID,
					Kind:       e.Kind,
					Confidence: e.Confidence,
				})
			}

			bufMu.Lock()
			symBuf = append(symBuf, syms...)
			edgeBuf = append(edgeBuf, edges...)
			pendingImport = append(pendingImport, deferredImports...)
			pendingCalls = append(pendingCalls, deferredCalls...)
			// Flush when buffer is large enough
			if len(symBuf) >= 500 {
				totalSymbols += len(symBuf)
				totalEdges += len(edgeBuf)
				if flushErr := idx.flushBuffers(projectID, &symBuf, &edgeBuf); flushErr != nil {
					slog.Warn("pincher.index.flush.err", "err", flushErr)
				}
			}
			bufMu.Unlock()
		}(path, relPath, hash, content)
	}

	wg.Wait()

	// Final flush
	bufMu.Lock()
	if len(symBuf) > 0 || len(edgeBuf) > 0 {
		totalSymbols += len(symBuf)
		totalEdges += len(edgeBuf)
		if flushErr := idx.flushBuffers(projectID, &symBuf, &edgeBuf); flushErr != nil {
			slog.Warn("pincher.index.final_flush.err", "err", flushErr)
		}
	}
	bufMu.Unlock()

	// Resolve deferred IMPORTS edges against the full symbol table. Both
	// endpoints (FromQN and ToName) are Module qualified names; we look up
	// the first matching Module symbol per QN. External imports (qualified
	// name not indexed as a Module) simply don't resolve and are dropped,
	// matching today's behaviour for unresolved edges.
	if n := idx.resolveImports(projectID, pendingImport); n > 0 {
		totalEdges += n
	}

	// Resolve deferred Go CALLS edges against the full symbol table. Without
	// this pass, cross-file Go calls (e.g. test files calling db.Open) would
	// be silently dropped because the per-file nameToID only sees one file's
	// symbols. See LATENT_ISSUES #4 in pincher-followups for the original
	// observation.
	if n := idx.resolveCalls(projectID, pendingCalls); n > 0 {
		totalEdges += n
	}

	duration := time.Since(start)

	// Update project stats — use DB totals so incremental runs reflect the full graph.
	dbSyms, dbEdges, _, _, _ := idx.store.GraphStats(projectID)
	// File count: total files walked this run + skipped (unchanged) files.
	dbFiles := totalFiles + totalSkipped
	if err := idx.store.UpsertProject(db.Project{
		ID:        projectID,
		Path:      absPath,
		Name:      projectName,
		IndexedAt: start,
		FileCount: dbFiles,
		SymCount:  dbSyms,
		EdgeCount: dbEdges,
	}); err != nil {
		slog.Warn("pincher.index.update_project.err", "err", err)
	}

	slog.Info("pincher.indexed",
		"project", projectName,
		"files", totalFiles,
		"symbols", totalSymbols,
		"edges", totalEdges,
		"skipped", totalSkipped,
		"blocked", totalBlocked,
		"ms", duration.Milliseconds(),
	)

	// Refresh query-planner stats if stale. PRAGMA optimize is a no-op when
	// nothing's changed, so it's safe to call even on incremental indexes
	// where most files were skipped via content-hash.
	_ = idx.store.Optimize()

	// Force the WAL back toward zero before queries resume. Index() is the
	// natural quiet point — no readers should be waiting on the pre-index
	// snapshot — so a TRUNCATE checkpoint here keeps the WAL bounded even
	// under multi-writer storms (multiple Claude Code sessions, MCP server
	// + manual CLI, etc.).
	_ = idx.store.CheckpointTruncate()

	return &IndexResult{
		ProjectID:  projectID,
		Project:    projectName,
		Path:       absPath,
		Files:      totalFiles,
		Symbols:    totalSymbols,
		Edges:      totalEdges,
		Skipped:    totalSkipped,
		Blocked:    totalBlocked,
		DurationMS: duration.Milliseconds(),
	}, nil
}

// flushBuffers writes accumulated symbols and edges to the DB then resets the slices.
func (idx *Indexer) flushBuffers(projectID string, syms *[]db.Symbol, edges *[]db.Edge) error {
	if err := idx.flushBatch(projectID, *syms, *edges); err != nil {
		return err
	}
	// Update file hashes for all flushed symbols
	seen := make(map[string]bool)
	for _, s := range *syms {
		if !seen[s.FilePath] {
			_ = idx.store.SetFileHash(projectID, s.FilePath, s.FileHash)
			seen[s.FilePath] = true
		}
	}
	*syms = (*syms)[:0]
	*edges = (*edges)[:0]
	return nil
}

func (idx *Indexer) flushBatch(projectID string, syms []db.Symbol, edges []db.Edge) error {
	// Detect file moves before upserting (non-fatal: a failure just means
	// stale IDs won't redirect, but indexing still succeeds).
	if len(syms) > 0 {
		if err := idx.store.DetectAndRecordMoves(projectID, syms); err != nil {
			slog.Debug("pincher.move_detection.error", "project", projectID, "err", err)
		}
	}
	if err := idx.store.BulkUpsertSymbols(syms); err != nil {
		return fmt.Errorf("upsert symbols: %w", err)
	}
	if err := idx.store.BulkUpsertEdges(edges); err != nil {
		return fmt.Errorf("upsert edges: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Byte-offset source retrieval
// ─────────────────────────────────────────────────────────────────────────────

// ReadSymbolSource retrieves the source code for a symbol using O(1) byte-offset seeking.
// This is the core pincherMCP innovation: no re-parsing, no line scanning.
//
//	1 SQL lookup  → get start_byte, end_byte, file_path
//	1 os.Open     → open the file
//	1 Seek        → seek to start_byte
//	1 Read        → read (end_byte - start_byte) bytes
func ReadSymbolSource(projectRoot string, sym db.Symbol) (string, error) {
	return ReadSymbolSourceCapped(projectRoot, sym, 0)
}

// ReadSymbolSourceCapped reads at most maxBytes from the symbol's byte range.
// maxBytes <= 0 means unbounded (equivalent to ReadSymbolSource). When the
// caller only needs the first few lines (e.g. handleSearch's 5-line snippet),
// a small cap avoids reading tens of KB for symbols whose ranges cover whole
// YAML mappings or Markdown sections.
//
// The caller is responsible for handling truncated tails — e.g. by treating
// the last line as possibly partial when slicing by newlines.
func ReadSymbolSourceCapped(projectRoot string, sym db.Symbol, maxBytes int) (string, error) {
	if sym.StartByte == sym.EndByte {
		return "", nil
	}
	absPath := filepath.Join(projectRoot, filepath.FromSlash(sym.FilePath))
	f, err := os.Open(absPath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", sym.FilePath, err)
	}
	defer f.Close()

	size := sym.EndByte - sym.StartByte
	if size <= 0 {
		return "", nil
	}
	if maxBytes > 0 && size > maxBytes {
		size = maxBytes
	}
	if _, err := f.Seek(int64(sym.StartByte), 0); err != nil {
		return "", fmt.Errorf("seek: %w", err)
	}
	buf := make([]byte, size)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", fmt.Errorf("read: %w", err)
	}
	return string(buf[:n]), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// File watcher
// ─────────────────────────────────────────────────────────────────────────────

// Watch starts a background goroutine that re-indexes projects when files change.
// Uses polling with a 5-second ticker. If any Index() is currently running
// (from any source — Watch itself, the CLI, or another caller), the tick is
// skipped: the per-project mutex would just queue the work and waste CPU
// during catch-up.
func (idx *Indexer) Watch(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if idx.anyActive() {
				// Skip; let in-flight indexing finish. Avoids the
				// continuous-tick / 53%-CPU catch-up storm we hit
				// during long initial indexes.
				continue
			}
			projects, err := idx.store.ListProjects()
			if err != nil {
				continue
			}
			for _, p := range projects {
				if idx.hasChanges(p) {
					slog.Debug("pincher.watcher.reindex", "project", p.Name)
					go func(p db.Project) {
						if _, err := idx.Index(ctx, p.Path, false); err != nil {
							slog.Warn("pincher.watcher.reindex.err", "project", p.Name, "err", err)
						}
					}(p)
				}
			}
		}
	}
}

// anyActive reports whether any Index() call is currently in flight on
// this Indexer. Used by Watch to back off during catch-up.
func (idx *Indexer) anyActive() bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return len(idx.active) > 0
}

// hasChanges checks if any source file in the project has changed since last index.
// Uses a fast mtime check before doing the full xxh3 hash comparison.
func (idx *Indexer) hasChanges(p db.Project) bool {
	// Quick stat check on a sample of files
	entries, err := os.ReadDir(p.Path)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if skip, _ := ast.ShouldSkip(e.Name()); skip {
			continue
		}
		if !ast.IsSourceFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// If any file is newer than the last index time, trigger re-index
		if info.ModTime().After(p.IndexedAt) {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// Call graph (BFS for trace tool)
// ─────────────────────────────────────────────────────────────────────────────

// Hop is one step in a BFS call trace.
type Hop struct {
	Symbol db.Symbol
	Depth  int
	Via    string // edge kind that brought us here
	Risk   string // CRITICAL|HIGH|MEDIUM|LOW
}

// Trace performs BFS from a named symbol, returning the call chain.
// direction: "outbound" (what it calls), "inbound" (what calls it), "both"
// maxDepth: 1-5
//
// Implementation: delegates to db.TraceViaCTE which issues at most 2 SQL
// recursive CTE queries regardless of graph size, replacing the old approach
// that issued O(nodes × depth × 2) individual SQL round trips.
func (idx *Indexer) Trace(ctx context.Context, projectID, name string, direction string, maxDepth int, addRisk bool) ([]Hop, error) {
	if maxDepth <= 0 || maxDepth > 5 {
		maxDepth = 3
	}

	// Find start symbol
	starts, err := idx.store.GetSymbolsByName(projectID, name, 5)
	if err != nil {
		return nil, err
	}
	if len(starts) == 0 {
		return nil, fmt.Errorf("symbol %q not found in project", name)
	}
	start := starts[0]

	edgeKinds := []string{"CALLS", "HTTP_CALLS", "ASYNC_CALLS"}

	// Single CTE traversal per direction (max 2 SQL calls total for "both").
	traceResults, err := idx.store.TraceViaCTE(start.ID, direction, edgeKinds, maxDepth)
	if err != nil {
		return nil, err
	}

	var hops []Hop
	for _, tr := range traceResults {
		sym, getErr := idx.store.GetSymbol(tr.SymbolID)
		if getErr != nil || sym == nil {
			continue
		}
		risk := ""
		if addRisk {
			risk = RiskLabel(tr.Depth)
		}
		hops = append(hops, Hop{Symbol: *sym, Depth: tr.Depth, Via: tr.ViaKind, Risk: risk})
		if len(hops) >= 500 {
			break
		}
	}
	return hops, nil
}

// RiskLabel maps a BFS depth to a risk label: 1=CRITICAL, 2=HIGH, 3=MEDIUM, 4+=LOW.
func RiskLabel(depth int) string {
	switch depth {
	case 1:
		return "CRITICAL"
	case 2:
		return "HIGH"
	case 3:
		return "MEDIUM"
	default:
		return "LOW"
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

var skippedDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".cache":       true,
	"dist":         true,
	"build":        true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	"env":          true,
	"target":       true, // Rust
	".next":        true,
	".nuxt":        true,
	"coverage":     true,
}

func isSkippedDir(name string) bool {
	return skippedDirs[name] || strings.HasPrefix(name, ".")
}

// skippedDirSlice returns the skippedDirs map keys as a slice for gocodewalker.ExcludeDirectory.
func skippedDirSlice() []string {
	out := make([]string, 0, len(skippedDirs))
	for k := range skippedDirs {
		out = append(out, k)
	}
	return out
}

// readGoModulePath returns the module declared in repoPath/go.mod, or "" if
// no go.mod is present or the module line can't be parsed. Non-Go projects
// and pre-module Go repos both produce "" — callers should treat that as
// "don't rewrite import paths".
func readGoModulePath(repoPath string) string {
	data, err := os.ReadFile(filepath.Join(repoPath, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "module ") {
			continue
		}
		mod := strings.TrimSpace(strings.TrimPrefix(line, "module"))
		mod = strings.Trim(mod, "\"")
		return mod
	}
	return ""
}

// resolveImports converts deferred IMPORTS edges into concrete db.Edge rows
// by matching both endpoints against Module symbols in the project. Edges
// with no matching Module on either side are dropped. Returns the number
// of edges actually persisted.
func (idx *Indexer) resolveImports(projectID string, pending []ast.ExtractedEdge) int {
	if len(pending) == 0 {
		return 0
	}

	// Cache QN → first matching symbol ID so we don't repeat SELECTs for
	// packages imported from many files.
	cache := make(map[string]string)
	lookup := func(qn string) string {
		if id, ok := cache[qn]; ok {
			return id
		}
		syms, err := idx.store.GetSymbolsByQN(projectID, qn)
		if err != nil || len(syms) == 0 {
			cache[qn] = ""
			return ""
		}
		cache[qn] = syms[0].ID
		return syms[0].ID
	}

	// Dedupe by (fromID, toID) — one pair of files can appear many times
	// when there are multiple Module rows per package.
	seen := make(map[string]bool)
	edges := make([]db.Edge, 0, len(pending))
	for _, e := range pending {
		fromID := lookup(e.FromQN)
		toID := lookup(e.ToName)
		if fromID == "" || toID == "" {
			continue
		}
		key := fromID + "\x00" + toID
		if seen[key] {
			continue
		}
		seen[key] = true
		edges = append(edges, db.Edge{
			ProjectID:  projectID,
			FromID:     fromID,
			ToID:       toID,
			Kind:       "IMPORTS",
			Confidence: e.Confidence,
		})
	}

	if len(edges) == 0 {
		return 0
	}
	if err := idx.store.BulkUpsertEdges(edges); err != nil {
		slog.Warn("pincher.imports.upsert.err", "err", err)
		return 0
	}
	return len(edges)
}

// resolveCalls converts deferred Go CALLS edges into concrete db.Edge rows
// by matching FromQN and ToName against the project's symbol table. ToName
// values like "db.Open" resolve via exact qualified-name match; bare
// identifiers like "Foo" (same-package calls where the extractor emits the
// short name) fall back to a name-only lookup. Self-edges and duplicates
// are dropped. Returns the number of edges actually persisted.
//
// Only Go CALLS reach this path; non-Go regex extractors emit noisy ToName
// values that would create false-positive cross-package edges.
func (idx *Indexer) resolveCalls(projectID string, pending []ast.ExtractedEdge) int {
	if len(pending) == 0 {
		return 0
	}

	qnCache := make(map[string]string)
	lookupQN := func(qn string) string {
		if qn == "" {
			return ""
		}
		if id, ok := qnCache[qn]; ok {
			return id
		}
		syms, err := idx.store.GetSymbolsByQN(projectID, qn)
		if err != nil || len(syms) == 0 {
			qnCache[qn] = ""
			return ""
		}
		qnCache[qn] = syms[0].ID
		return syms[0].ID
	}

	nameCache := make(map[string]string)
	lookupName := func(name string) string {
		if name == "" {
			return ""
		}
		if id, ok := nameCache[name]; ok {
			return id
		}
		syms, err := idx.store.GetSymbolsByName(projectID, name, 1)
		if err != nil || len(syms) == 0 {
			nameCache[name] = ""
			return ""
		}
		nameCache[name] = syms[0].ID
		return syms[0].ID
	}

	seen := make(map[string]bool)
	edges := make([]db.Edge, 0, len(pending))
	for _, e := range pending {
		fromID := lookupQN(e.FromQN)
		if fromID == "" && !strings.Contains(e.FromQN, ".") {
			fromID = lookupName(e.FromQN)
		}
		if fromID == "" {
			continue
		}
		toID := lookupQN(e.ToName)
		if toID == "" && !strings.Contains(e.ToName, ".") {
			toID = lookupName(e.ToName)
		}
		if toID == "" || fromID == toID {
			continue
		}
		key := fromID + "\x00" + toID
		if seen[key] {
			continue
		}
		seen[key] = true
		edges = append(edges, db.Edge{
			ProjectID:  projectID,
			FromID:     fromID,
			ToID:       toID,
			Kind:       "CALLS",
			Confidence: e.Confidence,
		})
	}

	if len(edges) == 0 {
		return 0
	}
	if err := idx.store.BulkUpsertEdges(edges); err != nil {
		slog.Warn("pincher.calls.upsert.err", "err", err)
		return 0
	}
	return len(edges)
}
