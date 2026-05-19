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
	"os/exec"
	"path/filepath"
	"sort"
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
//
// StartedAt is set when the IndexProgress struct is created (at the
// top of Index()) and lets clients compute elapsed time + ETA via
// `(now - StartedAt) / FilesDone * (FilesTotal - FilesDone)`. Stored
// as Unix nanoseconds in an atomic so GetProgress doesn't need to
// take the indexer lock to read it (#535).
type IndexProgress struct {
	FilesDone     atomic.Int64
	FilesTotal    atomic.Int64
	StartedAtUnix atomic.Int64
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

	// currentBranchByProject — populated at Index() start with the
	// detected git branch and consumed by flushBatch to stamp
	// Symbol.Branch / Edge.Branch on every per-file write (#1303
	// Phase 2a). Cleared in the same defer that drops idx.active.
	// Concurrent Index() runs on different projects each have their
	// own entry; the sync.Map handles the cross-goroutine read of
	// projectID's value during the per-file flush goroutines.
	currentBranchByProject sync.Map // projectID → string

	// branchCacheByProject caches the result of detectGitBranch per
	// project for branchCacheTTL so the Watch() poll cycle doesn't
	// fork `git rev-parse` every 2 seconds (#1303 Phase 2a +
	// regression caught by TestIndex_NoChange_SkipsResolvePass —
	// the subprocess spawn adds ~100 allocations per tick, blowing
	// the 800-alloc budget the watcher gates on). Branches change
	// rarely; a 30-second TTL means the doctor advisory still picks
	// up checkout-without-reindex within one user-perceivable poll
	// of the moment they re-run any pincher tool.
	branchCacheByProject sync.Map // projectID → branchCacheEntry

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

	// onEvent, when set, is invoked at index_started and index_complete
	// so a subscriber (the MCP server's /v1/events SSE bus, #654) can
	// stream lifecycle events. nil for the bare `pincher index` CLI,
	// which has no subscriber — emitEvent guards the nil. The callback
	// must not block: the server's bus does a non-blocking fan-out.
	onEvent func(eventType string, payload map[string]any)
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

// SetEventHook registers a callback invoked at index_started and
// index_complete (#654). The MCP server wires its /v1/events SSE bus
// here; the bare CLI leaves it nil. The callback runs on the indexing
// goroutine, so it must return promptly — the server's bus fan-out is
// non-blocking by design.
func (idx *Indexer) SetEventHook(fn func(eventType string, payload map[string]any)) {
	idx.onEvent = fn
}

// emitEvent invokes the registered event hook, if any. nil-safe so the
// CLI path (no subscriber) is a no-op.
func (idx *Indexer) emitEvent(eventType string, payload map[string]any) {
	if idx.onEvent != nil {
		idx.onEvent(eventType, payload)
	}
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

// MarkActiveForTest stamps the indexer's active+progress state for the
// given project so tests can exercise the "indexer mid-pass" branch of
// consumers (e.g. server-side `_meta.index_in_progress` injection) without
// having to spawn a real indexing pass. Production code never calls this
// directly — naming carries the contract. Pair with UnmarkActiveForTest in
// a defer to keep state isolated across test cases.
func (idx *Indexer) MarkActiveForTest(projectID string, done, total int64) {
	idx.mu.Lock()
	if idx.active == nil {
		idx.active = make(map[string]bool)
	}
	idx.active[projectID] = true
	idx.mu.Unlock()
	p := &IndexProgress{}
	p.FilesDone.Store(done)
	p.FilesTotal.Store(total)
	idx.progress.Store(projectID, p)
}

// UnmarkActiveForTest reverts MarkActiveForTest.
func (idx *Indexer) UnmarkActiveForTest(projectID string) {
	idx.mu.Lock()
	delete(idx.active, projectID)
	idx.mu.Unlock()
	idx.progress.Delete(projectID)
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

// GetProgressDetail returns progress + the StartedAtUnix timestamp.
// Used by HTTP /v1/index-progress to compute ETA + files/sec without
// the caller needing to know about the IndexProgress struct shape
// (#535). Returns startedAtUnix==0 when there's no progress entry
// (project not currently indexing); callers should treat that as
// "no ETA data available."
func (idx *Indexer) GetProgressDetail(projectID string) (done, total, startedAtUnix int64, active bool) {
	idx.mu.Lock()
	active = idx.active[projectID]
	idx.mu.Unlock()
	if v, ok := idx.progress.Load(projectID); ok {
		p := v.(*IndexProgress)
		return p.FilesDone.Load(), p.FilesTotal.Load(), p.StartedAtUnix.Load(), active
	}
	return 0, 0, 0, active
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

	// #1231 v0.66 DOGFOOD: post-pass parity check. ParityMismatchFiles
	// is the count of files whose persisted symbol count is < 90% of
	// extracted. ParityMissingSymbols is the sum of (expected - actual)
	// across those files. Both default to 0 on healthy runs. Non-zero
	// is a strong signal of silent persistence loss — see #1231 for the
	// root-cause investigation.
	ParityMismatchFiles   int
	ParityMissingSymbols  int
}

// #1338: pending-edge threshold above which the resolve block pre-loads
// the full project symbol-by-QN map (one bulk SELECT). Below the
// threshold, each resolveX runs per-call GetSymbolsByQN — cheaper
// because the bulk-scan + map-build cost would dominate on small
// projects. Measured crossover: ~500 pending edges on a 600-file Go
// project. 1000 sets a conservative floor so the gate is a win, not
// a loss, on YAML/JSON-heavy corpora where pending counts stay low.
const qnPreloadThreshold = 1000

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

	// #1303 Phase 2a: detect the git branch once per Index() call. The
	// value flows into projects.current_branch (via UpsertProjectMeta
	// below and UpsertProject at the tail) AND onto every Symbol/Edge
	// stamped by flushBatch via the per-project cache. Empty when the
	// project root isn't a git working tree; detached-HEAD falls back
	// to a short commit SHA so the value stays meaningful for diagnostics.
	//
	// detectGitBranchCached fronts the subprocess spawn with a 30s
	// per-project TTL so the Watch() poll cycle (every 2s active /
	// 30s idle) doesn't allocate a `git rev-parse` per tick on
	// no-change runs. force=true bypasses the cache so a manual
	// `pincher index --force` after a branch switch picks up the
	// new branch immediately rather than waiting out the TTL.
	currentBranch := idx.detectGitBranchCached(projectID, absPath, force)
	idx.currentBranchByProject.Store(projectID, currentBranch)
	defer idx.currentBranchByProject.Delete(projectID)

	// #1163 traces half (indexer scope): one OTLP span per index pass.
	// Uses the global tracer provider — when no OTLP endpoint is
	// configured, this is a zero-allocation no-op pair. The span's
	// outcome attributes (files/symbols/edges/duration) are stamped at
	// the end of the function via finishIndexSpan; the lifecycle is a
	// defer so an early return still ends the span cleanly.
	ctx, span := startIndexSpan(ctx, projectID, projectName, absPath, force)
	defer span.End()

	// Serialise per-project (in-process).
	idx.mu.Lock()
	if idx.active[projectID] {
		idx.mu.Unlock()
		return nil, fmt.Errorf("project %q is already being indexed", projectName)
	}
	idx.active[projectID] = true
	idx.mu.Unlock()
	prog := &IndexProgress{}
	prog.StartedAtUnix.Store(time.Now().UnixNano())
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
	releaseLock, err := acquireProjectLock(filepath.Dir(idx.store.Path), projectID, idx.binaryVersion)
	if err != nil {
		return nil, err
	}
	defer releaseLock()

	start := time.Now()

	// #654: index_started — emitted before UpsertProject so the
	// file_count_estimate can carry the prior run's count (UpsertProject
	// would otherwise overwrite it). Best-effort; a missing prior row
	// just yields estimate 0.
	var fileCountEstimate int
	// #936: When the running binary's version differs from the project's
	// last-indexed binary_version, treat the run as if force=true. The
	// canonical case: file's content hasn't changed but the extractor
	// HAS — e.g. python_extract.py was indexed by the pre-#856 regex
	// path, hash matched on the next index, so the new Python-AST
	// extractor never ran. Same shape affects any extractor/resolver
	// upgrade that's behaviorally significant but doesn't change file
	// content. Empty binary_version on either side (legacy index, dev
	// build) opts out of the force — we don't want one-shot CLI runs
	// with --version=dev to nuke the project's hash cache on every call.
	var binaryDriftForce bool
	// #986: capture the prior binary_version BEFORE the start-of-pass
	// UpsertProjectMeta. Pre-fix, the start stamp wrote idx.binaryVersion
	// — so any interrupted drift-reindex left the row claiming the new
	// version while the symbols table was partial. The next startup then
	// saw `prev.BinaryVersion == idx.binaryVersion`, no drift detected,
	// no re-reindex triggered. End state: 30% symbol coverage stuck on
	// the new binary_version stamp.
	//
	// The end-of-pass UpsertProject (line 772) writes the new version
	// only on successful completion. The start UpsertProjectMeta now
	// writes the OLD value (or "" when the project is new), so a
	// crashed/killed run leaves drift detection intact for the retry.
	var priorBinaryVersion string
	if prev, _ := idx.store.GetProject(projectID); prev != nil {
		fileCountEstimate = prev.FileCount
		priorBinaryVersion = prev.BinaryVersion
		if prev.BinaryVersion != "" && idx.binaryVersion != "" && prev.BinaryVersion != idx.binaryVersion {
			binaryDriftForce = true
			// #1497 v0.83: suppress the force-reindex cascade when the
			// schema migrations applied during this Open() are all
			// invalidatesNothing — DDL-only entries that don't touch
			// extractor surface. Pre-gate, every `make install` + `/mcp`
			// reconnect triggered full re-extraction of every project
			// regardless of whether the binary's extractor side actually
			// changed. For a typical schema-only bump on the pincher-repo
			// (~700 files × 30-40 ms = 25-30s) ÷ 6 parallel projects ≈
			// 150s cumulative cost paid by every user on every `/mcp`
			// reconnect that happened to coincide with a migration. See
			// internal/index/binary_drift_gate.go for the truth table.
			inv, migFrom, migTo := idx.store.LastStartupMigrationInvalidates()
			if shouldSuppressBinaryDriftForce(inv, migFrom, migTo) {
				binaryDriftForce = false
				slog.Info("pincher.index.binary_drift_force_suppressed",
					"reason", "schema_migrations_invalidatesNothing",
					"migrations_applied_from", migFrom,
					"migrations_applied_to", migTo,
					"project_id", projectID,
					"indexed_with", prev.BinaryVersion,
					"running", idx.binaryVersion)
			} else if len(inv.Languages) > 0 && !inv.All {
				// #1543 v0.84: language-scoped selective invalidation.
				// Only files whose extracted symbols include any listed
				// language get their hash cleared — those files re-run
				// on the normal Index() pass; all other files keep
				// their hash and skip re-extraction. Falls back to the
				// full force-reindex if the targeted clear errors
				// (under-invalidating is a silent correctness bug).
				cleared, clearErr := idx.store.ClearFileHashesByLanguage(projectID, inv.Languages)
				if clearErr != nil {
					slog.Warn("pincher.index.language_scoped_invalidation_failed",
						"project_id", projectID,
						"languages", inv.Languages,
						"err", clearErr,
						"fallback", "full_force_reindex")
				} else {
					binaryDriftForce = false
					slog.Info("pincher.index.binary_drift_force_language_scoped",
						"reason", "schema_migrations_language_scoped",
						"languages", inv.Languages,
						"files_cleared", cleared,
						"migrations_applied_from", migFrom,
						"migrations_applied_to", migTo,
						"project_id", projectID,
						"indexed_with", prev.BinaryVersion,
						"running", idx.binaryVersion)
				}
			} else {
				slog.Info("pincher.index.binary_drift_force_reindex",
					"project_id", projectID,
					"indexed_with", prev.BinaryVersion,
					"running", idx.binaryVersion)
			}
		}
	}
	idx.emitEvent("index_started", map[string]any{
		"project_id":          projectID,
		"project":             projectName,
		"path":                absPath,
		"started_at":          start.UTC().Format(time.RFC3339),
		"file_count_estimate": fileCountEstimate,
	})

	// Ensure project record exists. #304: stamp the running binary
	// version so health can detect drift later — empty when the
	// caller didn't plumb SetBinaryVersion. #894: use the *Meta variant
	// so the counts from the previous index run aren't zeroed during
	// the brief window before UpdateProjectCounts catches up — health
	// previously reported 0 files / 0 symbols / 0 edges in that gap
	// even though the underlying tables were intact.
	//
	// #986: write the PRIOR binary_version at start. The end-of-pass
	// UpsertProject flips it to the running version only on successful
	// completion. An interrupted pass therefore leaves drift detection
	// intact for the next startup retry, rather than silently locking
	// in a partial index under the new version stamp.
	if err := idx.store.UpsertProjectMeta(db.Project{
		ID:            projectID,
		Path:          absPath,
		Name:          projectName,
		IndexedAt:     start,
		BinaryVersion: priorBinaryVersion,
		CurrentBranch: currentBranch,
	}); err != nil {
		return nil, fmt.Errorf("upsert project: %w", err)
	}

	// Best-effort Go module path (from go.mod). Used by the Go extractor to
	// rewrite intra-module imports to within-module paths so IMPORTS edges
	// can resolve across files. Missing or malformed go.mod just disables
	// the rewrite — external imports stay unresolved as before.
	modulePath := readGoModulePath(absPath)

	// Python source-roots (e.g. "src" for a src-layout repo) are only
	// consumed inside the resolve block below — gated by #1314 to skip
	// on no-change ticks. Computing them here would be dead work
	// (#1317: a full WalkDir per tick for ~13% of post-#1314 watcher
	// allocations); declare here so the variable is in scope for the
	// gated block, but defer the actual scan.
	var pythonRoots []string

	// #1613 v0.85 follow-up: extraction-phase wall-clock timing so
	// dogfooders can compare extract_ms vs resolve_ms (already in
	// pincher.resolve_block.summary) vs gc_ms (already in
	// pincher.index.gc.summary) — answering "where does the time go
	// on this corpus?" without re-running with a profiler attached.
	// Covers walker + per-file goroutine fan-out + wg.Wait + final
	// flush — i.e. everything from walker launch to "every extracted
	// symbol/edge is in the DB."
	extractionStart := time.Now()

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
		pendingUsesVar []ast.ExtractedEdge // deferred Ansible USES_VAR (#1165): resolved against Setting symbols in canonical var-decl paths
		bufMu          sync.Mutex
		lastStatsFlush time.Time           // throttle for in-flight project counts; guarded by bufMu
		seenFiles      = map[string]bool{} // #326: relPaths the walker yielded this run; tail-pass GC's symbols for files NOT in this set. Populated in the main (single-threaded) loop so no mutex.
		// #1231 v0.66 DOGFOOD: per-file expected symbol count. Filled
		// in the per-file goroutine (after disambiguateDuplicates + the
		// symBuf append, under bufMu). Compared post-pass against the
		// DB COUNT(*) GROUP BY file_path to detect silent symbol loss
		// during persistence — the bug shape where pincher-repo's
		// server.go has 73 source methods but only 8 in the index after
		// a force-reindex.
		expectedPerFile = map[string]int{}
		// #1613 v0.85 follow-up: per-language extraction timing.
		// Per-file goroutine accumulates duration into this map keyed
		// by language; protected by bufMu. Emitted at end-of-extraction
		// via pincher.index.extraction.by_language so dogfooders can
		// see which extractor dominates.
		extractByLang      = map[string]time.Duration{}
		extractCountByLang = map[string]int{}
		// #1627 v0.85 follow-up: per-phase CPU-time accumulators across
		// all per-file goroutines. atomic.Int64 nanos because per-file
		// goroutines run concurrently. Wall-clock ≈ (extract + delete +
		// post) / effective_parallelism + idle. Emit at end of
		// extraction phase. Includes lock-wait — these are wall-clock-
		// per-goroutine sums, not pure CPU-time.
		deleteSymsNS  atomic.Int64
		extractNS     atomic.Int64
		postExtractNS atomic.Int64
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
		// #996: skip extraction for files inside fixture paths (testdata/
		// __fixtures__ / etc.) when they are NOT the project's own root.
		// Pre-fix, external projects like warp-fork (4360 files, mostly
		// JSON test fixtures) extracted every JSON object as a symbol,
		// producing 1.45M symbols / 247 edges — a ratio that wedged
		// graph queries, blew up the dashboard, and made aggregate
		// answers misleading. The fixture path detector already exists
		// (used by resolution at #750) but was never wired into the
		// indexer's per-file gate. Compute relPath here so we can skip
		// BEFORE reading file bytes / hashing / extracting.
		//
		// pincher's own pinned-corpus tests are unaffected: when a
		// corpus is indexed as its OWN project root, file paths are
		// relative to the corpus dir and isFixturePath returns false
		// (the heuristic checks for `/testdata/` etc. in the relative
		// path, which is absent at corpus-root indexing time).
		relPathProbe, _ := filepath.Rel(absPath, path)
		relPathProbe = filepath.ToSlash(relPathProbe)
		if isFixturePath(relPathProbe) {
			totalBlocked++
			slog.Debug("pincher.index.fixture_skip", "path", path, "relPath", relPathProbe)
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
				if err := idx.store.RecordExtractionFailureWithBinary(projectID, relPath, ast.DetectLanguage(path), "file_too_large", details, idx.binaryVersion); err != nil {
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

		if !force && !binaryDriftForce {
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
				// #1313: even unrecognised-language files were walked +
				// hashed; stamp the hash so the next pass's content-
				// hash check fires and skips. Without this, every
				// unknown-extension file (e.g. .lock, .min.js once we
				// blocklist-skip them, etc.) costs a full walk every
				// Watch tick forever.
				_ = idx.store.SetFileHash(projectID, relPath, hash)
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
			// #1627 v0.85 follow-up: time the pre-extract DB delete.
			deleteStart := time.Now()
			if delErr := idx.store.DeleteSymbolsForFile(projectID, relPath); delErr != nil {
				slog.Warn("pincher.index.delete_stale.err", "err", delErr, "file", relPath)
			}
			deleteSymsNS.Add(int64(time.Since(deleteStart)))

			// Three-layer extraction in one pass.
			//
			// Wrapped in recover() (#42 part 1): if an extractor panics on
			// pathological input, we catch it, persist the failure, and skip
			// the file rather than crashing the whole indexer goroutine.
			// Without this, one malformed file kills the entire index run.
			extractStart := time.Now()
			result := safeExtractWithModule(idx, projectID, lang, relPath, content, modulePath)
			extractDur := time.Since(extractStart)
			bufMu.Lock()
			extractByLang[lang] += extractDur
			extractCountByLang[lang]++
			bufMu.Unlock()
			extractNS.Add(int64(extractDur))
			postExtractStart := time.Now()
			defer func() { postExtractNS.Add(int64(time.Since(postExtractStart))) }()
			if result == nil || (len(result.Symbols) == 0 && len(result.Edges) == 0) {
				// #1313: zero-symbol extraction is a legitimate
				// outcome — empty test fixtures (`package X` only),
				// Markdown with no headings and no preamble, YAML
				// with no extractable settings, etc. Pre-fix, these
				// files never got a `files` row written (the row is
				// stamped inside flushBuffers, which only iterates
				// the symbols slice), so every subsequent watcher
				// tick re-walked + re-extracted them forever. User-
				// reported repro: 16 ghost files re-extracted every
				// pass on pincherMCP-on-Mac. Stamp the hash here so
				// the next pass's hash-check at line ~486 fires and
				// skips.
				//
				// #1165: relaxed to also stay in the pipeline when a
				// file extracted ZERO symbols but DID yield deferred
				// edges (`.j2` templates that contain only
				// `{{ var_name }}` output expressions are the canonical
				// case). Otherwise USES_VAR refs from those files would
				// silently drop here before reaching the deferred-edge
				// loop / persistence below.
				_ = idx.store.SetFileHash(projectID, relPath, hash)
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
			deferredUsesVar := make([]ast.ExtractedEdge, 0)
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
				if e.Kind == "USES_VAR" {
					// #1165: USES_VAR always defers — the target is a Setting
					// symbol whose canonical declaration lives in a sibling
					// file (group_vars/, host_vars/, roles/*/defaults/, ...).
					// Per-file nameToID never sees those, so binding has to
					// wait for the project-wide pass.
					deferredUsesVar = append(deferredUsesVar, e)
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
					// Python CALLS are AST-grade (the regex extractor doesn't
					// emit any), so they're as trustworthy as Go's for cross-
					// file resolution. Other regex extractors still get the
					// drop-on-per-file-miss policy to keep noise out.
					//
					// #1177 v0.72: TS/JS CALLS with a non-empty ReceiverType
					// also defer — the receiver-type hint bounds the
					// noise that would otherwise come from broad-deferral
					// of regex-tier CALLS, and the resolver's class-name
					// lookup needs the project-wide symbol pool to find
					// the Class.method target. Edges with empty
					// ReceiverType (bare free-function calls) still drop
					// on per-file miss to preserve the noise floor.
					if e.Kind == "CALLS" && (lang == "Go" || lang == "Python") {
						deferredCalls = append(deferredCalls, e)
					} else if e.Kind == "CALLS" && e.ReceiverType != "" {
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
			fileDeferred := make([]db.PendingEdge, 0, len(deferredImports)+len(deferredCalls)+len(deferredReads)+len(deferredUsesVar))
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
						BaseType:     e.BaseType,
					})
				}
			}
			appendDeferred(deferredImports)
			appendDeferred(deferredCalls)
			appendDeferred(deferredReads)
			appendDeferred(deferredUsesVar)
			// #423 piece 2: persist Go struct field maps so the resolver
			// can follow recv.field.method calls. No-op for files
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

			// #493: persist Go interface method-name sets so dead_code
			// can mark project-internal methods that satisfy an
			// interface as not-dead. No-op for files with no Interface
			// symbols.
			fileMethods := make([]db.InterfaceMethod, 0)
			for _, sym := range result.Symbols {
				if sym.Kind != "Interface" || len(sym.InterfaceMethods) == 0 {
					continue
				}
				ifaceID := db.MakeSymbolID(relPath, sym.QualifiedName, sym.Kind)
				for _, mname := range sym.InterfaceMethods {
					fileMethods = append(fileMethods, db.InterfaceMethod{
						ProjectID:   projectID,
						InterfaceID: ifaceID,
						MethodName:  mname,
					})
				}
			}

			// #1627 v0.86: single-transaction commit for all four
			// per-file post-extract writes. v0.85 observability showed
			// extraction-phase wall-clock was 73% writer-pool
			// serialization (#1628 measurement: 2064ms of 2840ms
			// extraction-phase on pincher-repo force-reindex was lock
			// contention, not extractor CPU). Collapsing four
			// independent Begin/Commit cycles per file (pending_edges,
			// struct_fields, interface_methods, files.hash) into one
			// removes the dominant source of contention.
			//
			// #1313: file_hash is part of this commit, so the v0.84
			// invariant — "hash is stamped on every successful
			// extraction independent of batch-flush success" — still
			// holds: the per-file tx commits atomically before
			// flushBuffers ever runs, and a flushBatch failure later
			// on the shared symBuf can't roll this back.
			if err := idx.store.CommitFileExtraction(db.FileExtractionCommit{
				ProjectID:        projectID,
				FilePath:         relPath,
				FileHash:         hash,
				PendingEdges:     fileDeferred,
				StructFields:     fileFields,
				InterfaceMethods: fileMethods,
			}); err != nil {
				slog.Warn("pincher.file_extraction.commit.err", "file", relPath, "err", err)
				// Fallback: write the file hash directly so the next
				// pass still skips this file. The four sub-commits
				// failed atomically, but the unchanged-skip path
				// (line 623 / 680 SetFileHash) must remain reachable
				// even when CommitFileExtraction errors.
				_ = idx.store.SetFileHash(projectID, relPath, hash)
			}

			refreshCounts := false
			bufMu.Lock()
			symBuf = append(symBuf, syms...)
			edgeBuf = append(edgeBuf, edges...)
			pendingImport = append(pendingImport, deferredImports...)
			pendingCalls = append(pendingCalls, deferredCalls...)
			pendingReads = append(pendingReads, deferredReads...)
			pendingUsesVar = append(pendingUsesVar, deferredUsesVar...)
			// #1231 v0.66 DOGFOOD: record this file's extracted-symbol
			// count under bufMu so the post-pass parity check
			// (Index() tail) can compare against the DB's
			// COUNT(*) GROUP BY file_path. Sum across multiple
			// re-extractions of the same file in one pass — rare
			// but possible if the walker yields a file twice (e.g.,
			// symlink loop the gocodewalker default doesn't catch).
			expectedPerFile[relPath] += len(syms)
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

	// #1613 v0.85 follow-up: extraction-phase summary. Emitted on every
	// run regardless of file count — even "0 files changed" watcher
	// ticks pay the walker cost, and that's useful signal in itself
	// (a 200ms walker tick on a 50k-file repo with no changes is the
	// expected steady-state cost; a 2s tick implies a problem).
	slog.Info("pincher.index.extraction.summary",
		"project_id", projectID,
		"files_extracted", totalFiles,
		"files_hash_skipped", totalSkipped,
		"files_blocked", totalBlocked,
		"duration_ms", time.Since(extractionStart).Milliseconds(),
	)

	// #1613 v0.85 follow-up: per-language extraction breakdown — only
	// emit when at least one file was extracted. Tells dogfooders which
	// extractor dominates the extraction cost (per pincher-repo data:
	// extraction is 82% of force-reindex wall-clock; per-language
	// numbers are needed to know whether Go's AST extractor, Markdown,
	// YAML, or something else is the actual bottleneck).
	if totalFiles > 0 {
		// Build per-language attrs sorted by total duration desc so
		// the slog line reads with the dominant cost first.
		type langStat struct {
			name  string
			dur   time.Duration
			count int
		}
		langs := make([]langStat, 0, len(extractByLang))
		for lang, d := range extractByLang {
			langs = append(langs, langStat{name: lang, dur: d, count: extractCountByLang[lang]})
		}
		sort.Slice(langs, func(i, j int) bool {
			return langs[i].dur > langs[j].dur
		})
		attrs := []any{
			"project_id", projectID,
			"total_languages", len(langs),
		}
		// Emit the top 10 languages by duration as flat key/value
		// pairs so structured-log consumers can grep `lang_N=`.
		// Anything beyond the top 10 is rolled into _other_ms /
		// _other_files so the line stays bounded.
		const topN = 10
		otherDur := time.Duration(0)
		otherCount := 0
		for i, l := range langs {
			if i < topN {
				attrs = append(attrs,
					fmt.Sprintf("lang_%d_name", i+1), l.name,
					fmt.Sprintf("lang_%d_ms", i+1), l.dur.Milliseconds(),
					fmt.Sprintf("lang_%d_files", i+1), l.count,
				)
				continue
			}
			otherDur += l.dur
			otherCount += l.count
		}
		if otherCount > 0 {
			attrs = append(attrs,
				"other_ms", otherDur.Milliseconds(),
				"other_files", otherCount,
				"other_languages", len(langs)-topN,
			)
		}
		slog.Info("pincher.index.extraction.by_language", attrs...)

		// #1627 v0.85 follow-up: per-phase CPU-time across all per-file
		// goroutines. Splits the extraction-phase wall-clock into:
		//   - delete_syms_ms: pre-extract DeleteSymbolsForFile (DB write)
		//   - extract_ms:     safeExtractWithModule (extractor CPU)
		//   - post_extract_ms: post-extract buffer mgmt + Replace* + SetFileHash + any mid-batch flushBuffers
		// Sum / effective_parallelism + idle ≈ wall-clock; includes
		// lock-wait, so the ratio matters more than absolutes.
		slog.Info("pincher.index.extraction.per_phase",
			"project_id", projectID,
			"files_extracted", totalFiles,
			"delete_syms_ms", time.Duration(deleteSymsNS.Load()).Milliseconds(),
			"extract_ms", time.Duration(extractNS.Load()).Milliseconds(),
			"post_extract_ms", time.Duration(postExtractNS.Load()).Milliseconds(),
			"wall_clock_ms", time.Since(extractionStart).Milliseconds(),
		)
	}

	// #457: resolve deferred edges against the FULL persisted candidate
	// pool, not just this run's in-memory pendingX slices. The DB rows
	// include hash-skipped files' candidates from prior runs — which
	// fixes #427's transitive edge-loss on incremental re-indexes.
	//
	// #670 §2: gate the load+resolve block on actual change. On a
	// watcher no-change tick (totalFiles == 0, not force), the
	// candidate pool and the symbol table are both unchanged since
	// the last run, so resolution would produce the same edges —
	// INSERT OR IGNORE no-ops. Pre-gate measurement: resolveReads
	// alone consumed 48% of allocations on the watcher hot path
	// (14× alloc regression vs v0.60 baseline). Safe because pending
	// edges only enter the table via per-file goroutines that
	// re-extract — and those increment totalFiles. The GC pass below
	// runs unconditionally and handles deleted files; resolve doesn't
	// clean up edges to deleted symbols anyway, so deletions don't
	// need to trigger a resolve.
	resolveChanged := force || totalFiles > 0
	var resolveBlockStart time.Time
	if resolveChanged {
		// #1613 v0.85 follow-up: total resolve-block timing so the
		// per-stage summary lines (pincher.imports/calls/reads/uses_var)
		// can be summed and compared against extraction wall-clock.
		// Emit at slog.Info immediately after the block closes (NOT
		// via defer — the function-scoped defer would fold in the GC
		// pass + parity check + project upsert that follow, and the
		// number wouldn't be comparable across watcher ticks where
		// only some of those subpasses run).
		resolveBlockStart = time.Now()

		// #1317: deferred from the top of Index() — only the resolve
		// block consumes pythonRoots, so on a no-change tick we skip
		// the WalkDir entirely.
		pythonRoots = ast.PythonSourceRoots(absPath)

		// #1338: pre-load the project's symbol-by-QN map once before
		// the resolve passes when the expected lookup volume justifies
		// the bulk SELECT. Pre-fix each resolveCalls / resolveReads
		// iteration ran one GetSymbolsByQN DB query per unique QN
		// (~20% of cold-path allocations on a Go-heavy project).
		//
		// Threshold gate: only pre-load when sum(pending) > qnPreloadThreshold.
		// On small projects (YAML-heavy K8s / sparse-edge Node) the bulk
		// scan allocates more than the per-call queries it saves. Measured
		// crossover lives around 500 pending edges on a 600-file project;
		// 1000 set conservative so the heuristic stays a win, not a loss.
		// On a LoadAllSymbolsByQN error, fall back to per-call lookups —
		// correctness preserved; only the speedup is lost.
		allImports := loadOrFallback(idx, projectID, "IMPORTS", pendingImport)
		allCalls := loadOrFallback(idx, projectID, "CALLS", pendingCalls)
		allReads := loadOrFallback(idx, projectID, "READS", pendingReads)
		allReads = append(allReads, loadOrFallback(idx, projectID, "WRITES", nil)...)

		var qnMap map[string][]db.Symbol
		if len(allImports)+len(allCalls)+len(allReads) > qnPreloadThreshold {
			if loaded, err := idx.store.LoadAllSymbolsByQN(projectID); err == nil {
				qnMap = loaded
			} else {
				slog.Warn("pincher.resolve.qn_preload.err", "err", err)
			}
		}

		// #1613 v0.85: per-stage observability so the next perf bottleneck
		// (resolveImports vs resolveCalls vs resolveReads vs resolveUsesVar)
		// has data to argue from. Mirrors the existing
		// `pincher.uses_var.resolve.summary` shape (#1500). Emitted at
		// slog.Info only when pending_in > 0 — quiet on no-op ticks.
		// `dropped` is pending_in minus resolved_out (conflates real drops
		// with dedupe; internal counter PR can split). Unlocks #1611
		// (parallelize resolve passes) which needs per-stage numbers.
		runResolve := func(kind string, pendingIn int, run func() int) {
			if pendingIn == 0 {
				return
			}
			startStage := time.Now()
			n := run()
			if n > 0 {
				totalEdges += n
			}
			slog.Info("pincher.resolve.summary",
				"kind", kind,
				"project_id", projectID,
				"pending_in", pendingIn,
				"resolved_out", n,
				"dropped", pendingIn-n,
				"duration_ms", time.Since(startStage).Milliseconds(),
			)
		}

		runResolve("IMPORTS", len(allImports), func() int {
			return idx.resolveImports(projectID, allImports, pythonRoots, qnMap)
		})
		runResolve("CALLS", len(allCalls), func() int {
			return idx.resolveCalls(projectID, allCalls, pythonRoots, qnMap)
		})
		runResolve("READS", len(allReads), func() int {
			return idx.resolveReads(projectID, allReads, qnMap)
		})

		// #1165 Phase 2: bind Ansible USES_VAR refs to Setting symbols in
		// canonical var-declaration paths. Loads its own pool (no
		// pre-load reuse with QN-keyed allImports/allCalls/allReads —
		// USES_VAR resolution is name-keyed, not QN-keyed).
		allUsesVar := loadOrFallback(idx, projectID, "USES_VAR", pendingUsesVar)
		// resolveUsesVar has its own `pincher.uses_var.resolve.summary`
		// emission with richer drop counters (#1500 for #1479) — keep its
		// internal log canonical and just match the same shape here for
		// the wrapper's `pincher.resolve.summary`. Both lines safely
		// coexist; agents grepping for either prefix get a hit.
		runResolve("USES_VAR", len(allUsesVar), func() int {
			return idx.resolveUsesVar(projectID, allUsesVar)
		})

		// #1613 v0.85 follow-up: total resolve-block duration. Allows
		// summing the per-stage durations against this aggregate to
		// see whether wrapper overhead exists. Pre-fix only the
		// per-stage lines existed; the aggregate was implicit.
		slog.Info("pincher.resolve_block.summary",
			"project_id", projectID,
			"duration_ms", time.Since(resolveBlockStart).Milliseconds(),
		)
	}

	// #326: Tail-pass GC for files removed from disk. The walker yields only
	// files that still exist; per-file goroutines call DeleteSymbolsForFile
	// only on files they visit. Without this pass, deleting a file from disk
	// leaves its symbols + file_hash row behind forever (paperclip in the
	// dogfood DB had 4820 orphan symbols and 0 files). Cheap: one SELECT
	// plus N small DELETEs at index tail; only fires when a file actually
	// disappeared.
	//
	// #756: the GC iterates the UNION of the `files` table and the distinct
	// file paths in `symbols`. Pre-fix it scanned only `files`, so symbols
	// whose `files` row was never written — a crash between flushBatch and
	// SetFileHash — stayed orphaned forever, invisible to the GC. The
	// per-file deletes are all idempotent, so reconsidering a path that has
	// symbols but no file_hash row (or vice versa) is safe.
	// #1613 v0.85 follow-up: tail GC observability. Pre-fix the GC pass
	// ran with no timing or per-file count signal — on a corpus where
	// the user deletes 1000 files between index runs it could become
	// the dominant cost without us knowing. Track gc_paths_considered
	// (size of the union scan), totalDeleted (files actually reaped),
	// and duration_ms. Only emit when non-trivial (paths considered >
	// 50 or any deletions happened) so the healthy-corpus happy path
	// stays quiet.
	gcStart := time.Now()
	var totalDeleted int
	gcPaths := map[string]bool{}
	if storedFiles, listErr := idx.store.ListFilesForProject(projectID); listErr == nil {
		for _, p := range storedFiles {
			gcPaths[p] = true
		}
	} else {
		slog.Warn("pincher.index.gc.list.err", "err", listErr)
	}
	if symPaths, listErr := idx.store.ListSymbolFilePaths(projectID); listErr == nil {
		for _, p := range symPaths {
			gcPaths[p] = true
		}
	} else {
		slog.Warn("pincher.index.gc.list_sym_paths.err", "err", listErr)
	}
	for stored := range gcPaths {
		if seenFiles[stored] {
			continue
		}
		// #1340 v0.71: synthetic external Module symbols live at file
		// paths under `@external/` — the walker never sees those (they
		// have no on-disk counterpart by design), so the GC would
		// otherwise reap them every pass. resolveImports re-creates
		// them each run; skipping the GC here means the symbols
		// remain queryable between resolve passes too.
		if strings.HasPrefix(stored, "@external/") {
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
	// #1613 v0.85 follow-up: emit the GC summary when the pass did
	// non-trivial work — small thresholds (>50 paths considered OR
	// any deletions) keep the noise floor low while ensuring an
	// unexpected GC blowup is visible.
	if len(gcPaths) > 50 || totalDeleted > 0 {
		slog.Info("pincher.index.gc.summary",
			"project_id", projectID,
			"paths_considered", len(gcPaths),
			"files_reaped", totalDeleted,
			"duration_ms", time.Since(gcStart).Milliseconds(),
		)
	}

	// #1231 v0.66 DOGFOOD: post-pass parity check guard.
	parityMismatchFiles, parityMissingSymbols := idx.runParityCheck(projectID, projectName, expectedPerFile)

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
		CurrentBranch: currentBranch,
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

	// #652 phase 1: closure-table tail-pass. Off by default; opt-in via
	// PINCHER_CLOSURE_TABLES=1. Materializes the depth-N transitive
	// closure of the edges graph so trace queries become a single
	// indexed SELECT instead of a recursive CTE. Failure is logged but
	// non-fatal — the indexer reports success even if the closure pass
	// fails, since trace falls back to the recursive-CTE path.
	if db.ClosureEnabled() {
		closureStart := time.Now()
		closureDepth := db.ClosureMaxDepth()
		if err := idx.store.BuildClosure(ctx, projectID, closureDepth); err != nil {
			slog.Warn("pincher.closure.build.err", "project", projectName, "err", err)
		} else {
			n, _ := idx.store.ClosureRowCount(projectID)
			slog.Info("pincher.closure.built",
				"project", projectName,
				"rows", n,
				"max_depth", closureDepth,
				"ms", time.Since(closureStart).Milliseconds(),
			)
		}
	}

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

	result := &IndexResult{
		ProjectID:            projectID,
		Project:              projectName,
		Path:                 absPath,
		Files:                totalFiles,
		Symbols:              totalSymbols,
		Edges:                totalEdges,
		Skipped:              totalSkipped,
		Blocked:              totalBlocked,
		Deleted:              totalDeleted,
		DurationMS:           duration.Milliseconds(),
		ParityMismatchFiles:  parityMismatchFiles,
		ParityMissingSymbols: parityMissingSymbols,
	}

	// #654: index_complete — the single success return, so this fires
	// exactly once per completed run (error paths return earlier and
	// never reach here).
	idx.emitEvent("index_complete", map[string]any{
		"project_id":  projectID,
		"project":     projectName,
		"path":        absPath,
		"files":       totalFiles,
		"symbols":     totalSymbols,
		"edges":       totalEdges,
		"skipped":     totalSkipped,
		"blocked":     totalBlocked,
		"deleted":     totalDeleted,
		"duration_ms": duration.Milliseconds(),
	})

	// #1163 traces half (indexer scope): stamp the post-pass outcome
	// attributes on the OTLP span before defer span.End() fires.
	finishIndexSpan(span, totalFiles, totalSymbols, totalEdges, totalSkipped, totalBlocked, totalDeleted, duration.Milliseconds(), nil)

	return result, nil
}

// runParityCheck compares the per-file extracted-symbol counts (from
// the indexer's in-memory `expectedPerFile` map, filled under bufMu in
// the per-file goroutines) against the persisted DB counts (one
// COUNT(*) GROUP BY file_path SELECT). Any file whose actual count is
// < expected*0.9 (>10% loss) trips a per-file `slog.Warn` plus, when
// any losses are found, a single summary warning naming #1231.
// Returns (mismatch_files, missing_symbols) for IndexResult.
//
// Threshold rationale: a strict 100% equality check is unreliable —
// rare extractor paths (disambiguateDuplicates merging two extractor
// outputs with identical IDs after suffix) can legitimately produce
// small count drift. 90% catches the #1231 shape (8 of 73 methods =
// 89% loss) without false-positive on small edges.
//
// Failure handling: if the DB query itself fails, log the err and
// return (0, 0) — the parity check is observation-only, never gates
// a successful index run.
func (idx *Indexer) runParityCheck(projectID, projectName string, expectedPerFile map[string]int) (int, int) {
	actualCounts, err := idx.store.SymbolCountsByFile(projectID)
	if err != nil {
		slog.Warn("pincher.index.parity.query.err", "err", err)
		return 0, 0
	}
	mismatchFiles, missingSymbols := 0, 0
	for fp, expected := range expectedPerFile {
		if expected == 0 {
			continue
		}
		actual := actualCounts[fp]
		// >=90% retained = healthy; anything below trips the guard.
		if actual*10 >= expected*9 {
			continue
		}
		mismatchFiles++
		missingSymbols += expected - actual
		slog.Warn("pincher.index.parity.mismatch",
			"project", projectName,
			"file", fp,
			"expected", expected,
			"actual", actual,
			"missing", expected-actual,
		)
	}
	if mismatchFiles > 0 {
		slog.Warn("pincher.index.parity.summary",
			"project", projectName,
			"files_with_loss", mismatchFiles,
			"missing_symbols", missingSymbols,
			"hint", "see #1231 — silent persistence loss in shared-DB multi-Watch environments. Run `pincher index --force` to retry; if loss persists, capture this log and attach to the issue.",
		)
	}
	return mismatchFiles, missingSymbols
}

// externalModuleSymbol constructs a synthetic Module symbol that
// represents an import target the in-project resolver couldn't bind to
// any real symbol — `import os` (Python stdlib), `require('fs')` (Node
// builtin), `{% extends "base.html" %}` (Jinja template outside the
// indexed tree), `module "x" { source = "hashicorp/consul/aws" }` (HCL
// registry), `[See](other.md)` (Markdown link to external doc), etc.
//
// #1340 v0.71 option (a). The symbol lives at the sentinel file_path
// "@external/<sanitized-qn>" so future opt-out filters can identify
// these rows with a single LIKE-prefix predicate. The qualified_name
// is the literal to_name from the IMPORTS edge — keeps the symbol
// addressable by the same string the extractor emitted.
func externalModuleSymbol(projectID, toName string) db.Symbol {
	filePath := "@external/" + sanitizeExternalPath(toName)
	id := db.MakeSymbolID(filePath, toName, "Module")
	name := toName
	// Trim path-shape names down to the basename for display.
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 && i+1 < len(name) {
		name = name[i+1:]
	}
	return db.Symbol{
		ID:                   id,
		ProjectID:            projectID,
		FilePath:             filePath,
		Name:                 name,
		QualifiedName:        toName,
		Kind:                 "Module",
		Language:             "External",
		StartByte:            0,
		EndByte:              0,
		StartLine:            0,
		EndLine:              0,
		IsExported:           true,
		ExtractionConfidence: 1.0,
	}
}

// sanitizeExternalPath replaces characters that aren't valid file-path
// bytes on the filesystems pincher targets (Windows is the strict
// limiter — `<>:"|?*` are illegal). The file_path string is never
// passed to actual filesystem operations for synthetic externals, but
// it does flow through doctor / list / search displays where weird
// bytes would render as control characters or break filters.
func sanitizeExternalPath(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '<', '>', ':', '"', '|', '?', '*', '\x00':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// flushBuffers writes accumulated symbols and edges to the DB then resets the slices.
//
// File-hash rows are stamped by the per-file goroutine BEFORE symbols join
// the batch (see #1313). flushBuffers no longer re-stamps them — that path
// only covered files whose extraction yielded ≥1 symbol AND whose batch
// flushed successfully, silently dropping the hash for zero-symbol files
// and for any file caught in a batch that failed.
func (idx *Indexer) flushBuffers(projectID string, syms *[]db.Symbol, edges *[]db.Edge) error {
	if err := idx.flushBatch(projectID, *syms, *edges); err != nil {
		return err
	}
	*syms = (*syms)[:0]
	*edges = (*edges)[:0]
	return nil
}

func (idx *Indexer) flushBatch(projectID string, syms []db.Symbol, edges []db.Edge) error {
	// #1303 Phase 2a: stamp the current git branch on every Symbol/Edge
	// before upsert. Looked up from the per-project cache populated at
	// Index() start; flushBatch is the single chokepoint for per-file
	// writes so stamping here covers every extractor without threading
	// branch through their result types. Resolve-pass edges go through
	// their own paths and are stamped where they call BulkUpsertEdges.
	branch := idx.currentBranchFor(projectID)
	if branch != "" {
		for i := range syms {
			if syms[i].Branch == "" {
				syms[i].Branch = branch
			}
		}
		for i := range edges {
			if edges[i].Branch == "" {
				edges[i].Branch = branch
			}
		}
	}
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

// branchCacheEntry is the per-project cache value for the git-branch
// lookup used by detectGitBranchCached. #1303 Phase 2a.
//
// headContent (#1474 perf follow-up) is the verbatim contents of
// `<project>/.git/HEAD` at cache time. `git checkout` rewrites this
// file atomically — for a symbolic ref it holds e.g. `ref: refs/heads/main`,
// for a detached HEAD it holds the full SHA — so an unchanged HEAD file
// is a sufficient (and very cheap) proof that the branch hasn't moved
// since we last asked git. When the cached headContent matches the
// fresh file read, force=true callers can safely skip the
// `git rev-parse` subprocess. Empty string when the read failed (file
// missing, non-git directory, permission error) — in that case the
// content check is treated as a miss and the subprocess runs.
type branchCacheEntry struct {
	branch      string
	cachedAt    time.Time
	headContent string
}

const branchCacheTTL = 30 * time.Second

// detectGitBranchCached wraps detectGitBranch with two cache layers:
//
//   - The 30-second `branchCacheTTL` (time-based) covers the Watch()
//     poll cycle so a watcher tick doesn't allocate a subprocess per
//     tick (#1303 Phase 2a regression fix for
//     TestIndex_NoChange_SkipsResolvePass).
//   - A content-based check against `<project>/.git/HEAD` (#1474 perf
//     follow-up) covers the rapid-fire `force=true` case — e.g. the
//     schema-migration auto-restart that fires parallel reindex calls
//     across every project. Pre-fix, every force call unconditionally
//     re-ran `git rev-parse`; CPU profiling showed 42% of cumulative
//     time on the Force bench was in `os/exec.Cmd.Start` / subprocess
//     stdout reads. Skipping the subprocess when HEAD content is
//     unchanged eliminates the storm without weakening the
//     "deliberate `git checkout` + `pincher index --force` reflects
//     the new branch immediately" promise: a real checkout rewrites
//     `.git/HEAD`, the content check fails, and we re-detect.
//
// Both checks fail open: a stat/read error on `.git/HEAD` is treated as
// a content miss, so worktrees / packed refs / unusual setups fall back
// to the pre-fix subprocess path with no behaviour change.
func (idx *Indexer) detectGitBranchCached(projectID, absPath string, bypass bool) string {
	headContent := readGitHEAD(absPath)
	if v, ok := idx.branchCacheByProject.Load(projectID); ok {
		if e, ok := v.(branchCacheEntry); ok {
			age := time.Since(e.cachedAt)
			// Normal (non-force) callers use the 30s TTL alone.
			if !bypass && age < branchCacheTTL {
				return e.branch
			}
			// Force callers skip the subprocess when HEAD content is
			// proven unchanged. Empty headContent counts as a miss so
			// the subprocess path stays in effect for non-git dirs
			// and read failures.
			if bypass && headContent != "" && e.headContent == headContent {
				return e.branch
			}
		}
	}
	branch := detectGitBranch(absPath)
	idx.branchCacheByProject.Store(projectID, branchCacheEntry{
		branch:      branch,
		cachedAt:    time.Now(),
		headContent: headContent,
	})
	return branch
}

// readGitHEAD returns the trimmed contents of `<absPath>/.git/HEAD`, or
// "" on any read error. The trim makes the content stable across the
// trailing newline / CRLF / no-newline variants different git versions
// emit. Used by detectGitBranchCached as a cheap "did anything change
// since last call" probe in place of a subprocess spawn.
func readGitHEAD(absPath string) string {
	headPath := filepath.Join(absPath, ".git", "HEAD")
	data, err := os.ReadFile(headPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// currentBranchFor returns the git branch cached for projectID by the
// in-flight Index() call, or "" when no Index() is active for that
// project (e.g. legacy callers that wrote symbols outside the Index()
// dispatcher — none exist in the current tree, but the empty-fallback
// keeps the behaviour conservative). #1303 Phase 2a.
func (idx *Indexer) currentBranchFor(projectID string) string {
	v, ok := idx.currentBranchByProject.Load(projectID)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// detectGitBranch returns the symbolic branch name for the working
// tree at absPath, or a short commit SHA when HEAD is detached, or
// "" when the directory isn't a git working tree. Wraps `git
// rev-parse --abbrev-ref HEAD` with a 2s timeout — pincher's index
// path runs sync so we cap the worst case rather than block a
// re-index forever on a hung git process. #1303 Phase 2a.
//
// Reasoning for the fallback shape:
//   - Symbolic branch (`main`, `feat/foo`): primary case, what the
//     advisory compares against.
//   - Detached HEAD: `--abbrev-ref` returns the literal "HEAD" in this
//     case, which is useless. We re-run `git rev-parse --short HEAD`
//     to get a content-addressed identifier so the advisory at least
//     distinguishes "indexed at SHA abc1234" from "indexed at SHA
//     def5678" — the user can act on that even without a branch name.
//   - Not a git repo: both commands fail; return "". The advisory
//     short-circuits the comparison when either side is empty.
//
// Exec failures (git not on PATH, permission denied) all collapse to
// "" — pincher never refuses to index a non-git directory and never
// flips the value into a confusing pseudo-branch like "<no-git>".
func detectGitBranch(absPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "git", "-C", absPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	if branch != "" && branch != "HEAD" {
		return branch
	}
	// Detached HEAD — fall back to short SHA.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	sha, err := exec.CommandContext(ctx2, "git", "-C", absPath, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(sha))
}

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
				// #972: a binary swap (supervisor auto-restart) changes
				// idx.binaryVersion but no source file mtimes, so the
				// changedFiles path alone never triggers the
				// binaryDriftForce branch inside Index(). The new
				// binary keeps serving old-binary extraction quality
				// until the user happens to save a file. Force the
				// reindex when the binary stamped on the project
				// differs from the running binary.
				binaryDrifted := p.BinaryVersion != "" && idx.binaryVersion != "" && p.BinaryVersion != idx.binaryVersion
				if len(changed) == 0 && !binaryDrifted {
					continue
				}
				slog.Debug("pincher.watcher.reindex",
					"project", p.Name,
					"changed_files", len(changed),
					"binary_drifted", binaryDrifted)
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
				// #1496: synchronous, not goroutine. Pre-fix Watch
				// fanned out one goroutine per project per tick — on a
				// multi-project setup the post-schema-migration storm
				// grabbed every project's cross-process lockfile
				// simultaneously, blocking user `force=true` calls
				// against any project until the slowest reindex
				// finished. Running serially holds at most one
				// project's lockfile at a time, so a user
				// `force=true` against an idle project wins the race
				// immediately. The total wall-clock cost is unchanged
				// (SQLite is single-writer at the WAL anyway, so the
				// goroutines were not actually running in parallel —
				// they were queueing on the single writer connection;
				// the only thing parallelism bought was simultaneous
				// lockfile holding, which is the very thing the
				// #1474 repro flagged as user-hostile).
				func(p db.Project) {
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
	seen := map[string]bool{}
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
		rel, relErr := filepath.Rel(p.Path, path)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		seen[relSlash] = true
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		if info.ModTime().After(p.IndexedAt) {
			changed = append(changed, relSlash)
		}
		return nil
	})
	// #828: a pure deletion produces no file with a new mtime, so the
	// mtime scan above can't see it — the deleted file is just gone.
	// Compare the walked set against the files table; any indexed path
	// no longer on disk counts as a change so the watcher re-indexes
	// (which lets the #326 tail-GC prune the orphaned symbols, and
	// feeds invalidateReferencers for files with edges into it).
	if stored, err := idx.store.ListFilesForProject(p.ID); err == nil {
		for _, sp := range stored {
			if !seen[sp] {
				changed = append(changed, sp)
			}
		}
	}
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

	// #652 phase 1 + #685 phase 2: closure-fast-path. When
	// PINCHER_CLOSURE_TABLES=1 is set AND the project's closure table is
	// populated AND the caller uses the default edge-kind set, do a
	// single indexed SELECT instead of a recursive CTE. Closure
	// traversal is itself filtered to the default kind set at build
	// time (BuildClosure source-edge WHERE), so a non-default kinds
	// query still falls through to CTE — the gate enforces that.
	//
	// Phase-2 (#685) closed the v0.54 trade-off: closure rows now
	// record the last-hop edge kind in via_kind, so Via surfaces on
	// fast-path results identically to the CTE path. Pre-v30 closure
	// rows get via_kind='' which the trace UI renders as a hyphen —
	// rebuild via `pincher closure rebuild` or the next Index() pass
	// to backfill.
	useClosure := db.ClosureEnabled() && projectID != "" && isDefaultTraceKinds(edgeKinds)
	if useClosure {
		if n, _ := idx.store.ClosureRowCount(projectID); n > 0 {
			traceResults, err := idx.store.TraceViaClosure(projectID, symbolID, direction, maxDepth)
			if err == nil {
				return idx.materializeHops(projectID, traceResults, addRisk), nil
			}
			slog.Warn("pincher.trace.closure_fastpath.err",
				"project", projectID, "err", err, "fallback", "TraceViaCTEScoped")
		}
	}

	// Single CTE traversal per direction (max 2 SQL calls total for "both").
	// Project-scoped (#7): the recursive edge join restricts to the
	// caller's project so a same-named or same-IDed symbol in a sibling
	// project can't appear in the result set.
	traceResults, err := idx.store.TraceViaCTEScoped(projectID, symbolID, direction, edgeKinds, maxDepth)
	if err != nil {
		return nil, err
	}

	return idx.materializeHops(projectID, traceResults, addRisk), nil
}

// materializeHops resolves trace-result symbol IDs to full db.Symbol rows
// via GetSymbolScoped (project-scoped, #2 belt-and-braces against stale
// cross-project IDs) and tags each with a risk label when requested.
// Used by both the closure fast-path and the recursive-CTE fallback so
// the post-trace shape stays identical regardless of which path produced
// the IDs.
func (idx *Indexer) materializeHops(projectID string, traceResults []db.TraceResult, addRisk bool) []Hop {
	var hops []Hop
	for _, tr := range traceResults {
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
	return hops
}

// isDefaultTraceKinds reports whether the caller's edge-kind set matches
// the default set the closure builder uses. Closure rows don't store
// per-hop edge kind (phase 1 simplification), so we only take the
// fast-path when the caller is asking for those default kinds — any
// other set forces the CTE path which still respects the kind filter.
func isDefaultTraceKinds(kinds []string) bool {
	if len(kinds) != 3 {
		return false
	}
	want := map[string]bool{"CALLS": true, "HTTP_CALLS": true, "ASYNC_CALLS": true}
	for _, k := range kinds {
		if !want[k] {
			return false
		}
	}
	return true
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
	// #1475 v0.78: Claude Code's ephemeral git-worktree convention
	// puts active feature branches at `.claude/worktrees/<branch>/`.
	// Each worktree is a full sibling working tree on a different
	// branch — typically also registered as its own pincher project.
	// Without this entry, gocodewalker (which only honors
	// .gitignore + the ExcludeDirectory list, NOT the "skip dotted
	// dirs" rule isSkippedDir applies elsewhere) descends into the
	// worktree and counts those files under the parent project,
	// producing duplicate file_path rows across project IDs +
	// inflated symbol/edge counts. The principled fix (detect any
	// nested `.git` entry as a project boundary) needs gocodewalker
	// per-dir callback support; this name-based skip is the
	// pragmatic fix for the most common offender. `.claude/` also
	// holds agent settings/logs that don't belong in the code-intel
	// graph anyway.
	".claude": true,
}

func isSkippedDir(name string) bool {
	return skippedDirs[name] || strings.HasPrefix(name, ".")
}

// isPythonFile reports whether path is a Python source file. Used by
// resolveImports to decide whether to expand to_name via Python's
// source-root-aware candidate generator vs the literal lookup that
// works for Go/Rust/Java/etc.
func isPythonFile(path string) bool {
	return strings.HasSuffix(path, ".py") || strings.HasSuffix(path, ".pyw")
}

// isJSorTSFile reports whether path is a JavaScript or TypeScript
// source file. Used by the #1177 receiver-type resolver to decide
// whether to fall through to the class-name-lookup path (TS/JS have
// multi-segment module paths derived from file paths, so the Go-
// flavoured pkg-prefix construction in resolveByReceiverType can't
// reach the real Class.method QN).
func isJSorTSFile(path string) bool {
	return strings.HasSuffix(path, ".ts") ||
		strings.HasSuffix(path, ".tsx") ||
		strings.HasSuffix(path, ".js") ||
		strings.HasSuffix(path, ".jsx") ||
		strings.HasSuffix(path, ".mjs") ||
		strings.HasSuffix(path, ".cjs")
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
			if err := idx.store.RecordExtractionFailureWithBinary(projectID, relPath, lang, "extractor_panicked", details, idx.binaryVersion); err != nil {
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
	// #1319 v0.71: track which reasons this pass actually re-records, so
	// the post-pass prune can delete stale rows whose underlying failure
	// has been fixed since they were first observed. Pre-fix, a fix-the-
	// bug PR left historical rows in extraction_failures forever — the
	// user's repro: README.md `qualified_name_collision` row 8 days old
	// after #1207's Markdown suppression made that diagnostic stop
	// firing for Markdown entirely; no GC mechanism existed to remove it.
	keep := make(map[string]struct{}, 2)

	for _, s := range result.Symbols {
		// byte_range_negative
		//
		// #1203 v0.66 DOGFOOD: Module symbols emitted for legitimately
		// empty files (Python package-marker `__init__.py` with zero
		// bytes, empty Go files with just `package main`) land with
		// EndByte == StartByte == 0. That is correct — there's no body
		// to span — but the pre-fix invariant `EndByte <= StartByte`
		// flagged it as a failure, flooding extraction_failures with
		// low-signal noise. Other symbol kinds (Function / Method /
		// Class / etc.) with zero or negative span are still real bugs:
		// an extractor produced a symbol but couldn't anchor its
		// source. Carve out Module specifically.
		if s.EndByte < s.StartByte || (s.EndByte == s.StartByte && s.Kind != "Module") {
			details := fmt.Sprintf("symbol %q (%s): end_byte=%d <= start_byte=%d",
				s.QualifiedName, s.Kind, s.EndByte, s.StartByte)
			if err := idx.store.RecordExtractionFailureWithBinary(projectID, relPath, lang, "byte_range_negative", details, idx.binaryVersion); err != nil {
				slog.Warn("pincher.failure_record.err", "err", err, "file", relPath)
			}
			keep["byte_range_negative"] = struct{}{}
		}
	}
	// qualified_name_collision — record once per file even if multiple QNs
	// collide; details lists the worst offender plus the count. The
	// pre-disambiguation count is carried on FileResult.QNCollisions
	// (#115): the regex extractor produced duplicates, then
	// disambiguateDuplicates suffixed them with `~<line>` so all symbols
	// survive — but the underlying scope-blindness still merits the
	// diagnostic so reviewers see when a new collision pattern appears.
	//
	// #1207 v0.66 DOGFOOD: skip the diagnostic for Markdown specifically.
	// Sibling headings with identical text are a COMMON shape in real
	// documentation (reference docs with repeated subsection structure,
	// auto-generated index pages, tutorial scaffolds). The collision is
	// the goldmark walker correctly emitting one Section per H2/H3/etc.
	// — not the "regex got the scope wrong" symptom this diagnostic was
	// designed to surface in code. Disambiguation by line still kicks
	// in (`~<line>` suffix), so every Section survives. Skipping the
	// failure row keeps the diagnostic surface focused on the real
	// regex-scope blindness cases in code corpora.
	//
	// #1208 v0.66 DOGFOOD: skip for TypeScript (same UX rationale —
	// residual collisions are legitimate JSX/object-property/re-export
	// shapes the regex tier can't scope-resolve). When the TS AST
	// extractor lands (#1177 area), reconsider.
	//
	// v0.71 dogfood: TSX (separate language tag from TypeScript) was
	// falling through to the diagnostic — Codex-corpus TSX files like
	// `App.tsx`, `GraphExplorerGPU.tsx`, etc. produced 15+ collisions
	// per file (object-property `data` / `res` / `api` / `graph`
	// keys, repeated across handlers). Same UX rationale as TypeScript;
	// extend the carve-out to TSX.
	suppressCollision := lang == "Markdown" || lang == "TypeScript" || lang == "TSX"
	if !suppressCollision && len(result.QNCollisions) > 0 {
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
		if err := idx.store.RecordExtractionFailureWithBinary(projectID, relPath, lang, "qualified_name_collision", details, idx.binaryVersion); err != nil {
			slog.Warn("pincher.failure_record.err", "err", err, "file", relPath)
		}
		keep["qualified_name_collision"] = struct{}{}
	}

	// #1319 v0.71: prune extraction_failures rows whose reason no longer
	// applies. A reason that stops firing (because the underlying bug
	// was fixed, or because a per-language suppression was added — see
	// #1207/#1208) should not leave evidence in the table forever; doing
	// so pollutes `pincher doctor`'s failure count, the snapshot-gate
	// `extraction_failures_by_reason` invariant, and the bench/dashboard
	// failure metric. Pre-fix the only purge was per-project on project
	// delete; per-file rows accumulated indefinitely.
	if err := idx.store.PruneExtractionFailuresForFile(projectID, relPath, keep); err != nil {
		slog.Warn("pincher.failure_prune.err", "err", err, "file", relPath)
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
func (idx *Indexer) resolveImports(projectID string, pending []ast.ExtractedEdge, pythonRoots []string, qnMap map[string][]db.Symbol) int {
	if len(pending) == 0 {
		return 0
	}

	// #1338: optional pre-loaded QN→symbols map; nil falls back to
	// per-call DB queries. Same wrapper pattern as resolveCalls /
	// resolveReads.
	lookupSyms := func(qn string) []db.Symbol {
		if qnMap != nil {
			return qnMap[qn]
		}
		syms, err := idx.store.GetSymbolsByQN(projectID, qn)
		if err != nil {
			return nil
		}
		return syms
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
		syms := lookupSyms(qn)
		if len(syms) == 0 {
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

	// lookupPythonTarget is the to-side lookup for Python IMPORTS: the
	// same canonical-pick as `lookup`, but drops config/docs symbols
	// first. #860: `import os` — a stdlib import with no in-project
	// target — would otherwise false-bind to a same-named config-file
	// `Setting` whose QN is literally "os" (any top-level JSON/YAML/TOML
	// key extracts that way). An IMPORTS-edge target is always a code
	// symbol — Module, Class, Function, Method — never a Setting; this
	// is the same guard resolveCalls already applies via
	// excludeNonCodeSyms (#762/#790).
	codeCache := make(map[string]string)
	lookupPythonTarget := func(qn string) string {
		if id, ok := codeCache[qn]; ok {
			return id
		}
		syms := lookupSyms(qn)
		syms = excludeNonCodeSyms(syms)
		if len(syms) == 0 {
			codeCache[qn] = ""
			return ""
		}
		canonical := syms[0].ID
		for i := 1; i < len(syms); i++ {
			if syms[i].ID < canonical {
				canonical = syms[i].ID
			}
		}
		codeCache[qn] = canonical
		return canonical
	}

	// Python imports use dotted module paths ("zelosmcp.config"), but
	// pincher's QNs are file-path-derived ("src.zelosmcp.config" for a
	// src-layout repo). PythonImportCandidates expands toName with each
	// detected source-root prefix plus the identity fallback; lookupPython
	// returns the first hit. Non-Python edges keep the cheap single-lookup
	// path. See internal/ast/python_resolve.go.
	lookupPython := func(toName, fromFile string) string {
		for _, c := range ast.PythonImportCandidates(toName, fromFile, pythonRoots) {
			if id := lookupPythonTarget(c); id != "" {
				return id
			}
		}
		return ""
	}

	// Dedupe by (fromID, toID) — one pair of files can appear many times
	// when there are multiple Module rows per package.
	//
	// #1340 v0.71: when toID would otherwise be "" (the target name
	// doesn't resolve to any in-project symbol), synthesize a placeholder
	// external Module symbol and bind the edge to it. Pre-fix every
	// `import os`, `require('fs')`, `{% extends "base.html" %}`,
	// `[See](other.md)` IMPORTS edge silently dropped because the
	// resolver had nothing to bind toID against — the user's measurement
	// showed Python = 53 symbols / 0 IMPORTS, JS = 63 / 0, Jinja2 = 0,
	// while Go (in-project resolution) had 16. Option (a) from the
	// issue: synthesize lightweight Module symbols at file_path
	// "@external/<to_name>" so to_id always binds. These rows show up
	// in search by default — a known tradeoff per the issue, addressed
	// by a future opt-out filter PR.
	seen := make(map[string]bool)
	edges := make([]db.Edge, 0, len(pending))
	externals := make(map[string]db.Symbol)
	// #1613 v0.85 follow-up: per-reason drop counters. Mirrors the
	// resolveUsesVar pattern (#1500 for #1479) and gives the
	// per-resolver `pincher.imports.resolve.summary` slog line a
	// richer breakdown than the aggregate `dropped` the wrapper
	// reports. Without this split, "10k pending → 9k edges" looks
	// the same whether the 1k dropped because of FromQN misses, ToName
	// misses, or dedupe — but the recovery is different per shape
	// (extractor bug vs missing external vs noise floor).
	var droppedFromMissing, droppedToMissing, dedupedDuplicate, externalSynthesized int
	for _, e := range pending {
		fromID := lookup(e.FromQN)
		var toID string
		if isPythonFile(e.FromFile) {
			toID = lookupPython(e.ToName, e.FromFile)
		} else {
			toID = lookup(e.ToName)
		}
		if fromID == "" {
			// FromQN didn't resolve at all — almost always means the
			// source file produced no symbol the edge can hang on
			// (preamble-scoped Jinja IMPORTS / HCL module / Markdown
			// preamble-link). Drop, same as pre-#1340.
			droppedFromMissing++
			continue
		}
		if toID == "" && e.ToName != "" {
			ext := externalModuleSymbol(projectID, e.ToName)
			toID = ext.ID
			externals[toID] = ext
			externalSynthesized++
		}
		if toID == "" {
			droppedToMissing++
			continue
		}
		key := fromID + "\x00" + toID
		if seen[key] {
			dedupedDuplicate++
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

	// #1613 v0.85 follow-up: per-stage summary at slog.Info when
	// anything dropped — quiet on the all-resolved happy path so
	// healthy projects stay quiet. Mirrors `pincher.uses_var.resolve.summary`.
	if droppedFromMissing+droppedToMissing+dedupedDuplicate > 0 {
		slog.Info("pincher.imports.resolve.summary",
			"project_id", projectID,
			"pending_in", len(pending),
			"resolved_out", len(edges),
			"dropped_from_missing", droppedFromMissing,
			"dropped_to_missing", droppedToMissing,
			"deduped_duplicate", dedupedDuplicate,
			"external_synthesized", externalSynthesized,
		)
	}

	// #1340: persist the synthetic external Module symbols BEFORE the
	// edges that reference them — the FK semantics of the symbols table
	// don't enforce it, but downstream consumers that join symbols and
	// edges expect the symbol row to exist by the time the edge does.
	if len(externals) > 0 {
		extSyms := make([]db.Symbol, 0, len(externals))
		for _, s := range externals {
			extSyms = append(extSyms, s)
		}
		if err := idx.store.BulkUpsertSymbols(extSyms); err != nil {
			slog.Warn("pincher.imports.external_upsert.err", "err", err, "count", len(extSyms))
			// Drop the synthetic edges too — they'd dangle into a
			// non-existent symbol. The non-external edges still land.
			filtered := edges[:0]
			for _, edge := range edges {
				if _, isExt := externals[edge.ToID]; !isExt {
					filtered = append(filtered, edge)
				}
			}
			edges = filtered
		}
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

// resolveUsesVar binds Ansible USES_VAR pending edges (#1165) emitted
// by the YAML extractor (task / playbook string scalars) and the Jinja
// extractor ({{ var_name }} output expressions) to the canonical
// Setting symbol whose file lives in a recognised Ansible var-declaration
// path:
//
//   - group_vars/<name>.yml         (or group_vars/<name>/main.yml form)
//   - host_vars/<name>.yml          (or host_vars/<name>/main.yml form)
//   - roles/*/defaults/main.yml     (lowest precedence — role defaults)
//   - roles/*/vars/main.yml         (higher than defaults)
//   - vars/*.yml                    (playbook-level vars files)
//
// The resolver loads every Setting symbol in the project once, indexes
// them by `name`, and binds each pending edge against that index. When
// the same var name is declared in multiple var-files (common —
// `db_host` in group_vars/all.yml and an override in group_vars/staging.yml),
// the lexicographically smallest symbol ID is chosen as canonical
// (mirrors #428's pickCanonical for IMPORTS). The full Ansible 22-level
// precedence model is intentionally NOT implemented here — the audit
// queries this graph feeds (`dead_code`, "what uses var X?") answer
// correctly with the canonical-pick approximation, and modelling true
// host-context precedence would require per-play resolution well
// outside the scope of #1165 (filed as future work in the issue's
// Out-of-scope section).
//
// FromQN binds to the file's own Module symbol (the YAML/Jinja extractor
// emits FromQN = module). Per-file collisions on Module QN are rare in
// the Ansible context — task files and templates are filename-keyed —
// but the existing IMPORTS resolver pattern (canonical-pick by ID) is
// reused for consistency.
func (idx *Indexer) resolveUsesVar(projectID string, pending []ast.ExtractedEdge) int {
	if len(pending) == 0 {
		return 0
	}

	// One bulk load of all symbols, then bucket Settings by name and
	// Modules by QN. Pre-load avoids N round-trips for pending edges
	// that re-reference the same var name across many files.
	allByQN, err := idx.store.LoadAllSymbolsByQN(projectID)
	if err != nil {
		slog.Warn("pincher.uses_var.preload.err", "err", err)
		return 0
	}

	// Setting-by-name index, scoped to Ansible var-declaration paths.
	// Multiple Settings with the same name across var-files compete;
	// the lex-smallest ID wins as canonical (#428).
	//
	// From-side lookup tables: the YAML/Jinja extractors don't emit a
	// Module symbol per file (they only emit Settings / Functions /
	// Blocks). The pending-edge FromQN is the file's bare module name
	// ("main", "config", ...), which by itself doesn't disambiguate
	// across the project. Bucket every symbol by file_path so the
	// resolver can pick a representative — any symbol in the source
	// file — when no Module exists. The representative is the
	// lex-smallest symbol ID in the file, which is stable across
	// re-indexes the same way pickCanonical is.
	settingByName := make(map[string][]db.Symbol)
	moduleByQN := make(map[string][]db.Symbol)
	symsByFile := make(map[string][]db.Symbol)
	for _, syms := range allByQN {
		for _, s := range syms {
			symsByFile[s.FilePath] = append(symsByFile[s.FilePath], s)
			switch s.Kind {
			case "Setting":
				if isAnsibleVarDeclFile(s.FilePath) {
					settingByName[s.Name] = append(settingByName[s.Name], s)
				}
			case "Module":
				moduleByQN[s.QualifiedName] = append(moduleByQN[s.QualifiedName], s)
			}
		}
	}

	pickCanonical := func(syms []db.Symbol) string {
		if len(syms) == 0 {
			return ""
		}
		canonical := syms[0].ID
		for i := 1; i < len(syms); i++ {
			if syms[i].ID < canonical {
				canonical = syms[i].ID
			}
		}
		return canonical
	}

	// FromQN cache keeps repeated module lookups cheap. When the QN
	// doesn't bind to a Module (the YAML/Jinja extractors don't emit
	// one — see symsByFile godoc above), fall back to a representative
	// symbol from the FromFile so the edge still anchors at "some
	// symbol in this file". Last-ditch: a .j2 template that contains
	// only `{{ var }}` output expressions and no {% macro %} / {% block %} /
	// {% set %} produces ZERO symbols — synthesize a per-file Module
	// symbol so the USES_VAR edge has a stable anchor. The synthetic
	// rows are persisted in the same BulkUpsertSymbols call as the
	// externals in resolveImports (#1340), keeping all-or-nothing
	// semantics on the from-side.
	fromCache := make(map[string]string)
	synthFiles := make(map[string]db.Symbol)
	resolveFrom := func(qn, fromFile string) string {
		cacheKey := qn + "\x00" + fromFile
		if id, ok := fromCache[cacheKey]; ok {
			return id
		}
		id := pickCanonical(moduleByQN[qn])
		if id == "" && fromFile != "" {
			id = pickCanonical(symsByFile[fromFile])
		}
		if id == "" && fromFile != "" {
			synth := syntheticFileModuleSymbol(projectID, fromFile, qn)
			synthFiles[synth.ID] = synth
			id = synth.ID
		}
		fromCache[cacheKey] = id
		return id
	}
	// ToName cache: same `db_host` referenced from many places resolves
	// to one canonical ID — compute it once.
	toCache := make(map[string]string)
	resolveTo := func(name string) string {
		if id, ok := toCache[name]; ok {
			return id
		}
		id := pickCanonical(settingByName[name])
		toCache[name] = id
		return id
	}

	seen := make(map[string]bool)
	edges := make([]db.Edge, 0, len(pending))
	// #1479 diagnostic counters — surface why pending edges drop so the
	// user can distinguish "resolver bug" from "var-decl path not in the
	// canonical set" from "extractor emitted unbindable refs". Logged
	// once per resolve pass at the end, only when something dropped, so
	// healthy projects stay quiet.
	var droppedFromMissing, droppedToMissing, dedupedDuplicate int
	for _, e := range pending {
		fromID := resolveFrom(e.FromQN, e.FromFile)
		toID := resolveTo(e.ToName)
		if fromID == "" {
			droppedFromMissing++
			continue
		}
		if toID == "" {
			droppedToMissing++
			continue
		}
		key := fromID + "\x00" + toID
		if seen[key] {
			dedupedDuplicate++
			continue
		}
		seen[key] = true
		edges = append(edges, db.Edge{
			ProjectID:  projectID,
			FromID:     fromID,
			ToID:       toID,
			Kind:       "USES_VAR",
			Confidence: e.Confidence,
			Source:     "resolve_pass",
		})
	}

	// Persist any synthesized per-file Module symbols BEFORE the edges
	// that reference them — mirrors the #1340 external-module ordering
	// in resolveImports. Without this, an edge whose from_id is the
	// synthetic dangles into a non-existent symbol row.
	if len(synthFiles) > 0 {
		synthSyms := make([]db.Symbol, 0, len(synthFiles))
		for _, s := range synthFiles {
			synthSyms = append(synthSyms, s)
		}
		if err := idx.store.BulkUpsertSymbols(synthSyms); err != nil {
			slog.Warn("pincher.uses_var.synth_from_upsert.err", "err", err, "count", len(synthSyms))
			// Drop edges that pointed at synthetics so they don't dangle.
			filtered := edges[:0]
			for _, e := range edges {
				if _, isSynth := synthFiles[e.FromID]; !isSynth {
					filtered = append(filtered, e)
				}
			}
			edges = filtered
		}
	}

	// #1479 diagnostic: surface drop counts when ANY edges were
	// dropped so users debugging "USES_VAR is silently 0 in my DB" can
	// distinguish bug-modes. Logged at INFO so doctor / dashboards can
	// pick it up; counters are summed across the pass.
	dropped := droppedFromMissing + droppedToMissing
	if dropped > 0 || dedupedDuplicate > 0 {
		slog.Info("pincher.uses_var.resolve.summary",
			"project_id", projectID,
			"pending_in", len(pending),
			"resolved_out", len(edges),
			"dropped_from_missing", droppedFromMissing,
			"dropped_to_missing", droppedToMissing,
			"deduped_duplicate", dedupedDuplicate,
		)
	}

	// Atomic replace, same pattern as resolveImports (#475). Without
	// this, removing a Jinja/YAML var reference between re-index runs
	// would leave the prior edge behind forever.
	//
	// #1479 ordering note: the DELETE fires even when len(edges)==0,
	// which is intentional — if every reference was removed from the
	// source files between passes, the prior edges should disappear
	// too. The early-return at the top of resolveUsesVar handles the
	// "nothing was passed in" case before reaching this point, so a
	// resolveChanged=false watcher tick can't wipe the prior edges
	// (cf. indexer.go:878 gate). The user-visible #1479 symptom
	// ("USES_VAR shows in summary but absent from DB") would imply
	// either (a) every pending edge dropped to_missing because the
	// resolver couldn't find Setting symbols in canonical var-decl
	// paths, OR (b) the DELETE fires and INSERT silently fails. The
	// new pincher.uses_var.resolve.summary log + the BulkUpsertEdges
	// error path below pin both.
	if err := idx.store.DeleteEdgesByKindAndSource(projectID, "USES_VAR", "resolve_pass"); err != nil {
		slog.Warn("pincher.uses_var.delete_prior.err", "err", err)
	}
	if len(edges) == 0 {
		return 0
	}
	if err := idx.store.BulkUpsertEdges(edges); err != nil {
		slog.Warn("pincher.uses_var.upsert.err", "err", err, "edge_count", len(edges))
		return 0
	}
	return len(edges)
}

// syntheticFileModuleSymbol creates a Module symbol that represents the
// source file when the file produced no extractable symbols of its own
// (common for .j2 templates that contain only `{{ var }}` outputs).
// The synthetic anchors USES_VAR edges (#1165) so audit queries like
// "what uses var X?" can list the referencing file even when there's no
// Function/Block/Setting in it.
//
// The Module symbol uses the file's basename (minus extension) as both
// Name and QualifiedName, matching jinjaModuleName / YAML extractor
// conventions. Confidence 0.5 — half the regex-tier baseline — flags
// the row as resolver-synthesized vs extractor-emitted; downstream
// consumers that want extractor-only symbols can filter on that floor.
func syntheticFileModuleSymbol(projectID, filePath, qnHint string) db.Symbol {
	qn := qnHint
	if qn == "" {
		qn = filepath.Base(filePath)
		if i := strings.LastIndex(qn, "."); i > 0 {
			qn = qn[:i]
		}
	}
	return db.Symbol{
		ID:                   db.MakeSymbolID(filePath, qn, "Module"),
		ProjectID:            projectID,
		FilePath:             filePath,
		Name:                 qn,
		QualifiedName:        qn,
		Kind:                 "Module",
		Language:             ast.DetectLanguage(filePath),
		IsExported:           true,
		ExtractionConfidence: 0.5,
	}
}

// isAnsibleVarDeclFile returns true when relPath looks like an
// Ansible variable declaration file. Conservative — only recognised
// canonical paths qualify, so generic YAML config can't be
// mistaken for a var-decl source. The set:
//
//   - group_vars/<name>.yml      (file form)
//   - group_vars/<name>/*.yml    (directory form)
//   - host_vars/<name>.yml       (file form)
//   - host_vars/<name>/*.yml     (directory form)
//   - roles/<role>/defaults/main.yml
//   - roles/<role>/vars/main.yml
//   - vars/<name>.yml            (playbook-adjacent vars/)
//
// Plays can declare vars inline via `vars:` blocks — those Settings
// have file_paths matching the parent playbook (`site.yml` etc.) and
// are intentionally NOT recognised here. The audit-shaped queries this
// graph feeds care about cross-file binding; inline play vars don't
// produce graph value because the use and decl already share a file.
func isAnsibleVarDeclFile(relPath string) bool {
	clean := strings.ReplaceAll(relPath, "\\", "/")
	if !strings.HasSuffix(clean, ".yml") && !strings.HasSuffix(clean, ".yaml") {
		return false
	}
	if strings.HasPrefix(clean, "group_vars/") || strings.Contains(clean, "/group_vars/") {
		return true
	}
	if strings.HasPrefix(clean, "host_vars/") || strings.Contains(clean, "/host_vars/") {
		return true
	}
	if strings.HasPrefix(clean, "vars/") || strings.Contains(clean, "/vars/") {
		// `roles/<r>/vars/main.yml` also matches this branch via
		// `/vars/`, which is intentional.
		return true
	}
	if strings.Contains(clean, "/defaults/") {
		// `roles/<r>/defaults/main.yml`
		return true
	}
	return false
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
			BaseType:     r.BaseType,
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

// fileOfID returns the FilePath of the symbol with the given ID
// from a candidate slice. Used by #566 sibling detection — the
// canonical pickCanonical result loses file-path context, so we
// recover it via a linear scan over the same slice. Empty string
// if the ID isn't in the slice (shouldn't happen — pickCanonical
// always picks from syms).
func fileOfID(syms []db.Symbol, id string) string {
	for i := range syms {
		if syms[i].ID == id {
			return syms[i].FilePath
		}
	}
	return ""
}

// isBuildTagSibling reports whether two file paths look like
// build-tag conditional-compilation siblings (#566): same directory,
// same base-name stem (everything before a recognized platform/arch
// suffix), differing only by the platform/arch part.
//
// Examples that ARE siblings:
//   - web_windows.go ↔ web_unix.go
//   - syscall_amd64.go ↔ syscall_arm64.go
//   - web_windows.go ↔ web_linux.go
//
// NOT siblings:
//   - web.go ↔ web_test.go (test files are intentionally separate)
//   - web_windows.go ↔ other.go (different stem)
//   - web_windows.go ↔ web_windows.go (same file)
//
// Doesn't parse `//go:build` constraints; the cheap heuristic is
// filename-based per Go's `_GOOS.go` / `_GOARCH.go` naming
// convention. The `//go:build` parsing path is filed as a v0.21+
// follow-up.
func isBuildTagSibling(a, b string) bool {
	if a == "" || b == "" || a == b {
		return false
	}
	// Same directory required.
	aDir, aFile := splitFile(a)
	bDir, bFile := splitFile(b)
	if aDir != bDir {
		return false
	}
	aStem := stripPlatformSuffix(aFile)
	bStem := stripPlatformSuffix(bFile)
	return aStem != "" && aStem == bStem
}

// splitFile splits a path into (directory, basename) without
// pulling in path/filepath (its slash semantics differ between
// OSes; pincher always stores forward-slash relative paths).
func splitFile(p string) (string, string) {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i], p[i+1:]
		}
	}
	return "", p
}

// stripPlatformSuffix returns the stem of a Go source file with
// recognized platform/arch suffix removed. Returns "" if the file
// doesn't end in `.go` or has no recognized suffix — those don't
// participate in build-tag sibling detection.
//
// Recognized GOOS values: every one Go supports as of 1.22.
// Recognized GOARCH values: same.
func stripPlatformSuffix(name string) string {
	const ext = ".go"
	if !strings.HasSuffix(name, ext) {
		return ""
	}
	stem := name[:len(name)-len(ext)]
	suffixes := []string{
		// GOOS
		"_aix", "_android", "_darwin", "_dragonfly", "_freebsd", "_hurd",
		"_illumos", "_ios", "_js", "_linux", "_nacl", "_netbsd", "_openbsd",
		"_plan9", "_solaris", "_unix", "_wasip1", "_windows", "_zos",
		// GOARCH
		"_386", "_amd64", "_amd64p32", "_arm", "_arm64", "_armbe", "_arm64be",
		"_loong64", "_mips", "_mips64", "_mips64le", "_mips64p32", "_mips64p32le",
		"_mipsle", "_ppc", "_ppc64", "_ppc64le", "_riscv", "_riscv64",
		"_s390", "_s390x", "_sparc", "_sparc64", "_wasm",
	}
	for _, s := range suffixes {
		if strings.HasSuffix(stem, s) {
			return strings.TrimSuffix(stem, s)
		}
	}
	return ""
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
//
// #1158 (v0.61): retained as the Go-specific entry point. The Go
// resolver call sites at indexer.go:2278 / :2687 continue to call
// this name. New per-language equivalents live in
// `polymorphicMethodNamesByLanguage` — `isPolymorphicMethodName(name,
// lang)` dispatches to whichever language's set applies. This keeps
// the v0.62/v0.63 TS/Python/Rust resolvers ready to consume the same
// pattern without re-litigating the Go-specific list.
func isPolymorphicInterfaceMethodName(name string) bool {
	_, ok := polymorphicMethodNamesByLanguage["Go"][name]
	return ok
}

// isPolymorphicMethodName is the language-aware entry point added in
// #1158 (v0.61). The Go resolver continues to call
// isPolymorphicInterfaceMethodName for backward-compatibility shape;
// future TS/Python/Rust resolvers pass their own language tag and pick
// up the matching set. Unknown languages return false (no filtering),
// matching pre-v0.61 behavior for any path that didn't already opt in.
func isPolymorphicMethodName(name, language string) bool {
	if names, ok := polymorphicMethodNamesByLanguage[language]; ok {
		_, hit := names[name]
		return hit
	}
	return false
}

// polymorphicMethodNamesByLanguage maps language → method names that
// resolve to stdlib/runtime interfaces in that language. The
// per-language entries reflect the typical false-positive surface a
// receiver-type-less name-fallback resolver hits when the project
// happens to define a Method whose name collides with a polymorphic
// stdlib API.
//
// Adding a language: enumerate the names that appear on widely-used
// stdlib types whose name (without the import scope) would collide
// with an in-project Method definition. Conservative inclusion —
// only well-known interface methods, NOT every imaginable method
// name. Each entry must have a real-world false-positive case
// motivating it; otherwise legitimate calls get filtered out.
//
// Pre-v0.61 the Go set was hardcoded in isPolymorphicInterfaceMethodName.
// v0.61 (#1158) generalized to this map so TS/Python receivers can
// share the same shape when their resolvers land.
var polymorphicMethodNamesByLanguage = map[string]map[string]struct{}{
	"Go": {
		// fmt.Stringer / error
		"String": {}, "Error": {}, "GoString": {},
		// io interfaces
		"Read": {}, "Write": {}, "Close": {}, "Seek": {},
		"ReadAt": {}, "WriteAt": {},
		"ReadFrom": {}, "WriteTo": {},
		"ReadByte": {}, "WriteByte": {},
		"ReadString": {}, "WriteString": {},
		"ReadRune": {}, "WriteRune": {},
		"UnreadByte": {}, "UnreadRune": {},
		// sync.Locker family
		"Lock": {}, "Unlock": {}, "RLock": {}, "RUnlock": {}, "TryLock": {},
		// sort.Interface
		"Len": {}, "Less": {}, "Swap": {},
		// http.Handler / net interfaces
		"ServeHTTP": {},
		// encoding interfaces — common marshalers
		"MarshalJSON": {}, "UnmarshalJSON": {},
		"MarshalText": {}, "UnmarshalText": {},
		"MarshalBinary": {}, "UnmarshalBinary": {},
		"MarshalYAML": {}, "UnmarshalYAML": {},
		// fmt.Formatter / fmt.Scanner
		"Format": {}, "Scan": {},
		// time.Time methods that pair with common local-var names
		"Now": {}, "Add": {}, "Sub": {}, "Before": {}, "After": {}, "Equal": {},
		// context.Context
		"Deadline": {}, "Done": {}, "Err": {}, "Value": {},
		// errors.Is / errors.As / errors.Unwrap
		"Is": {}, "As": {}, "Unwrap": {},
		// #567: *exec.Cmd / *http.Server / *cron.Cron / pool workers
		"Run": {},
	},
	// TypeScript — built-in types + DOM + Promise + Iterator
	// + the typical "every class overrides this" methods. Resolver
	// will consume this when the v0.62+ TS receiver-type binding
	// piece lands. Conservatively scoped to names that have real-
	// world false-positive impact on TS codebases mirroring the Go
	// patterns: toString collides with Object.prototype.toString on
	// every TS Class, hasOwnProperty with Object.prototype, etc.
	"TypeScript": {
		// Object.prototype universal methods — every TS class
		// effectively defines these.
		"toString": {}, "valueOf": {}, "hasOwnProperty": {},
		"isPrototypeOf": {}, "propertyIsEnumerable": {},
		"toLocaleString": {},
		// Promise / thenable
		"then": {}, "catch": {}, "finally": {},
		// Iterator / async iterator
		"next": {}, "return": {}, "throw": {},
		// Common Map/Set/Array members aliased on user types
		"size": {}, "length": {},
		"clear": {}, "delete": {}, "has": {}, "get": {}, "set": {},
		"add": {}, "remove": {},
		"forEach": {}, "map": {}, "filter": {}, "reduce": {},
		"push": {}, "pop": {}, "shift": {}, "unshift": {},
		// Error.prototype
		"message": {}, "name": {}, "stack": {},
		// DOM EventTarget / EventEmitter
		"addEventListener": {}, "removeEventListener": {},
		"dispatchEvent": {}, "emit": {}, "on": {}, "off": {},
		"once": {},
		// Lifecycle (React / Angular / Vue / web-components)
		"render": {}, "constructor": {}, "destroy": {},
		"connectedCallback": {}, "disconnectedCallback": {},
		"attributeChangedCallback": {},
	},
	// Python — dunder methods + common protocol methods. Same
	// shape: when a user Class defines __str__ etc., every print(x)
	// call would otherwise false-bind to that single user Method.
	// Resolver will consume this when the v0.62 Python AST resolver
	// gains cross-file binding.
	"Python": {
		// Universal dunders
		"__init__": {}, "__del__": {}, "__repr__": {}, "__str__": {},
		"__bytes__": {}, "__format__": {}, "__hash__": {},
		"__bool__": {}, "__dir__": {},
		// Comparison
		"__lt__": {}, "__le__": {}, "__eq__": {}, "__ne__": {},
		"__gt__": {}, "__ge__": {},
		// Attribute access
		"__getattr__": {}, "__getattribute__": {}, "__setattr__": {},
		"__delattr__": {},
		// Container
		"__len__": {}, "__getitem__": {}, "__setitem__": {},
		"__delitem__": {}, "__contains__": {}, "__iter__": {},
		"__next__": {}, "__reversed__": {},
		// Callable
		"__call__": {},
		// Context manager
		"__enter__": {}, "__exit__": {},
		// Async
		"__aenter__": {}, "__aexit__": {},
		"__aiter__": {}, "__anext__": {},
		"__await__": {},
		// Numeric protocol — common subset
		"__add__": {}, "__sub__": {}, "__mul__": {}, "__truediv__": {},
		"__floordiv__": {}, "__mod__": {}, "__pow__": {},
		"__neg__": {}, "__pos__": {}, "__abs__": {},
		// Common protocol methods on built-ins
		"close": {}, "read": {}, "write": {}, "flush": {},
		"send": {}, "recv": {},
	},
}

// isFixturePath reports whether filePath is inside an isolated test-
// fixture corpus (testdata/, __fixtures__/, etc.). #750: name-fallback
// call/binding resolution must not pick a fixture symbol as the target
// of an edge from real project code — testdata corpora are isolated
// mini-projects, not real call targets, so binding `Open()` to a
// `testdata/corpus/.../auth.Open` fixture is always a false positive.
// Mirror of server.isTestFixturePath (the index package can't import
// server). Note: when a corpus is indexed as its OWN project root
// (the pinned-corpus snapshot path), file paths are relative to the
// corpus dir and this returns false — so snapshots are unaffected.
func isFixturePath(filePath string) bool {
	low := strings.ToLower(strings.ReplaceAll(filePath, `\`, `/`))
	for _, dir := range []string{"/testdata/", "/test-fixtures/", "/test_fixtures/", "/__fixtures__/", "/fixtures/"} {
		if strings.Contains(low, dir) {
			return true
		}
	}
	for _, prefix := range []string{"testdata/", "test-fixtures/", "test_fixtures/", "__fixtures__/", "fixtures/"} {
		if strings.HasPrefix(low, prefix) {
			return true
		}
	}
	return false
}

// preferNonFixtureSyms returns syms with fixture-path entries removed,
// unless that would empty the set (in which case the original is
// returned so intra-fixture resolution still works). #750.
func preferNonFixtureSyms(syms []db.Symbol) []db.Symbol {
	out := make([]db.Symbol, 0, len(syms))
	for _, s := range syms {
		if !isFixturePath(s.FilePath) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return syms
	}
	return out
}

// excludeMethodSyms drops Method-kind symbols. #754: the bare-name
// resolution fallbacks (resolveCalls's lookupName, resolveReads's
// lookupNameInLang) are only reached for unqualified identifiers like
// `Foo()` — and a bare call/reference in Go is syntactically never a
// method invocation (methods always require a receiver). When a
// project has both `func Extract(...)` and several `(x).Extract(...)`
// methods, picking the lex-smallest ID over the mixed set funnelled
// every bare `Extract()` call onto `bashExtractor.Extract` and left
// the real package-level `ast.Extract` with zero inbound edges. This
// filter CAN empty the set — when it does, the bare call has no valid
// Function/Variable target and should resolve to "" (unresolved is
// correct; a false bind to an arbitrary same-named method is not).
func excludeMethodSyms(syms []db.Symbol) []db.Symbol {
	out := make([]db.Symbol, 0, len(syms))
	for _, s := range syms {
		if s.Kind != "Method" {
			out = append(out, s)
		}
	}
	return out
}

// excludeNonCodeSyms drops config/docs symbols (Setting, Section,
// Document, Resource, Output, Local, Provider, Block, DataSource) from a
// candidate set. #762: a CALLS-edge participant is always a code symbol
// — a bare call name (`build`, `test`, `run`) can collide with a
// config-file key whose QN is literally that string (npm script keys,
// top-level YAML/JSON keys all extract as Setting with QN = the key
// name). Without this filter resolveCalls's lookupQN/lookupName resolve
// a Go call to the Setting. Like excludeMethodSyms this CAN empty the
// set; when it does, the QN lookup correctly returns "" so the bare-name
// fallback runs (and is itself filtered) — a false bind to a JSON
// Setting is never the right answer for a CALLS edge.
func excludeNonCodeSyms(syms []db.Symbol) []db.Symbol {
	out := make([]db.Symbol, 0, len(syms))
	for _, s := range syms {
		switch s.Kind {
		case "Setting", "Section", "Document", "Resource",
			"Output", "Local", "Provider", "Block", "DataSource":
			// non-code — skip
		default:
			out = append(out, s)
		}
	}
	return out
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
	// #750: drop fixture-corpus symbols before counting — a real
	// receiver-method call must not bind into testdata/.
	syms = preferNonFixtureSyms(syms)
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
func (idx *Indexer) resolveCalls(projectID string, pending []ast.ExtractedEdge, pythonRoots []string, qnMap map[string][]db.Symbol) int {
	if len(pending) == 0 {
		return 0
	}

	// #1338: optional pre-loaded QN→symbols map; nil falls back to
	// per-call DB queries. Wrapper closure routes either way.
	lookupSyms := func(qn string) []db.Symbol {
		if qnMap != nil {
			return qnMap[qn]
		}
		syms, err := idx.store.GetSymbolsByQN(projectID, qn)
		if err != nil {
			return nil
		}
		return syms
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
	// #566: when N symbols share a QN and only differ by build-tag
	// platform suffix (`web_windows.go` / `web_unix.go`), the
	// canonical edge goes to one of them and the others false-positive
	// in dead_code as "no inbound CALLS." Track the sibling IDs so the
	// main loop can fan out an extra edge to each. Cache parallel to
	// qnCache; same key.
	qnSiblings := make(map[string][]string)
	siblingsForCanonical := func(syms []db.Symbol, canonical string) []string {
		out := make([]string, 0)
		for i := range syms {
			if syms[i].ID == canonical {
				continue
			}
			if isBuildTagSibling(syms[i].FilePath, fileOfID(syms, canonical)) {
				out = append(out, syms[i].ID)
			}
		}
		return out
	}
	qnCache := make(map[string]string)
	lookupQN := func(qn string) string {
		if qn == "" {
			return ""
		}
		if id, ok := qnCache[qn]; ok {
			return id
		}
		syms := lookupSyms(qn)
		if len(syms) == 0 {
			qnCache[qn] = ""
			return ""
		}
		// #762: a CALLS participant is always a code symbol — drop
		// config/doc candidates so a bare Go call name colliding with a
		// config-file key QN doesn't resolve the call to a Setting. If
		// that empties the set, return "" so the bare-name fallback runs.
		syms = excludeNonCodeSyms(syms)
		if len(syms) == 0 {
			qnCache[qn] = ""
			return ""
		}
		canonical := pickCanonical(syms)
		qnCache[qn] = canonical
		// #566: cache build-tag siblings for the fan-out emission below.
		if len(syms) > 1 {
			if sibs := siblingsForCanonical(syms, canonical); len(sibs) > 0 {
				qnSiblings[qn] = sibs
			}
		}
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
		syms := lookupSyms(qn)
		if len(syms) == 0 {
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
		// #750: a bare-name call from real project code must not bind
		// into an isolated testdata fixture. Drop fixture candidates
		// before picking the canonical target (and its build-tag
		// siblings) — keeps them only if every candidate is a fixture.
		syms = preferNonFixtureSyms(syms)
		// #754: a bare `Foo()` call is never a method invocation —
		// drop Method-kind candidates so an ambiguous name shared by a
		// package function and same-named methods resolves to the
		// function, not the lex-smallest method.
		syms = excludeMethodSyms(syms)
		// #762: a bare call name can also collide with a config-file key
		// (`build`, `test`) — drop non-code candidates so the name
		// fallback can't resolve a Go call to a Setting/Section.
		syms = excludeNonCodeSyms(syms)
		if len(syms) == 0 {
			nameCache[name] = ""
			return ""
		}
		canonical := pickCanonical(syms)
		nameCache[name] = canonical
		// #566: same fan-out cache as lookupQN, keyed by the bare
		// name. The main loop reads qnSiblings[e.ToName] regardless
		// of which lookup path resolved the canonical — bare-name
		// calls (intra-package `Foo()`) take the lookupName route
		// and absolutely need this caching path.
		if len(syms) > 1 {
			if sibs := siblingsForCanonical(syms, canonical); len(sibs) > 0 {
				qnSiblings[name] = sibs
			}
		}
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
		// #758: segments[0] is the receiver expression as written. If it
		// looks like a Go stdlib package (`strings`, `bytes`, `time`, ...)
		// this is a package-function call, not a receiver-method call —
		// binding it by the *enclosing* method's receiver type produces a
		// false edge (e.g. `strings.Index(...)` inside
		// `*Indexer.resolveCalls` false-bound to `*Indexer.Index`). The
		// same stoplist guards the #410 receiver-method fallback below.
		// The complete fix threads the receiver *variable name* through
		// ExtractedEdge (a schema migration) so segments[0] can be checked
		// against the actual receiver var rather than a stoplist.
		if len(segments) > 0 && isStdlibReceiver(segments[0]) {
			return ""
		}
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

	// lookupPythonCall expands a Python call's to_name through every
	// source-root candidate, returning the first hit. The extractor has
	// already alias-rewritten imported names and self.X → class.X, so
	// what we get here is either a bare local name, a dotted path that
	// might need a src-prefix, or a relative-import path (rare for calls).
	lookupPythonCall := func(toName, fromFile string) string {
		for _, c := range ast.PythonImportCandidates(toName, fromFile, pythonRoots) {
			if id := lookupQN(c); id != "" {
				return id
			}
		}
		return ""
	}

	// #1177 v0.72: TS/JS receiver-type binding via class-name lookup.
	// Go's resolveByReceiverType assumes single-segment package paths
	// (`pkg.Type.method` QN shape) — TS/JS have multi-segment module
	// paths derived from file paths, so the pkg-prefix construction
	// can't reach the real Class.method QN. Instead, look up the
	// receiver Class by name to get its full QN, then append the
	// method name. Cached per receiverType because GetSymbolsByName
	// is a DB round-trip and a hot trace will hit the same type many
	// times.
	classQNCache := map[string]string{}
	classQNByName := func(name string) string {
		if name == "" {
			return ""
		}
		if qn, ok := classQNCache[name]; ok {
			return qn
		}
		syms, err := idx.store.GetSymbolsByName(projectID, name, 50)
		if err != nil || len(syms) == 0 {
			classQNCache[name] = ""
			return ""
		}
		for _, s := range syms {
			if s.Kind == "Class" || s.Kind == "Interface" {
				classQNCache[name] = s.QualifiedName
				return s.QualifiedName
			}
		}
		classQNCache[name] = ""
		return ""
	}
	// resolveByReceiverTypeNonGo handles the JS/TS shape:
	//   ToName="this.X" or "recv.X" + ReceiverType="<ClassName>"
	// Looks up the Class symbol by name, then constructs
	// <ClassQN>.<methodName> and tries lookupQN. Single-segment
	// chains only — multi-segment (`a.b.X`) needs class-field
	// tracking which isn't in this PR's scope.
	resolveByReceiverTypeNonGo := func(toName, receiverType string) string {
		if receiverType == "" || toName == "" {
			return ""
		}
		segments := strings.Split(toName, ".")
		if len(segments) != 2 {
			return ""
		}
		methodName := segments[1]
		if methodName == "" {
			return ""
		}
		classQN := classQNByName(receiverType)
		if classQN == "" {
			return ""
		}
		return lookupQN(classQN + "." + methodName)
	}

	seen := make(map[string]bool)
	edges := make([]db.Edge, 0, len(pending))
	// #1613 v0.85 follow-up: per-reason drop counters. Mirror the
	// resolveUsesVar pattern (#1500). Splits the aggregate
	// `dropped = pending_in - resolved_out` the wrapper reports into
	// the actual signal — from_missing (extractor produced an unbindable
	// FromQN), to_missing (no in-project target found after every
	// fallback path), self_edge (fromID == toID, e.g., recursive call
	// pre-dedupe), dedupe (same (from,to) pair already counted).
	var droppedFromMissing, droppedToMissing, droppedSelfEdge, dedupedDuplicate int
	for _, e := range pending {
		fromID := lookupFromQN(e.FromQN, e.FromFile)
		if fromID == "" && !strings.Contains(e.FromQN, ".") {
			fromID = lookupName(e.FromQN)
		}
		if fromID == "" {
			droppedFromMissing++
			continue
		}
		var toID string
		if isPythonFile(e.FromFile) {
			toID = lookupPythonCall(e.ToName, e.FromFile)
			if toID == "" && !strings.Contains(e.ToName, ".") {
				toID = lookupName(e.ToName)
			}
		} else {
			toID = lookupQN(e.ToName)
			if toID == "" && !strings.Contains(e.ToName, ".") {
				toID = lookupName(e.ToName)
			}
		}
		// #423 piece 3: precise receiver-type binding before the
		// looser project-wide receiver-method fallback.
		if toID == "" {
			toID = resolveByReceiverType(e.ToName, e.ReceiverType, e.FromQN)
		}
		// #1177 v0.72: TS/JS receiver-type fallback for files where
		// the Go pkg-prefix path can't reach the real Class.method
		// QN. Runs after the Go path so Go calls still get the
		// struct-field-aware behaviour first.
		if toID == "" && e.ReceiverType != "" && isJSorTSFile(e.FromFile) {
			toID = resolveByReceiverTypeNonGo(e.ToName, e.ReceiverType)
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
		if toID == "" {
			droppedToMissing++
			continue
		}
		if fromID == toID {
			droppedSelfEdge++
			continue
		}
		key := fromID + "\x00" + toID
		if seen[key] {
			dedupedDuplicate++
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
		// #566: fan out to build-tag siblings of the canonical target.
		// `web.go::launchWeb` calling `platformPIDAlive` resolves to ONE
		// of {web_unix.go, web_windows.go} depending on lex-order;
		// without this fan-out the other variant looks dead despite
		// being the implementation built on a different platform. We
		// emit a copy of the edge to each sibling so dead_code stops
		// false-positiving the not-currently-built variant.
		for _, sib := range qnSiblings[e.ToName] {
			if sib == fromID {
				continue
			}
			sibKey := fromID + "\x00" + sib
			if seen[sibKey] {
				continue
			}
			seen[sibKey] = true
			edges = append(edges, db.Edge{
				ProjectID:  projectID,
				FromID:     fromID,
				ToID:       sib,
				Kind:       "CALLS",
				// Same confidence as the canonical edge — both are
				// equally "real" implementations of the same call.
				Confidence: e.Confidence,
				Source:     "resolve_pass",
			})
		}
	}

	// #1613 v0.85 follow-up: per-stage summary mirrors the
	// pincher.uses_var.resolve.summary shape. Only emits when
	// something dropped — quiet on the all-resolved happy path.
	if droppedFromMissing+droppedToMissing+droppedSelfEdge+dedupedDuplicate > 0 {
		slog.Info("pincher.calls.resolve.summary",
			"project_id", projectID,
			"pending_in", len(pending),
			"resolved_out", len(edges),
			"dropped_from_missing", droppedFromMissing,
			"dropped_to_missing", droppedToMissing,
			"dropped_self_edge", droppedSelfEdge,
			"deduped_duplicate", dedupedDuplicate,
		)
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
func (idx *Indexer) resolveReads(projectID string, pending []ast.ExtractedEdge, qnMap map[string][]db.Symbol) int {
	if len(pending) == 0 {
		return 0
	}

	// #1338: optional pre-loaded QN→symbols map; nil falls back to
	// per-call DB queries. Mirrors the resolveCalls pattern.
	lookupSyms := func(qn string) []db.Symbol {
		if qnMap != nil {
			return qnMap[qn]
		}
		syms, err := idx.store.GetSymbolsByQN(projectID, qn)
		if err != nil {
			return nil
		}
		return syms
	}

	// #760: struct-field index, structQN → set of field names. Used by
	// the binding pass to suppress the false CALLS edge from a struct-
	// field read (`e.Confidence`) to a same-named project Method. The
	// extractor stamps ExtractedEdge.BaseType with the base's declared
	// type; if that type names a project struct with a field of the
	// READS edge's ToName, the read is a field access — never a
	// function-value reference — so no binding edge is emitted. On a
	// load failure we leave the map empty: the binding pass then falls
	// back to its pre-#760 heuristic rather than dropping silently.
	fieldsByStructQN := map[string]map[string]bool{}
	if rows, err := idx.store.LoadStructFields(projectID); err == nil {
		for _, f := range rows {
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
				m = map[string]bool{}
				fieldsByStructQN[structQN] = m
			}
			m[f.FieldName] = true
		}
	} else {
		slog.Warn("pincher.struct_fields.load.err", "err", err)
	}
	// isStructFieldRead reports whether a READS edge's base type names a
	// project struct that has a field called fieldName — i.e. the read
	// is `someStruct.fieldName`, a field access, not a method value.
	// baseType is the type as written (e.g. "ast.ExtractedEdge" or, for
	// a same-package type, the bare "ExtractedEdge"); fromQN supplies
	// the caller's package so the bare form can be qualified.
	isStructFieldRead := func(baseType, fromQN, fieldName string) bool {
		if baseType == "" || fieldName == "" {
			return false
		}
		if m := fieldsByStructQN[baseType]; m[fieldName] {
			return true
		}
		// Same-package struct: the extractor wrote the type unqualified
		// ("ExtractedEdge"); the struct's QN is "<pkg>.ExtractedEdge".
		if !strings.Contains(baseType, ".") {
			pkg := fromQN
			if i := strings.Index(pkg, "."); i > 0 {
				pkg = pkg[:i]
			}
			if pkg != "" {
				if m := fieldsByStructQN[pkg+"."+baseType]; m[fieldName] {
					return true
				}
			}
		}
		return false
	}

	// QN cache: maps the qualified name to (id, language, kind-class).
	// language is needed for #436 — same-language scoping prevents Go
	// references to common identifiers (`path`, `result`, `fs`) from
	// binding to JS / Python locals that happen to share the name.
	// isFunc is added for #565 — a READS-pass identifier that resolves
	// to a Function/Method (not a Variable) is a function-value binding
	// (e.g. `s.spawnFn = s.defaultSpawn`, `T{Handler: someFn}`); we
	// emit a CALLS edge for it instead of dropping, so dead_code stops
	// flagging the bound function as unreachable.
	type lookup struct {
		id     string
		lang   string
		isVar  bool
		isFunc bool
	}
	qnCache := make(map[string]lookup)
	lookupQN := func(qn string) lookup {
		if qn == "" {
			return lookup{}
		}
		if v, ok := qnCache[qn]; ok {
			return v
		}
		syms := lookupSyms(qn)
		if len(syms) == 0 {
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
		k := syms[canonical].Kind
		v := lookup{
			id:     syms[canonical].ID,
			lang:   syms[canonical].Language,
			isVar:  k == "Variable",
			isFunc: k == "Function" || k == "Method",
		}
		qnCache[qn] = v
		return v
	}

	// Name lookups are scoped to the source language AND, for the
	// to-side, the source Go package. Cache key includes both so callers
	// asking for the same name from different languages / packages don't
	// collide on the first one in.
	//
	// #764: pkgDir scopes the bare-name fallback to the reader's Go
	// package. A bare unqualified Go read is *always* same-package — a
	// cross-package read is written `pkg.Name` (a selector, now emitted
	// qualified by extractGoReads). Without this scope, every bare
	// `version` / `result` / `name` read funnels onto the single
	// project-wide Go symbol of that name (`main.version` collected 37
	// inbound READS edges, ~30 false, pre-fix). Package identity is
	// approximated by source-file directory: same dir == same Go
	// package. Empty pkgDir disables scoping (used for from-side
	// discovery, where the package isn't known yet).
	type nameKey struct{ name, lang, pkgDir string }
	nameCache := make(map[nameKey]lookup)
	lookupNameInLang := func(name, lang, pkgDir string) lookup {
		if name == "" {
			return lookup{}
		}
		k := nameKey{name: name, lang: lang, pkgDir: pkgDir}
		if v, ok := nameCache[k]; ok {
			return v
		}
		syms, err := idx.store.GetSymbolsByName(projectID, name, 10)
		if err != nil || len(syms) == 0 {
			nameCache[k] = lookup{}
			return lookup{}
		}
		// #750: a name-fallback binding edge from real project code
		// must not target an isolated testdata fixture. Drop fixture
		// candidates unless every match is a fixture.
		syms = preferNonFixtureSyms(syms)
		// #764: drop candidates outside the reader's package (= source
		// file directory). Skipped when pkgDir is empty.
		if pkgDir != "" {
			scoped := make([]db.Symbol, 0, len(syms))
			for _, s := range syms {
				if filepath.ToSlash(filepath.Dir(s.FilePath)) == pkgDir {
					scoped = append(scoped, s)
				}
			}
			if len(scoped) == 0 {
				nameCache[k] = lookup{}
				return lookup{}
			}
			syms = scoped
		}
		// #436: only match symbols of the same language as the source.
		// #754: priority is Variable > Function > Method. Method
		// candidates are kept (the reads pass emits the bare trailing
		// component of a selector — `w.defaultDo` surfaces as
		// `defaultDo` — so a bare-name read CAN denote a method value;
		// #565's `w.doFn = w.defaultDo` binding edge needs that). But a
		// Function with the same name wins over a Method: a bare
		// `Process()` call-subject ident that also appears in the reads
		// stream must not bind the binding-pass CALLS edge onto an
		// arbitrary same-named method when the real package-level
		// function exists.
		var v, fnCand, methodCand lookup
		for _, s := range syms {
			if lang != "" && s.Language != lang {
				continue
			}
			switch s.Kind {
			case "Variable":
				v = lookup{id: s.ID, lang: s.Language, isVar: true}
			case "Function":
				if fnCand.id == "" {
					fnCand = lookup{id: s.ID, lang: s.Language, isFunc: true}
				}
			case "Method":
				if methodCand.id == "" {
					methodCand = lookup{id: s.ID, lang: s.Language, isFunc: true}
				}
			}
			if v.id != "" {
				break // Variable is the strongest match — stop.
			}
		}
		if v.id == "" {
			if fnCand.id != "" {
				v = fnCand
			} else {
				v = methodCand
			}
		}
		nameCache[k] = v
		return v
	}

	// #487 (resolveReads variant): when multiple files share a FromQN
	// (the `package main` cmd/* fan-out shape), prefer the symbol whose
	// file_path matches the candidate's FromFile so cross-file binding
	// edges attribute to the right caller. Fall back to project-wide
	// pickCanonical when FromFile is empty or doesn't match. Same
	// shape as resolveCalls's lookupFromQN.
	lookupFromQN := func(qn, fromFile string) lookup {
		base := lookupQN(qn)
		if base.id == "" || fromFile == "" {
			return base
		}
		syms := lookupSyms(qn)
		if len(syms) == 0 {
			return base
		}
		for _, s := range syms {
			if s.FilePath == fromFile {
				k := s.Kind
				return lookup{
					id:     s.ID,
					lang:   s.Language,
					isVar:  k == "Variable",
					isFunc: k == "Function" || k == "Method",
				}
			}
		}
		return base
	}

	seen := make(map[string]bool)
	edges := make([]db.Edge, 0, len(pending))
	// #1613 v0.85 follow-up: per-reason drop counters. The READS pass
	// has more distinct drop reasons than IMPORTS/CALLS — language
	// mismatch, polymorphic-method blocklist, struct-field reads, and
	// the "target wasn't a Variable" filter all matter for triaging
	// "10k pending → 3k edges, where did the other 7k go?". Track each
	// separately so the summary log can split them.
	var (
		droppedFromMissing    int
		droppedToMissing      int
		droppedSelfEdge       int
		droppedLangMismatch   int
		droppedPolymorphic    int
		droppedStructField    int
		droppedNotVariable    int
		dedupedDuplicate      int
	)
	for _, e := range pending {
		from := lookupFromQN(e.FromQN, e.FromFile)
		fromID := from.id
		if fromID == "" && !strings.Contains(e.FromQN, ".") {
			// From-side name lookup — no language or package scope yet
			// (we're trying to discover it). The from symbol's language
			// becomes the gate for the to-side lookup below.
			from = lookupNameInLang(e.FromQN, "", "")
			fromID = from.id
		}
		if fromID == "" {
			droppedFromMissing++
			continue
		}
		// #764: the reader's package, approximated by source-file
		// directory — scopes the bare-name to-side fallback so a bare
		// `version` read can't bind to a same-named symbol in another
		// package. Empty when FromFile is unknown (scoping then skipped).
		readerPkgDir := ""
		if e.FromFile != "" {
			readerPkgDir = filepath.ToSlash(filepath.Dir(e.FromFile))
		}
		to := lookupQN(e.ToName)
		// #731: a bare ToName (`version`) can collide with a *qualified
		// name* in another language — JSON/YAML config files have a
		// top-level `version` key whose QN is literally "version", while
		// the Go package var's QN is `main.version`. So lookupQN
		// "succeeds" with the cross-language Setting, which used to
		// suppress the name fallback below — and then the #436 guard
		// dropped the edge entirely. Fall through to the same-language
		// name lookup whenever the QN match is empty OR a language
		// mismatch; keep the QN result only if the name lookup finds
		// nothing better (the #436 guard then correctly drops it).
		qnLangMismatch := to.id != "" && from.lang != "" && to.lang != "" && to.lang != from.lang
		if (to.id == "" || qnLangMismatch) && !strings.Contains(e.ToName, ".") {
			if nameTo := lookupNameInLang(e.ToName, from.lang, readerPkgDir); nameTo.id != "" {
				to = nameTo
			}
		}
		// #436: belt-and-suspenders — even when QN matched, drop the
		// edge if the target is a different language than the source.
		// QN matches across languages can happen for short namespace
		// segments (`util`, `index`) that look identical when lifted
		// to the qualified-name table.
		if to.id == "" {
			droppedToMissing++
			continue
		}
		if fromID == to.id {
			droppedSelfEdge++
			continue
		}
		if from.lang != "" && to.lang != "" && from.lang != to.lang {
			droppedLangMismatch++
			continue
		}
		// #565: when the READS-pass target resolves to a Function or
		// Method (not a Variable), the source AST node was a function
		// value reference — `s.spawnFn = s.defaultSpawn`,
		// `T{Handler: someFn}`, struct-literal callback fields. The
		// existing extractGoCalls path emits a CALLS edge only when
		// the identifier appears as the call subject `(`; bare
		// references don't produce one. So the target stays
		// unreachable in the call graph and dead_code false-positives
		// it. Convert here: emit a CALLS edge with confidence 0.4
		// (lower than direct CALLS at 0.7 since this is a binding,
		// not an invocation — the call happens later through the
		// binding). Existing extractGoCalls CALLS edges win the
		// UNIQUE constraint dedup, so direct calls keep their
		// confidence. Net effect: dead_code stops flagging methods
		// and functions whose only "caller" is a binding expression.
		if to.isFunc {
			// Apply the same polymorphic-method blocklist that
			// resolveCalls uses (#465). Without this, every read of an
			// identifier whose name happens to be a polymorphic method
			// (`String`, `Read`, `Close`, `Write`, etc.) — including
			// when it's the call-subject of a stdlib type's method
			// invocation like `b.String()` — would false-bind to a
			// project-internal Method of that name. Same blocklist
			// keeps the rule symmetric across both passes.
			//
			// Extract the trailing component for the polymorphic
			// check, since e.ToName is whatever extractGoReads
			// emitted (always a bare name for the Ident-walk path).
			candName := e.ToName
			if i := strings.LastIndex(candName, "."); i > 0 && i < len(candName)-1 {
				candName = candName[i+1:]
			}
			if isPolymorphicInterfaceMethodName(candName) {
				droppedPolymorphic++
				continue
			}
			// #760: the read resolved to a project Method, but if the
			// extractor knew the base expression's type and that type
			// is a project struct with a field of this name, the AST
			// node was a struct-field read (`e.Confidence`), not a
			// function-value reference (`w.defaultDo`). Drop — emitting
			// a binding CALLS edge here false-binds `e.Confidence` to
			// `*hclExtractor.Confidence`.
			if isStructFieldRead(e.BaseType, e.FromQN, candName) {
				droppedStructField++
				continue
			}
			key := fromID + "\x00" + to.id + "\x00CALLS"
			if seen[key] {
				dedupedDuplicate++
				continue
			}
			seen[key] = true
			edges = append(edges, db.Edge{
				ProjectID:  projectID,
				FromID:     fromID,
				ToID:       to.id,
				Kind:       "CALLS",
				Confidence: 0.4,
				// Distinct source tag so resolveCalls's resolve_pass
				// CALLS aren't wiped by this pass's atomic replace.
				// Both pass-tags satisfy dead_code's any-inbound-edge
				// check; trace + query treat them identically.
				Source: "binding_pass",
			})
			continue
		}
		// Variable target → READS / WRITES edge. Anything else (Class,
		// Interface, Type, etc.) was a stray identifier reference —
		// drop.
		if !to.isVar {
			droppedNotVariable++
			continue
		}
		// Dedupe key includes the edge kind so a function that BOTH
		// reads and writes the same var produces two edges (READS +
		// WRITES) — they answer different refactor questions.
		key := fromID + "\x00" + to.id + "\x00" + e.Kind
		if seen[key] {
			dedupedDuplicate++
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

	// #1613 v0.85 follow-up: per-stage summary. READS has the richest
	// drop-reason taxonomy of the four resolvers — language mismatch,
	// polymorphic-method blocklist, struct-field reads, and the
	// not-Variable filter each map to a distinct extractor or
	// resolver behavior. Quiet on no-drops happy path.
	totalDropped := droppedFromMissing + droppedToMissing + droppedSelfEdge +
		droppedLangMismatch + droppedPolymorphic + droppedStructField +
		droppedNotVariable + dedupedDuplicate
	if totalDropped > 0 {
		slog.Info("pincher.reads.resolve.summary",
			"project_id", projectID,
			"pending_in", len(pending),
			"resolved_out", len(edges),
			"dropped_from_missing", droppedFromMissing,
			"dropped_to_missing", droppedToMissing,
			"dropped_self_edge", droppedSelfEdge,
			"dropped_lang_mismatch", droppedLangMismatch,
			"dropped_polymorphic_method", droppedPolymorphic,
			"dropped_struct_field_read", droppedStructField,
			"dropped_not_variable", droppedNotVariable,
			"deduped_duplicate", dedupedDuplicate,
		)
	}

	// #475: atomic replace of the prior resolve pass's READS + WRITES.
	// Both kinds share the read-pass output, so wipe both before insert.
	for _, k := range []string{"READS", "WRITES"} {
		if err := idx.store.DeleteEdgesByKindAndSource(projectID, k, "resolve_pass"); err != nil {
			slog.Warn("pincher.reads.delete_prior.err", "kind", k, "err", err)
		}
	}
	// #565: separate atomic replace for function-value-binding CALLS
	// edges (source='binding_pass'). Distinct from resolveCalls's
	// resolve_pass CALLS so this delete doesn't nuke the direct-call
	// graph. Per-file CALLS (source='per_file') are also untouched.
	if err := idx.store.DeleteEdgesByKindAndSource(projectID, "CALLS", "binding_pass"); err != nil {
		slog.Warn("pincher.binding.delete_prior.err", "err", err)
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
