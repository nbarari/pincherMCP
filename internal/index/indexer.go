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
				// Stamp the source file before deferral so the resolver
				// can disambiguate FromQN against project-wide name
				// collisions (#487). Extractors leave FromFile empty;
				// indexer fills it here.
				e.FromFile = relPath
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

			// #457: persist this file's deferred edge candidates so a
			// future incremental re-index that hash-skips this file
			// still has its IMPORTS / CALLS / READS / WRITES in the
			// candidate pool. Without this, edges from a skipped file
			// to a changed file get dropped on resolve.
			fileDeferred := make([]db.PendingEdge, 0, len(deferredImports)+len(deferredCalls)+len(deferredReads))
			appendDeferred := func(src []ast.ExtractedEdge) {
				for _, e := range src {
					fileDeferred = append(fileDeferred, db.PendingEdge{
						ProjectID:    projectID,
						FromFile:     relPath,
						Kind:         e.Kind,
						FromQN:       e.FromQN,
						ToName:       e.ToName,
						Confidence:   e.Confidence,
						ReceiverType: e.ReceiverType,
					})
				}
			}
			appendDeferred(deferredImports)
			appendDeferred(deferredCalls)
			appendDeferred(deferredReads)
			if err := idx.store.ReplacePendingEdgesForFile(projectID, relPath, fileDeferred); err != nil {
				slog.Warn("pincher.pending_edges.replace.err", "file", relPath, "err", err)
			}

			// #423 piece 2: persist Go struct field maps so the resolver
			// can follow recv.field.method calls. Mirrors the
			// pending_edges per-file replace pattern. No-op for files
			// that contain no Class symbols with a non-empty Fields map.
			fileFields := make([]db.StructField, 0)
			for _, sym := range result.Symbols {
				if sym.Kind != "Class" || len(sym.Fields) == 0 {
					continue
				}
				structID := db.MakeSymbolID(relPath, sym.QualifiedName, sym.Kind)
				for fname, ftype := range sym.Fields {
					fileFields = append(fileFields, db.StructField{
						ProjectID: projectID,
						StructID:  structID,
						FieldName: fname,
						FieldType: ftype,
					})
				}
			}
			if err := idx.store.ReplaceStructFieldsForFile(projectID, relPath, fileFields); err != nil {
				slog.Warn("pincher.struct_fields.replace.err", "file", relPath, "err", err)
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

	// #457: resolve deferred edges against the FULL persisted candidate
	// pool, not just this run's in-memory pendingX slices. The DB rows
	// include hash-skipped files' candidates from prior runs — which
	// fixes #427's transitive edge-loss on incremental re-indexes.
	// LoadPendingEdges returns nil/[] on the first index (no prior
	// rows), at which point the resolve passes are effectively no-ops.
	allImports := loadOrFallback(idx, projectID, "IMPORTS", pendingImport)
	if n := idx.resolveImports(projectID, allImports); n > 0 {
		totalEdges += n
	}

	allCalls := loadOrFallback(idx, projectID, "CALLS", pendingCalls)
	if n := idx.resolveCalls(projectID, allCalls); n > 0 {
		totalEdges += n
	}

	allReads := loadOrFallback(idx, projectID, "READS", pendingReads)
	allReads = append(allReads, loadOrFallback(idx, projectID, "WRITES", nil)...)
	if n := idx.resolveReads(projectID, allReads); n > 0 {
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
			// #457: drop any pending_edges rows that pointed out from
			// the removed file so re-resolution doesn't try to bind
			// dangling FromQN→ToName candidates against the now-shrunk
			// symbol set.
			if err := idx.store.DeletePendingEdgesForFile(projectID, stored); err != nil {
				slog.Warn("pincher.index.gc.delete_pending_edges.err", "err", err, "file", stored)
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
				changed := idx.changedFiles(p)
				if len(changed) == 0 {
					continue
				}
				slog.Debug("pincher.watcher.reindex",
					"project", p.Name,
					"changed_files", len(changed))
				// #427 (partial): clear file_hash on direct referencers
				// of any changed file so they re-extract and their
				// deferred CALLS / IMPORTS / READS feed resolveCalls
				// again. Handles one hop — known limitation: a
				// referencer that itself has incoming edges from
				// elsewhere drops those edges when DeleteSymbolsForFile
				// cascades, and they aren't recovered unless the
				// upstream-of-referencer files also re-extract. The
				// transitive expansion needed for full correctness
				// converges on "every file with a cross-file edge" for
				// typical Go projects, so the right long-term fix is
				// persisted-deferred-edges (Option 3 from the issue
				// analysis) rather than recursive hash invalidation.
				idx.invalidateReferencers(p, changed)
				go func(p db.Project) {
					if _, err := idx.Index(ctx, p.Path, false); err != nil {
						slog.Warn("pincher.watcher.reindex.err", "project", p.Name, "err", err)
					}
				}(p)
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

// hasChanges is kept as a thin compatibility wrapper around
// changedFiles so existing tests that just need the bool signal don't
// have to change shape. Returns true the moment changedFiles would
// emit any entry.
func (idx *Indexer) hasChanges(p db.Project) bool {
	return len(idx.changedFiles(p)) > 0
}

// changedFiles returns the relative paths (project-root relative,
// forward-slashed) of indexable source files whose mtime is newer than
// the project's last index. Returns an empty slice when nothing changed.
//
// Replaces the older hasChanges() bool helper (#427): the watcher now
// needs the concrete list to find downstream referencers whose
// cross-file edges must be rebuilt — not just a generic "something
// changed" signal.
//
// #377: walks recursively so edits in subdirectories (internal/, src/)
// trigger re-index. Uses the indexer's skipped-dirs set to avoid
// vendor/.git/node_modules dominating the walk.
func (idx *Indexer) changedFiles(p db.Project) []string {
	var changed []string
	_ = filepath.WalkDir(p.Path, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
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
			if rel, relErr := filepath.Rel(p.Path, path); relErr == nil {
				changed = append(changed, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	return changed
}

// invalidateReferencers clears the file_hash rows for every file that
// has a cross-file edge pointing into any of `changed`. The next
// Index() walk sees the hash mismatch and re-extracts those files,
// which re-populates the deferred CALLS / IMPORTS / READS edges that
// resolveCalls / resolveImports / resolveReads need in order to rebuild
// the cross-file edges that DeleteSymbolsForFile(changed) cascade-
// deletes. Best-effort — failures are logged, not fatal: a missed
// invalidation just means the edge stays missing until the next force
// re-index (no worse than the pre-fix behaviour).
//
// The referencer-files are invalidated regardless of whether they
// themselves appear in `changed`: changedFiles is mtime-based and the
// `files` table stores IndexedAt at second precision, so mtime can
// falsely match for files that were written within the same second as
// the prior Index. Trusting `changed` membership here would let those
// files keep their old (unchanged) content hash and slip through
// Index()'s hash-skip even though the cross-file edges into the real
// changed file are gone.
func (idx *Indexer) invalidateReferencers(p db.Project, changed []string) {
	if len(changed) == 0 {
		return
	}
	cleared := map[string]bool{}
	for _, target := range changed {
		referencers, err := idx.store.FilesWithEdgesToFile(p.ID, target)
		if err != nil {
			slog.Debug("pincher.watcher.referencers.err", "project", p.Name, "target", target, "err", err)
			continue
		}
		for _, ref := range referencers {
			if cleared[ref] {
				continue
			}
			cleared[ref] = true
			if err := idx.store.DeleteFileHash(p.ID, ref); err != nil {
				slog.Debug("pincher.watcher.invalidate.err", "project", p.Name, "file", ref, "err", err)
			}
		}
	}
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

	// Cache QN → canonical matching symbol ID so we don't repeat SELECTs
	// for packages imported from many files. The "canonical" pick is the
	// lexicographically smallest symbol ID — Go packages emit one Module
	// row per file (`<file>::<pkg>#Module`), and `GetSymbolsByQN` returns
	// rows in implementation-defined order without an ORDER BY. Picking
	// `syms[0]` non-deterministically caused #428: the same logical
	// `(server → db)` IMPORTS edge resolved to different
	// (from_module_file, to_module_file) pairs across re-index runs,
	// each new pair landing as a new edge under the
	// UNIQUE(project_id, from_id, to_id, kind) constraint. Sorting picks
	// a stable representative per package so re-resolution is idempotent.
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
		canonical := syms[0].ID
		for i := 1; i < len(syms); i++ {
			if syms[i].ID < canonical {
				canonical = syms[i].ID
			}
		}
		cache[qn] = canonical
		return canonical
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
			Source:     "resolve_pass",
		})
	}

	// #475: atomic replace of the prior resolve pass's IMPORTS edges.
	if err := idx.store.DeleteEdgesByKindAndSource(projectID, "IMPORTS", "resolve_pass"); err != nil {
		slog.Warn("pincher.imports.delete_prior.err", "err", err)
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

// loadOrFallback returns the project's persisted pending_edges of the
// given kind (#457). When the load fails (e.g. on a corrupt DB or a
// brand-new project before the first row lands), it falls back to
// this run's in-memory candidates so resolve still produces *some*
// graph rather than silently emitting zero edges. Fallback is
// strictly looser than the persisted path — it can only undercount
// cross-file edges from prior runs, not introduce new false positives.
func loadOrFallback(idx *Indexer, projectID, kind string, fallback []ast.ExtractedEdge) []ast.ExtractedEdge {
	rows, err := idx.store.LoadPendingEdges(projectID, kind)
	if err != nil {
		slog.Warn("pincher.pending_edges.load.err", "kind", kind, "err", err)
		return fallback
	}
	out := make([]ast.ExtractedEdge, 0, len(rows))
	for _, r := range rows {
		out = append(out, ast.ExtractedEdge{
			FromQN:       r.FromQN,
			ToName:       r.ToName,
			Kind:         r.Kind,
			FromFile:     r.FromFile,
			Confidence:   r.Confidence,
			ReceiverType: r.ReceiverType,
		})
	}
	return out
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

// isPolymorphicInterfaceMethodName reports whether `name` looks like a
// method name that overwhelmingly resolves to a stdlib interface in
// real Go code. Companion to isStdlibReceiver (#410): that filter caught
// `strings.Index(...)`-style calls where the receiver itself is a known
// stdlib package. This filter catches the harder case of
// `localVar.PolymorphicMethod(...)` where the receiver is a variable
// whose type happens to be from stdlib — the receiver name doesn't
// reveal the stdlib origin but the method name does. #465: dogfood
// surfaced `*bytesCollector.String` as a top-3 architecture hotspot
// because every `.String()` call in the project (`time.Time.String`,
// `*url.URL.String`, `bytes.Buffer.String`, ...) was binding to the
// single project-local Method with that name.
//
// Under-counting cost: a project that genuinely calls its own
// `String()` Method *as a receiver-method call from another package*
// loses that edge. Direct-QN-match calls from within the same package
// still resolve through the QN path, not this fallback.
func isPolymorphicInterfaceMethodName(name string) bool {
	switch name {
	// fmt.Stringer / error
	case "String", "Error", "GoString":
		return true
	// io interfaces
	case "Read", "Write", "Close", "Seek", "ReadAt", "WriteAt",
		"ReadFrom", "WriteTo", "ReadByte", "WriteByte",
		"ReadString", "WriteString", "ReadRune", "WriteRune",
		"UnreadByte", "UnreadRune":
		return true
	// sync.Locker family
	case "Lock", "Unlock", "RLock", "RUnlock", "TryLock":
		return true
	// sort.Interface
	case "Len", "Less", "Swap":
		return true
	// http.Handler / net interfaces
	case "ServeHTTP":
		return true
	// encoding interfaces — common JSON/text/binary marshalers
	case "MarshalJSON", "UnmarshalJSON",
		"MarshalText", "UnmarshalText",
		"MarshalBinary", "UnmarshalBinary",
		"MarshalYAML", "UnmarshalYAML":
		return true
	// fmt.Formatter / fmt.Scanner
	case "Format", "Scan":
		return true
	// time.Time methods that pair with common local-var names
	// (deadline.Add, expiry.Sub, ts.Now, etc.)
	case "Now", "Add", "Sub", "Before", "After", "Equal":
		return true
	// context.Context
	case "Deadline", "Done", "Err", "Value":
		return true
	// errors.Is / errors.As / errors.Unwrap target methods
	case "Is", "As", "Unwrap":
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

	// pickCanonical returns the lexicographically smallest symbol ID,
	// stable across re-index runs. Same rationale as resolveImports
	// (#428): SQLite returns rows in implementation-defined order
	// without an ORDER BY, so picking syms[0] non-deterministically
	// can resolve the same logical (from, to) pair to different
	// symbol IDs across runs, defeating the
	// UNIQUE(project_id, from_id, to_id, kind) dedup.
	pickCanonical := func(syms []db.Symbol) string {
		canonical := syms[0].ID
		for i := 1; i < len(syms); i++ {
			if syms[i].ID < canonical {
				canonical = syms[i].ID
			}
		}
		return canonical
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
		canonical := pickCanonical(syms)
		qnCache[qn] = canonical
		return canonical
	}
	// lookupFromQN disambiguates FromQN by FromFile when known
	// (#487). Multiple `package main` directories in one project
	// produce N symbols sharing QN `main.main`; pickCanonical alone
	// would attribute every deferred edge to the lex-smallest path.
	// Prefer the symbol whose file_path matches FromFile (the file
	// that produced the deferred candidate); fall back to project-
	// wide pickCanonical when FromFile is empty (older candidates
	// without the field) or doesn't match any symbol's path. Not
	// cached because FromFile makes the key (qn, fromFile) — the
	// project-wide QN lookup IS cached above.
	lookupFromQN := func(qn, fromFile string) string {
		if qn == "" {
			return ""
		}
		if fromFile == "" {
			return lookupQN(qn)
		}
		syms, err := idx.store.GetSymbolsByQN(projectID, qn)
		if err != nil || len(syms) == 0 {
			return ""
		}
		for _, s := range syms {
			if s.FilePath == fromFile {
				return s.ID
			}
		}
		return pickCanonical(syms)
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
		// limit=200 so pickCanonical sees enough candidates to pick a
		// stable representative; limit=1 would just pick whichever
		// SQLite returns first under its implementation-defined order.
		// Two functions sharing a short name across a 200-package
		// project is the realistic upper bound this guards against.
		syms, err := idx.store.GetSymbolsByName(projectID, name, 200)
		if err != nil || len(syms) == 0 {
			nameCache[name] = ""
			return ""
		}
		canonical := pickCanonical(syms)
		nameCache[name] = canonical
		return canonical
	}

	// #423 piece 3: receiver-type-aware resolution. Load all
	// struct_fields rows once and index them by struct QN so the
	// inner loop can look up `recv.field` types in O(1). When the
	// load fails (corrupt DB, brand-new project) we silently skip
	// receiver-type resolution — the existing fallbacks still run.
	// Map shape: structQN → fieldName → fieldType (Go-syntax).
	fieldsByStructQN := map[string]map[string]string{}
	if rows, err := idx.store.LoadStructFields(projectID); err == nil {
		for _, f := range rows {
			// struct_id format is "<file>::<QN>#<Kind>"; recover the QN.
			structQN := ""
			if i := strings.Index(f.StructID, "::"); i >= 0 {
				rest := f.StructID[i+2:]
				if j := strings.LastIndex(rest, "#"); j > 0 {
					structQN = rest[:j]
				}
			}
			if structQN == "" {
				continue
			}
			m := fieldsByStructQN[structQN]
			if m == nil {
				m = map[string]string{}
				fieldsByStructQN[structQN] = m
			}
			m[f.FieldName] = f.FieldType
		}
	} else {
		slog.Warn("pincher.struct_fields.load.err", "err", err)
	}

	// resolveByReceiverType implements #423: when an unresolved CALLS
	// candidate carries a receiver-type hint, try to bind it precisely
	// before the project-wide receiver-method fallback. Two shapes:
	//
	//   1. ToName = "recv.Method"        — direct method on the receiver
	//   2. ToName = "recv.field.Method"  — chained through a struct field
	//
	// Both cases derive the package from the caller's FromQN (same-
	// package only — qualified types like `*foo.Bar` are skipped until
	// import-graph awareness lands). The lookup uses the structured
	// (parent QN, method name) pair, so it doesn't suffer from the
	// polymorphic-method false-bind that the existing fallback guards
	// against with isPolymorphicInterfaceMethodName.
	resolveByReceiverType := func(toName, receiverType, fromQN string) string {
		if receiverType == "" || toName == "" || fromQN == "" {
			return ""
		}
		// Caller's package = first dot-segment of FromQN. Methods are
		// QN'd as `pkg.<recvType>.<name>`, so the package is segment 0.
		pkg := fromQN
		if i := strings.Index(pkg, "."); i > 0 {
			pkg = pkg[:i]
		}
		bareRecv := strings.TrimPrefix(receiverType, "*")
		// Skip qualified receiver types (e.g. `*foo.Bar`) — without
		// import-graph info we can't map "foo" to its actual package.
		if strings.Contains(bareRecv, ".") {
			return ""
		}
		structQN := pkg + "." + bareRecv
		segments := strings.Split(toName, ".")
		switch len(segments) {
		case 2:
			// recv.Method — Method's QN is `pkg.<recvType>.<method>`
			// using the receiver-type expression as written (Go's QN
			// builder includes the * for pointer receivers).
			methodQN := pkg + "." + receiverType + "." + segments[1]
			return lookupQN(methodQN)
		case 3:
			// recv.field.Method — look up field type via struct_fields.
			fieldType, ok := fieldsByStructQN[structQN][segments[1]]
			if !ok {
				return ""
			}
			bareField := strings.TrimPrefix(fieldType, "*")
			// Same-package only for v0.19 — qualified field types
			// (`io.Writer`, `*foo.Bar`) need import-graph awareness.
			if strings.Contains(bareField, ".") {
				return ""
			}
			// Try both pointer and value receiver QNs since the field's
			// Go type doesn't tell us how the method was declared.
			for _, recvForm := range []string{"*" + bareField, bareField} {
				methodQN := pkg + "." + recvForm + "." + segments[2]
				if id := lookupQN(methodQN); id != "" {
					return id
				}
			}
		}
		return ""
	}

	seen := make(map[string]bool)
	edges := make([]db.Edge, 0, len(pending))
	for _, e := range pending {
		fromID := lookupFromQN(e.FromQN, e.FromFile)
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
		// #423 piece 3: precise receiver-type binding before the
		// looser project-wide receiver-method fallback.
		if toID == "" {
			toID = resolveByReceiverType(e.ToName, e.ReceiverType, e.FromQN)
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
				// #465: skip when the trailing method name is a
				// polymorphic interface method (String, Error, Read,
				// Write, Lock, ...). isStdlibReceiver caught the case
				// where the receiver itself is a stdlib package; this
				// catches the harder case of `localVar.Method(...)`
				// where the receiver name doesn't reveal stdlib origin
				// but the method name does — every `.String()` in the
				// project was binding to the single local Method named
				// String.
				if !isStdlibReceiver(receiver) && !isPolymorphicInterfaceMethodName(trailing) {
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
			Source:     "resolve_pass",
		})
	}

	// #475: atomic replace of the prior resolve pass's CALLS edges so
	// rule changes (e.g. the #465 polymorphic-method blocklist) converge
	// without --force on every incremental re-index. Per-file CALLS
	// edges keep their cascade-on-delete model.
	if err := idx.store.DeleteEdgesByKindAndSource(projectID, "CALLS", "resolve_pass"); err != nil {
		slog.Warn("pincher.calls.delete_prior.err", "err", err)
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
		// #428: pick the lexicographically smallest ID for stability
		// across re-index runs. SQLite returns rows in
		// implementation-defined order without an ORDER BY, so a naive
		// syms[0] resolves the same logical edge to different IDs
		// across runs and defeats the
		// UNIQUE(project_id, from_id, to_id, kind) dedup.
		canonical := 0
		for i := 1; i < len(syms); i++ {
			if syms[i].ID < syms[canonical].ID {
				canonical = i
			}
		}
		v := lookup{id: syms[canonical].ID, lang: syms[canonical].Language, isVar: syms[canonical].Kind == "Variable"}
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
			Source:     "resolve_pass",
		})
	}

	// #475: atomic replace of the prior resolve pass's READS + WRITES.
	// Both kinds share the read-pass output, so wipe both before insert.
	for _, k := range []string{"READS", "WRITES"} {
		if err := idx.store.DeleteEdgesByKindAndSource(projectID, k, "resolve_pass"); err != nil {
			slog.Warn("pincher.reads.delete_prior.err", "kind", k, "err", err)
		}
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
