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
	"github.com/kwad77/pincher/internal/ast"
	"github.com/kwad77/pincher/internal/db"
	"github.com/zeebo/xxh3"
)

// IndexProgress tracks live file-processing progress for a running index job.
type IndexProgress struct {
	FilesDone  atomic.Int64
	FilesTotal atomic.Int64
}

// DefaultMaxFileSize bounds per-file memory during indexing. Real source
// files are well under 200 KB; 4 MB leaves headroom for hand-written test
// corpora and large generated TypeScript declarations while preventing a
// single pathological input (e.g. a 100 MB JSON dump) from stalling the
// indexer or exhausting memory (#111).
const DefaultMaxFileSize int64 = 4 * 1024 * 1024

// Indexer manages repository indexing for pincherMCP.
type Indexer struct {
	store    *db.Store
	mu       sync.Mutex
	active   map[string]bool         // projectID → indexing in progress
	progress sync.Map                // projectID → *IndexProgress

	// maxFileSize is the per-file byte ceiling. Files larger than this are
	// recorded as "file_too_large" extraction failures and skipped without
	// being read into memory. Zero or negative means "no cap".
	maxFileSize int64

	// binaryVersion is stamped on each project at index time so health
	// can detect when an old indexer's CALLS data needs to be refreshed
	// against newer resolution rules (#304). Empty until SetBinaryVersion
	// is called — pre-existing rows then keep their stored version
	// rather than getting overwritten with "".
	binaryVersion string
}

// New creates a new Indexer with the default per-file size cap.
func New(store *db.Store) *Indexer {
	return &Indexer{
		store:       store,
		active:      make(map[string]bool),
		maxFileSize: DefaultMaxFileSize,
	}
}

// SetBinaryVersion records the running binary's version so it can be
// stamped on projects at index time (#304). Callers (cmd/pinch/main.go,
// the MCP server) plumb their build-time `version` here. Safe to call
// before or after `New`; idempotent.
func (idx *Indexer) SetBinaryVersion(v string) {
	idx.binaryVersion = v
}

// SetMaxFileSize overrides the per-file size cap. Pass 0 (or negative) to
// disable the cap entirely. Settings are honoured on the next Index() call.
func (idx *Indexer) SetMaxFileSize(bytes int64) {
	idx.mu.Lock()
	idx.maxFileSize = bytes
	idx.mu.Unlock()
}

// MaxFileSize returns the current per-file size cap in bytes (0 = no cap).
func (idx *Indexer) MaxFileSize() int64 {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.maxFileSize
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
	Deleted    int // files removed from disk since last index — symbols GC'd this run (#326)
	DurationMS int64
}

// Index indexes a repository at the given path (incremental by default).
// If force=true, all files are re-parsed regardless of content hash.
func (idx *Indexer) Index(ctx context.Context, repoPath string, force bool) (*IndexResult, error) {
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}

	// #310: fail fast when the path doesn't exist, rather than walking
	// a missing root and silently returning {files: 0, symbols: 0}
	// with the misleading "no indexable source files" diagnosis.
	// Catches typo'd paths immediately so the agent can correct.
	fi, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("path %q does not exist on disk", absPath)
		}
		return nil, fmt.Errorf("stat path %q: %w", absPath, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("path %q is not a directory", absPath)
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

	// Ensure project record exists. #304: stamp the running binary
	// version so health can detect drift later — empty when the
	// caller didn't plumb SetBinaryVersion.
	if err := idx.store.UpsertProject(db.Project{
		ID:            projectID,
		Path:          absPath,
		Name:          projectName,
		IndexedAt:     start,
		BinaryVersion: idx.binaryVersion,
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
		totalFiles     int
		totalSymbols   int
		totalEdges     int
		totalSkipped   int
		totalBlocked   int
		wg             sync.WaitGroup
		symBuf         []db.Symbol
		edgeBuf        []db.Edge
		pendingImport  []ast.ExtractedEdge // deferred IMPORTS: resolved globally after full pass
		pendingCalls   []ast.ExtractedEdge // deferred Go CALLS: resolved globally after full pass
		pendingReads   []ast.ExtractedEdge // deferred Go READS (#247 #3): resolved globally; only Variable targets persist
		bufMu          sync.Mutex
		lastStatsFlush time.Time           // throttle for in-flight project counts; guarded by bufMu
		seenFiles      = map[string]bool{} // #326: relPaths the walker yielded this run; tail-pass GC's symbols for files NOT in this set. Populated in the main (single-threaded) loop so no mutex.
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
		if !ast.IsSourceFile(path) && !ast.MayHaveShebang(path) {
			continue
		}

		// Per-file size cap (#111): stat before read so a 100 MB JSON dump
		// doesn't allocate 100 MB of process memory just to be rejected.
		// Failure is persisted so `pincher doctor` surfaces it; counted as
		// blocked so the IndexResult still reflects "files refused upstream
		// of extraction".
		if cap := idx.maxFileSize; cap > 0 {
			info, statErr := os.Stat(path)
			if statErr != nil {
				slog.Debug("pincher.index.stat.skip", "path", path, "err", statErr)
				continue
			}
			if info.Size() > cap {
				totalBlocked++
				relPath, _ := filepath.Rel(absPath, path)
				relPath = filepath.ToSlash(relPath)
				details := fmt.Sprintf("size=%d bytes (cap=%d bytes)", info.Size(), cap)
				if err := idx.store.RecordExtractionFailure(projectID, relPath, ast.DetectLanguage(path), "file_too_large", details); err != nil {
					slog.Warn("pincher.index.record_failure.err", "err", err, "file", relPath, "reason", "file_too_large")
				}
				slog.Debug("pincher.index.too_large", "path", path, "size", info.Size(), "cap", cap)
				continue
			}
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
		// #326: Mark this file as walker-yielded BEFORE the hash skip so the
		// tail-pass GC doesn't delete unchanged files. Populated single-threaded
		// here (not in the per-file goroutine) so no mutex needed.
		seenFiles[relPath] = true

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

			lang := ast.DetectLanguageFromContent(path, content)
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

			// Three-layer extraction in one pass.
			//
			// Wrapped in recover() (#42 part 1): if an extractor panics on
			// pathological input, we catch it, persist the failure, and skip
			// the file rather than crashing the whole indexer goroutine.
			// Without this, one malformed file kills the entire index run.
			result := safeExtractWithModule(idx, projectID, lang, relPath, content, modulePath)
			if result == nil || len(result.Symbols) == 0 {
				return
			}

			// Sanity heuristics — flag suspicious extractor output before it
			// reaches the DB. These are cheap (O(n) per file, n = symbols
			// per file) and catch real bug classes:
			//   - byte_range_negative: end_byte <= start_byte. Always wrong.
			//   - qualified_name_collision: same QN twice in one file. Means
			//     the dotted-path builder produced a duplicate (PR #39's
			//     YAML-byte-range bug used to do this when sequences were
			//     re-indexed).
			recordExtractionHeuristics(idx, projectID, lang, relPath, result)

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
			//
			// Go READS edges (#247 #3) ALWAYS defer — the local nameToID
			// would only catch in-file Variable references; cross-file
			// references need the project-wide post-pass. The post-pass
			// also drops references that don't resolve to a Variable
			// symbol, which is what filters out function names, types,
			// local vars, and parameters from over-emission.
			edges := make([]db.Edge, 0, len(result.Edges))
			deferredImports := make([]ast.ExtractedEdge, 0)
			deferredCalls := make([]ast.ExtractedEdge, 0)
			deferredReads := make([]ast.ExtractedEdge, 0)
			for _, e := range result.Edges {
				if e.Kind == "IMPORTS" {
					deferredImports = append(deferredImports, e)
					continue
				}
				if (e.Kind == "READS" || e.Kind == "WRITES") && lang == "Go" {
					deferredReads = append(deferredReads, e)
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

			refreshCounts := false
			bufMu.Lock()
			symBuf = append(symBuf, syms...)
			edgeBuf = append(edgeBuf, edges...)
			pendingImport = append(pendingImport, deferredImports...)
			pendingCalls = append(pendingCalls, deferredCalls...)
			pendingReads = append(pendingReads, deferredReads...)
			// Flush when buffer is large enough
			if len(symBuf) >= 500 {
				totalSymbols += len(symBuf)
				totalEdges += len(edgeBuf)
				if flushErr := idx.flushBuffers(projectID, &symBuf, &edgeBuf); flushErr != nil {
					slog.Warn("pincher.index.flush.err", "err", flushErr)
				}
				// Refresh the cached projects.* counts at most once every 5s
				// per project so `pincher list` reflects in-flight progress
				// during long index runs instead of reporting zeros.
				if time.Since(lastStatsFlush) > 5*time.Second {
					lastStatsFlush = time.Now()
					refreshCounts = true
				}
			}
			bufMu.Unlock()

			if refreshCounts {
				if dbSyms, dbEdges, _, _, _ := idx.store.GraphStats(projectID); dbSyms > 0 {
					_ = idx.store.UpdateProjectCounts(projectID, int(prog.FilesDone.Load()), dbSyms, dbEdges)
				}
			}
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

	// Resolve deferred Go READS edges against the full symbol table.
	// Only persists edges where the resolved target is a Variable
	// symbol — this is the natural filter for the over-emission from
	// extractGoReads (function names, types, local vars all get dropped
	// here without needing scope analysis at extraction time). #247 #3.
	if n := idx.resolveReads(projectID, pendingReads); n > 0 {
		totalEdges += n
	}

	// #326: Tail-pass GC for files removed from disk. The walker yields only
	// files that still exist; per-file goroutines call DeleteSymbolsForFile
	// only on files they visit. Without this pass, deleting a file from disk
	// leaves its symbols + file_hash row behind forever (paperclip in the
	// dogfood DB had 4820 orphan symbols and 0 files). Cheap: one SELECT
	// plus N small DELETEs at index tail; only fires when a file actually
	// disappeared.
	var totalDeleted int
	if storedFiles, listErr := idx.store.ListFilesForProject(projectID); listErr == nil {
		for _, stored := range storedFiles {
			if seenFiles[stored] {
				continue
			}
			if err := idx.store.DeleteSymbolsForFile(projectID, stored); err != nil {
				slog.Warn("pincher.index.gc.delete_symbols.err", "err", err, "file", stored)
				continue
			}
			if err := idx.store.DeleteFileHash(projectID, stored); err != nil {
				slog.Warn("pincher.index.gc.delete_hash.err", "err", err, "file", stored)
			}
			totalDeleted++
		}
	} else {
		slog.Warn("pincher.index.gc.list.err", "err", listErr)
	}

	duration := time.Since(start)

	// Update project stats — use DB totals so incremental runs reflect the full graph.
	dbSyms, dbEdges, _, _, _ := idx.store.GraphStats(projectID)
	// File count: total files walked this run + skipped (unchanged) files.
	dbFiles := totalFiles + totalSkipped
	if err := idx.store.UpsertProject(db.Project{
		ID:            projectID,
		Path:          absPath,
		Name:          projectName,
		IndexedAt:     start,
		FileCount:     dbFiles,
		SymCount:      dbSyms,
		EdgeCount:     dbEdges,
		BinaryVersion: idx.binaryVersion,
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
		Deleted:    totalDeleted,
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
// hasChanges reports whether any indexable file under p.Path has been
// modified since the last index run. Returns true on the first newer
// file (early exit), so the cost on a clean tree is one stat per file
// + one readdir per directory.
//
// #377: pre-fix this only inspected p.Path's top-level entries via
// os.ReadDir, so edits to anything in a subdirectory (e.g. internal/
// or src/) were silently ignored. The watcher would then never trigger
// a re-index, and `search` returned stale results until an explicit
// `index` call. The fix walks recursively, sharing the indexer's
// skipped-dirs set so vendor/.git/node_modules don't dominate the walk.
func (idx *Indexer) hasChanges(p db.Project) bool {
	var changed bool
	_ = filepath.WalkDir(p.Path, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Permission denied / path vanished mid-walk — skip and keep going.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// Don't recurse into the project root if it itself matches
			// (root has "" as base name and shouldn't be skipped).
			if path != p.Path && isSkippedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if skip, _ := ast.ShouldSkip(name); skip {
			return nil
		}
		if !ast.IsSourceFile(name) && !ast.MayHaveShebang(name) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		if info.ModTime().After(p.IndexedAt) {
			changed = true
			return filepath.SkipAll
		}
		return nil
	})
	return changed
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
	// Find start symbol by short name. When multiple symbols share the
	// name (common: many `Run`, `Handler`, `Open` in one project), this
	// picks the first match — same documented heuristic the search tool
	// uses when no qualifier is provided. Callers that have an exact ID
	// (e.g. `changes` already knows which symbol it walked over) should
	// use `TraceByID` instead so blast radius is computed for the exact
	// symbol, not whichever same-named one resolves first (#5).
	starts, err := idx.store.GetSymbolsByName(projectID, name, 5)
	if err != nil {
		return nil, err
	}
	if len(starts) == 0 {
		return nil, fmt.Errorf("symbol %q not found in project", name)
	}
	return idx.TraceByID(ctx, projectID, starts[0].ID, direction, maxDepth, addRisk)
}

// TraceByID is Trace with the start-symbol disambiguation step skipped:
// the caller already has an exact symbol ID (typically from a previous
// `search`, `symbol`, or `changes` call), so resolving by name would be
// wrong when multiple symbols share that name. Use this whenever you
// have an ID; the name-based `Trace` is for tools that take a name.
//
// projectID scopes the BFS traversal to a single project's edges (#7).
// Pass "" to opt into legacy cross-project traversal (rare; useful only
// when the caller is intentionally exploring an inter-project edge).
func (idx *Indexer) TraceByID(ctx context.Context, projectID, symbolID, direction string, maxDepth int, addRisk bool, edgeKinds ...string) ([]Hop, error) {
	if maxDepth <= 0 || maxDepth > 5 {
		maxDepth = 3
	}

	// Default edge kinds when caller doesn't specify (preserves the
	// original behaviour for the 4 existing call sites). New callers
	// pass their own list — e.g. ["READS", "WRITES"] to follow var
	// refs only, or ["CALLS", "READS"] to mix.
	if len(edgeKinds) == 0 {
		edgeKinds = []string{"CALLS", "HTTP_CALLS", "ASYNC_CALLS"}
	}

	// Single CTE traversal per direction (max 2 SQL calls total for "both").
	// Project-scoped (#7): the recursive edge join restricts to the
	// caller's project so a same-named or same-IDed symbol in a sibling
	// project can't appear in the result set.
	traceResults, err := idx.store.TraceViaCTEScoped(projectID, symbolID, direction, edgeKinds, maxDepth)
	if err != nil {
		return nil, err
	}

	var hops []Hop
	for _, tr := range traceResults {
		// GetSymbolScoped (#2) belt-and-braces: the BFS already
		// filtered edges, but a stale row whose target was overwritten
		// by a colliding symbol in another project would still surface
		// here. The scoped lookup verifies the symbol belongs to the
		// requested project. Falls back to unscoped when projectID="".
		var sym *db.Symbol
		var getErr error
		if projectID != "" {
			sym, getErr = idx.store.GetSymbolScoped(projectID, tr.SymbolID)
		} else {
			sym, getErr = idx.store.GetSymbol(tr.SymbolID)
		}
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

// safeExtractWithModule wraps ast.ExtractWithModule in a recover() that
// persists an "extractor_panicked" failure row instead of crashing the
// per-file goroutine. Returns nil on panic so the caller skips the file.
//
// Without this, a single malformed file (e.g. a YAML stream that triggers
// a yaml.v3 panic, a Cypher-shape that breaks the parser, etc.) kills the
// entire Index() run via the goroutine crash. Now the panic is captured,
// the file is skipped, and the user sees it in `extraction_failures`.
//
// #42 part 1 — diagnostic surface.
func safeExtractWithModule(idx *Indexer, projectID, lang, relPath string, content []byte, modulePath string) (result *ast.FileResult) {
	defer func() {
		if r := recover(); r != nil {
			details := fmt.Sprintf("panic: %v", r)
			if err := idx.store.RecordExtractionFailure(projectID, relPath, lang, "extractor_panicked", details); err != nil {
				slog.Warn("pincher.failure_record.err", "err", err, "file", relPath)
			}
			slog.Warn("pincher.extractor.panic", "file", relPath, "lang", lang, "panic", r)
			result = nil
		}
	}()
	return ast.ExtractWithModule(content, lang, relPath, modulePath)
}

// recordExtractionHeuristics scans extractor output for shapes that are
// always-wrong, and records each as an extraction_failures row. The bug
// classes it catches:
//
//   - byte_range_negative: end_byte <= start_byte for any symbol. Means
//     the extractor produced an inverted range; the symbol can't be sliced
//     from the source file. This is a hard correctness bug — every fetch
//     of that symbol returns either zero bytes or the wrong content.
//
//   - qualified_name_collision: the same qualified_name appears more than
//     once in a single file's extraction output. Pre-fix, PR #39's YAML
//     scalar byte-range bug occasionally produced two `services.web.image`
//     entries when the byte-range walker drifted into the next sibling.
//
// Heuristics are O(n) in symbols per file. They run after extraction
// succeeded, so a parse-error path doesn't double-record (parse_error +
// these heuristics).
//
// #42 part 1 — diagnostic surface.
func recordExtractionHeuristics(idx *Indexer, projectID, lang, relPath string, result *ast.FileResult) {
	for _, s := range result.Symbols {
		// byte_range_negative
		if s.EndByte <= s.StartByte {
			details := fmt.Sprintf("symbol %q (%s): end_byte=%d <= start_byte=%d",
				s.QualifiedName, s.Kind, s.EndByte, s.StartByte)
			if err := idx.store.RecordExtractionFailure(projectID, relPath, lang, "byte_range_negative", details); err != nil {
				slog.Warn("pincher.failure_record.err", "err", err, "file", relPath)
			}
		}
	}
	// qualified_name_collision — record once per file even if multiple QNs
	// collide; details lists the worst offender plus the count. The
	// pre-disambiguation count is carried on FileResult.QNCollisions
	// (#115): the regex extractor produced duplicates, then
	// disambiguateDuplicates suffixed them with `~<line>` so all symbols
	// survive — but the underlying scope-blindness still merits the
	// diagnostic so reviewers see when a new collision pattern appears.
	if len(result.QNCollisions) == 0 {
		return
	}
	var worst string
	worstCount := 0
	for qn, n := range result.QNCollisions {
		if n > worstCount {
			worst = qn
			worstCount = n
		}
	}
	details := fmt.Sprintf("qualified_name %q appears %d times (extractor produced duplicates)",
		worst, worstCount)
	if err := idx.store.RecordExtractionFailure(projectID, relPath, lang, "qualified_name_collision", details); err != nil {
		slog.Warn("pincher.failure_record.err", "err", err, "file", relPath)
	}
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

// isStdlibReceiver reports whether `name` looks like a Go stdlib
// package leaf (the bit after the last `/` in an import path).
// When `ToName="strings.Index"` fails QN lookup, the receiver
// `strings` matches → we skip the in-project method-name fallback
// (#410) and drop the edge rather than false-bind to any
// `*Indexer.Index` in the project.
//
// This is a stoplist, not a complete check. The proper fix is
// import-graph-aware resolution (per file, only fall back when
// the receiver matches an in-project package). Until that lands,
// this catches the dominant false-positive sources observed in
// pincher-repo's dogfood pass: every common stdlib package whose
// methods share names with in-project Methods.
//
// Names included cover the stdlib packages whose method/function
// names overlap most heavily with in-project method names:
// strings.Index, bytes.Buffer.String, time.Now, os.Open,
// io.Copy, fmt.Sprintf, http.Get, etc.
func isStdlibReceiver(name string) bool {
	switch name {
	case "strings", "bytes", "time", "os", "fmt", "io", "ioutil",
		"errors", "context", "sync", "atomic", "filepath", "path",
		"exec", "url", "http", "regexp", "sort", "strconv", "math",
		"rand", "log", "slog", "ast", "token", "reflect", "runtime",
		"unsafe", "unicode", "utf8", "utf16", "binary", "hex",
		"base64", "csv", "xml", "html", "template", "tls", "syscall",
		"encoding", "hash", "crypto", "sha256", "sha1", "md5", "rsa",
		"tar", "zip", "gzip", "flate", "bufio", "bits", "big",
		"cmplx", "list", "ring", "heap", "container", "image",
		"color", "draw", "jpeg", "png", "gif", "mime", "multipart",
		"smtp", "mail", "net", "tcp", "udp", "ip", "rpc", "jsonrpc",
		"signal", "user", "build", "cgo", "format", "parser",
		"printer", "scanner", "types", "constant", "doc", "importer",
		"plugin", "tabwriter", "text", "elliptic", "ecdsa", "ed25519",
		"x509", "pem", "subtle", "sql", "driver", "expvar", "trace",
		"pprof":
		return true
	}
	return false
}

// resolveMethodByName is the #285 receiver-method fallback. Looks up
// every project symbol with the given short name; returns the ID
// only when exactly one is of kind=Method. Multiple matches are
// ambiguous (different types each defining the same method name —
// `Close`, `Run`, `String`); without receiver-type info we can't
// pick correctly, so we drop the edge rather than guess. Zero
// matches naturally returns "".
func resolveMethodByName(store *db.Store, projectID, name string) string {
	syms, err := store.GetSymbolsByName(projectID, name, 16)
	if err != nil {
		return ""
	}
	var only string
	for _, s := range syms {
		if s.Kind != "Method" {
			continue
		}
		if only != "" {
			// Ambiguous — multiple Method symbols share this name.
			return ""
		}
		only = s.ID
	}
	return only
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

	// methodCache: receiver-method fallback lookups (#285). Empty
	// string means "tried, no unique match" so we don't re-query.
	methodCache := make(map[string]string)
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
		// #285: receiver-method calls (e.g. `idx.Index(...)`) produce
		// ToName="idx.Index" which never matches a real qualified name
		// (the Method's QN is `index.*Indexer.Index`). Without type
		// info we can't compute the canonical QN. As a pragmatic
		// fallback: when ToName looks like "receiver.Method" and QN
		// lookup failed, look up a Method symbol whose plain name
		// matches the trailing component. Filtered to Method kind
		// (so `time.Now()` doesn't accidentally bind to a project
		// Function named `Now`); resolved unambiguously when there's
		// exactly one matching Method in the project.
		if toID == "" {
			if i := strings.LastIndex(e.ToName, "."); i > 0 && i < len(e.ToName)-1 {
				receiver := e.ToName[:i]
				trailing := e.ToName[i+1:]
				// #410: skip the receiver-method fallback when the
				// receiver looks like a Go stdlib package. Otherwise
				// `strings.Index(...)` falsely binds to any
				// `*Indexer.Index` Method in the project (and same
				// pattern for `bytes.Buffer.String`, `time.Now`,
				// `os.Open`, etc.). Without import-graph awareness
				// (the proper fix), this stoplist catches the most
				// common false-positive sources surfaced during
				// pincher-repo's own dogfood pass.
				if !isStdlibReceiver(receiver) {
					if id, ok := methodCache[trailing]; ok {
						toID = id
					} else {
						id := resolveMethodByName(idx.store, projectID, trailing)
						methodCache[trailing] = id
						toID = id
					}
				}
			}
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

// resolveReads converts deferred Go READS and WRITES edges into
// concrete db.Edge rows when the resolved target is a Variable symbol.
// Anything else (Function, Method, Class, local-name shadow with no
// Variable in the project) is dropped — that's the natural filter for
// the over-emission from extractGoReads which deliberately walks every
// identifier without scope analysis. Self-edges and duplicates within
// a (kind) are dropped — but a function that both reads AND writes a
// var produces both edges (different kinds, different keys). Returns
// the number of edges actually persisted.
//
// #247 #3: READS surfaces every reader of a package var; WRITES
// surfaces every modifier. Trace can answer the narrower question
// "who modifies Cache" vs "who only observes it" by filtering on
// edge kind. Confidence is preserved from the extracted edge (0.5 —
// lower than CALLS at 0.7 because over-emission is expected).
func (idx *Indexer) resolveReads(projectID string, pending []ast.ExtractedEdge) int {
	if len(pending) == 0 {
		return 0
	}

	// QN cache: maps the qualified name to (id, language, isVariable).
	// language is needed for #436 — same-language scoping prevents Go
	// references to common identifiers (`path`, `result`, `fs`) from
	// binding to JS / Python locals that happen to share the name.
	type lookup struct {
		id    string
		lang  string
		isVar bool
	}
	qnCache := make(map[string]lookup)
	lookupQN := func(qn string) lookup {
		if qn == "" {
			return lookup{}
		}
		if v, ok := qnCache[qn]; ok {
			return v
		}
		syms, err := idx.store.GetSymbolsByQN(projectID, qn)
		if err != nil || len(syms) == 0 {
			qnCache[qn] = lookup{}
			return lookup{}
		}
		v := lookup{id: syms[0].ID, lang: syms[0].Language, isVar: syms[0].Kind == "Variable"}
		qnCache[qn] = v
		return v
	}

	// Name lookups are scoped to the source language. Cache key includes
	// the language so two source languages asking for the same name
	// don't collide on the cached entry from the first one in.
	type nameKey struct{ name, lang string }
	nameCache := make(map[nameKey]lookup)
	lookupNameInLang := func(name, lang string) lookup {
		if name == "" {
			return lookup{}
		}
		k := nameKey{name: name, lang: lang}
		if v, ok := nameCache[k]; ok {
			return v
		}
		syms, err := idx.store.GetSymbolsByName(projectID, name, 10)
		if err != nil || len(syms) == 0 {
			nameCache[k] = lookup{}
			return lookup{}
		}
		// #436: only match symbols of the same language as the source.
		// Variable matches preferred within that scoped set.
		var v lookup
		for _, s := range syms {
			if lang != "" && s.Language != lang {
				continue
			}
			if s.Kind == "Variable" {
				v = lookup{id: s.ID, lang: s.Language, isVar: true}
				break
			}
			if v.id == "" {
				v = lookup{id: s.ID, lang: s.Language, isVar: false}
			}
		}
		nameCache[k] = v
		return v
	}

	seen := make(map[string]bool)
	edges := make([]db.Edge, 0, len(pending))
	for _, e := range pending {
		from := lookupQN(e.FromQN)
		fromID := from.id
		if fromID == "" && !strings.Contains(e.FromQN, ".") {
			// From-side name lookup — no language scope yet (we're
			// trying to discover it). The from symbol's language
			// becomes the gate for the to-side lookup below.
			from = lookupNameInLang(e.FromQN, "")
			fromID = from.id
		}
		if fromID == "" {
			continue
		}
		to := lookupQN(e.ToName)
		if to.id == "" && !strings.Contains(e.ToName, ".") {
			to = lookupNameInLang(e.ToName, from.lang)
		}
		// #436: belt-and-suspenders — even when QN matched, drop the
		// edge if the target is a different language than the source.
		// QN matches across languages can happen for short namespace
		// segments (`util`, `index`) that look identical when lifted
		// to the qualified-name table.
		if to.id == "" || !to.isVar || fromID == to.id {
			continue
		}
		if from.lang != "" && to.lang != "" && from.lang != to.lang {
			continue
		}
		// Dedupe key includes the edge kind so a function that BOTH
		// reads and writes the same var produces two edges (READS +
		// WRITES) — they answer different refactor questions.
		key := fromID + "\x00" + to.id + "\x00" + e.Kind
		if seen[key] {
			continue
		}
		seen[key] = true
		// Preserve the extractor's edge kind verbatim so READS stays
		// READS and WRITES stays WRITES through resolution.
		kind := e.Kind
		if kind != "READS" && kind != "WRITES" {
			kind = "READS" // defensive default for any future extractor that forgets to set Kind
		}
		edges = append(edges, db.Edge{
			ProjectID:  projectID,
			FromID:     fromID,
			ToID:       to.id,
			Kind:       kind,
			Confidence: e.Confidence,
		})
	}

	if len(edges) == 0 {
		return 0
	}
	if err := idx.store.BulkUpsertEdges(edges); err != nil {
		slog.Warn("pincher.reads.upsert.err", "err", err)
		return 0
	}
	return len(edges)
}
