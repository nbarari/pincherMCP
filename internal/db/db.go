// Package db manages the SQLite store for pincherMCP.
//
// Design: every symbol row serves three purposes simultaneously:
//   (1) Byte-offset O(1) source retrieval  (start_byte / end_byte)
//   (2) Knowledge graph node               (kind, edges table)
//   (3) FTS5 BM25 full-text search         (symbols_fts virtual table)
//
// All three indexes are populated in a single AST parse pass.
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tkz "github.com/tiktoken-go/tokenizer"
	_ "modernc.org/sqlite"
)

// tke is the cl100k_base tokenizer singleton (same BPE family as Claude).
// Initialised once on first use; falls back to 4-char approximation on error.
var (
	tke     tkz.Codec
	tkeOnce sync.Once
	tkeErr  error
)

func getTokenizer() tkz.Codec {
	tkeOnce.Do(func() {
		tke, tkeErr = tkz.Get(tkz.Cl100kBase)
	})
	return tke
}

// Store wraps a SQLite database.
//
// Two connection pools point at the same underlying SQLite file:
//
//   - db: the WRITER pool. SetMaxOpenConns(1) preserves the
//     single-writer invariant SQLite requires. All Exec / Begin paths
//     route here.
//   - ro: the READER pool. SetMaxOpenConns(N) (default 4); opened with
//     `mode=ro` query parameter so SQLite enforces RO at the file
//     level, not just by routing convention. All Query / QueryRow
//     paths SHOULD route here.
//
// Why two pools (#51): SQLite WAL allows concurrent readers without
// blocking the writer. Pre-fix, every read serialized through the
// single writer pool — a `search` call queued behind an active
// `Index()` waited for the indexer to release the connection between
// flushes. With the reader pool, concurrent tool calls run in parallel.
//
// SECURITY / CORRECTNESS:
//   - The reader pool's MaxOpenConns MUST stay at 1 (single-writer)
//     — see TestStore_WriterPoolStaysSingleWriter.
//   - Reads that happen to use s.db still work (writer pool serves
//     reads too); migrating them to s.ro is purely a performance win.
//   - Migrations, Optimize, CheckpointTruncate, all PRAGMA writes,
//     all Exec, all Begin paths — must use s.db.
type Store struct {
	db   *sql.DB // writer pool: MaxOpenConns=1
	ro   *sql.DB // reader pool: MaxOpenConns=4 (default), mode=ro
	Path string

	// lastStartupMigrationInvalidates records the union of invalidates
	// across migrations actually applied during this process's Open()
	// call (#1497). Doctor surfaces this so users can see "the bump
	// from v30→v33 was schema-only — your symbol/edge data is still
	// fresh." A future binaryDriftForce gate (#1497 follow-up) will
	// suppress the force=true cascade when this is invalidatesNothing.
	//
	// Empty (`{}`) on Open() against an up-to-date DB (no migrations
	// applied this startup). Populated only by migrate().
	lastStartupMigrationInvalidates  MigrationInvalidates
	lastStartupMigrationsAppliedFrom int // schema version at Open()
	lastStartupMigrationsAppliedTo   int // schema version after migrate()
}

// LastStartupMigrationInvalidates returns the union of invalidates
// scopes across schema migrations applied during this process's
// Open() call (#1497). Returns {} when no migrations ran (DB already
// at current schema). Plus the version range that was bumped — `from`
// is the schema version found at startup, `to` is the schema version
// after migrate() completed.
//
// Used by doctor and stats to surface "this binary upgrade was a
// schema-only DDL slice with no impact on extraction data" vs "this
// upgrade requires re-extraction" — guides users on whether the next
// `make install` would benefit from a manual `pincher index --force`.
func (s *Store) LastStartupMigrationInvalidates() (inv MigrationInvalidates, from, to int) {
	return s.lastStartupMigrationInvalidates, s.lastStartupMigrationsAppliedFrom, s.lastStartupMigrationsAppliedTo
}

// DataDir returns the platform data directory for pincherMCP, honouring
// $PINCHER_DATA_DIR if set so dev binaries can be pinned to a separate
// dir from the user's stable install. Order:
//
//  1. $PINCHER_DATA_DIR — used verbatim, no `pincherMCP` suffix appended
//     (the env var IS the full directory path).
//  2. Platform default — `%APPDATA%\pincherMCP\` on Windows,
//     `~/Library/Application Support/pincherMCP/` on macOS,
//     `$XDG_DATA_HOME/pincherMCP/` (fallback `~/.local/share/pincherMCP/`)
//     on Linux.
//
// `--data-dir` CLI flags still override both — the flag is checked first,
// and only if empty does the caller fall back to DataDir(). This lets the
// env var be a session-wide default for dev shells without forcing it on
// scripted callers that always pass `--data-dir` explicitly.
func DataDir() (string, error) {
	if env := strings.TrimSpace(os.Getenv("PINCHER_DATA_DIR")); env != "" {
		return env, os.MkdirAll(env, 0o700)
	}
	var base string
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("APPDATA")
		if base == "" {
			base = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
		}
	case "darwin":
		base = filepath.Join(os.Getenv("HOME"), "Library", "Application Support")
	default:
		base = os.Getenv("XDG_DATA_HOME")
		if base == "" {
			base = filepath.Join(os.Getenv("HOME"), ".local", "share")
		}
	}
	dir := filepath.Join(base, "pincherMCP")
	return dir, os.MkdirAll(dir, 0o700)
}

// Open opens (or creates) the pincher database at dir/pincher.db.
//
// modernc.org/sqlite parses query parameters in `_pragma=name(value)` form,
// not the mattn/go-sqlite3 convention of `_name=value`. The previous DSN
// used the mattn form, so every PRAGMA was silently ignored — including
// journal_mode=WAL — leaving every database in default `delete` (rollback
// journal) mode. The post-Open assertion below catches any future regression.
func Open(dir string) (*Store, error) {
	// #830: create the data dir if it's missing. DataDir() already
	// MkdirAll's the default + PINCHER_DATA_DIR paths, but a `--data-dir`
	// flag pointing at a not-yet-existing dir reached here uncreated and
	// SQLite failed with a misleading SQLITE_CANTOPEN ("out of memory
	// (14)" in modernc's wording). Creating it here makes all three
	// data-dir sources consistent.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, "pincher.db")
	// WAL mode + normal sync = best throughput/durability tradeoff.
	// cache_size=-65536 = 64 MB page cache.
	// busy_timeout=5000ms prevents immediate failure when a write lock is held
	// (e.g. watcher auto-index overlapping a manual index call).
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=cache_size(-65536)&_pragma=busy_timeout(5000)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer
	db.SetMaxIdleConns(1)

	// Defensive: assert WAL actually engaged. A misconfigured DSN would
	// silently leave the DB in `delete` mode, where readers and writers
	// serialize at the file level — degrading every query.
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		db.Close()
		return nil, fmt.Errorf("verify journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		db.Close()
		return nil, fmt.Errorf("journal_mode is %q, expected WAL — DSN parameters may not be applying", mode)
	}

	s := &Store{db: db, Path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	// WAL guardrail — silent no-op when journal_mode is not WAL.
	//
	// journal_size_limit caps the WAL at 256 MB. After any checkpoint that
	// finds the WAL exceeding this size, SQLite truncates it. Combined with
	// the explicit CheckpointTruncate() call at the tail of Index(), this
	// keeps the WAL bounded under normal operation without paying the
	// per-write checkpoint cost an aggressive wal_autocheckpoint would
	// impose.
	//
	// History note: an earlier version of this branch also set
	// wal_autocheckpoint = 100 to defend against the 70 GB WAL observed
	// under the 2026-04-29 multi-writer storm. That setting cost an
	// empirical 14.5× slowdown on heavy single-writer indexing (the
	// 484K-symbol thinksmart corpus went from 78s to 1124s), so we
	// reverted to the SQLite default of 1000 pages. The real defense
	// against that storm is the cross-process lockfile + the --hook
	// bloat-trap guard, which together prevent the multi-writer scenario
	// that starved checkpoints in the first place.
	_, _ = db.Exec("PRAGMA journal_size_limit = 268435456")

	if err := s.attachReaderPool(path, DefaultReaderPoolSize); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// DefaultReaderPoolSize is the reader pool's MaxOpenConns when not
// overridden via OpenWithReaders / SetReaderPoolSize. 4 connections
// covers the typical concurrent-tool-call burst (search + symbol +
// query + trace simultaneously) without exhausting file descriptors
// on small VMs.
const DefaultReaderPoolSize = 4

// MaxReaderPoolSize caps any caller-requested reader pool size.
// Above ~32 connections, SQLite's per-process locking machinery costs
// more than the parallelism gains; documented limit so a user passing
// --db-readers 1000 doesn't shoot themselves in the foot.
const MaxReaderPoolSize = 32

// MinReaderPoolSize prevents callers from disabling the reader pool
// entirely (which would defeat the point of having one). 1 means
// reads serialize but still go through the RO path so SQLite's
// mode=ro defense still applies; useful in resource-constrained
// environments where the extra connections matter.
const MinReaderPoolSize = 1

// OpenWithReaders is Open + a tunable reader pool size. Callers that
// want non-default parallelism (server deployments fronted by HTTP,
// perf-tuned configurations) use this; everyone else uses Open.
//
// readers is clamped to [MinReaderPoolSize, MaxReaderPoolSize]. Pass 0
// to use DefaultReaderPoolSize.
func OpenWithReaders(dir string, readers int) (*Store, error) {
	s, err := Open(dir)
	if err != nil {
		return nil, err
	}
	if readers != DefaultReaderPoolSize {
		if err := s.SetReaderPoolSize(readers); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// SetReaderPoolSize re-tunes the reader pool's MaxOpenConns at runtime.
// readers is clamped to [MinReaderPoolSize, MaxReaderPoolSize]; pass 0
// to fall back to DefaultReaderPoolSize.
func (s *Store) SetReaderPoolSize(readers int) error {
	if readers <= 0 {
		readers = DefaultReaderPoolSize
	}
	if readers < MinReaderPoolSize {
		readers = MinReaderPoolSize
	}
	if readers > MaxReaderPoolSize {
		readers = MaxReaderPoolSize
	}
	if s.ro == nil {
		return fmt.Errorf("reader pool not initialised")
	}
	s.ro.SetMaxOpenConns(readers)
	s.ro.SetMaxIdleConns(readers)
	return nil
}

// attachReaderPool opens the reader-side pool against `path`. Internal
// helper so Open + OpenWithReaders share the same DSN + RO discipline.
//
// `mode=ro` makes SQLite enforce read-only at the file level, so even a
// routing bug that sends a write through s.ro fails fast with "attempt
// to write a readonly database" instead of silently corrupting state.
// busy_timeout matches the writer so contention retries align.
// cache_size is half the writer's; readers don't need as much page-cache
// because they don't dirty pages.
func (s *Store) attachReaderPool(path string, readers int) error {
	if readers < MinReaderPoolSize {
		readers = MinReaderPoolSize
	}
	if readers > MaxReaderPoolSize {
		readers = MaxReaderPoolSize
	}
	roDSN := fmt.Sprintf(
		"file:%s?mode=ro&_pragma=cache_size(-32768)&_pragma=busy_timeout(5000)",
		path,
	)
	ro, err := sql.Open("sqlite", roDSN)
	if err != nil {
		return fmt.Errorf("open reader pool: %w", err)
	}
	ro.SetMaxOpenConns(readers)
	ro.SetMaxIdleConns(readers)
	s.ro = ro

	// #401: warm the reader pool so the first MCP call after server
	// start doesn't pay the connection-open cost (~5-15ms per
	// connection on cold disk). database/sql opens connections
	// lazily; explicitly grab and release N connections here so
	// `readers` slots are pre-filled. PingContext acquires the
	// connection, runs a no-op probe, returns it to the pool. We
	// run them in parallel — each Ping waits for its own slot, and
	// they don't contend with each other since the pool has N
	// independent slots.
	warmCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			conn, err := ro.Conn(warmCtx)
			if err != nil {
				return // best-effort; lazy open will still work on first real call
			}
			_ = conn.PingContext(warmCtx)
			_ = conn.Close()
		}()
	}
	wg.Wait()
	return nil
}

// DB returns the WRITER pool for advanced callers that need to write.
// Most direct callers want RO() instead — this method preserved for
// backward compatibility with code that doesn't yet distinguish.
func (s *Store) DB() *sql.DB { return s.db }

// RO returns the READER pool — read-only, MaxOpenConns=4 by default.
// Use this for direct sql.DB access in pure-SELECT contexts (e.g. the
// Cypher executor). Routing reads here unlocks WAL's concurrent-reader
// capability instead of serialising through the single writer.
func (s *Store) RO() *sql.DB {
	if s.ro != nil {
		return s.ro
	}
	// Defensive fallback: if the reader pool failed to open for some
	// reason, fall back to the writer so callers don't crash. Slower
	// but functionally correct.
	return s.db
}

// Close closes both pools. Safe to call multiple times; the underlying
// sql.DB.Close handles redundant calls cleanly.
func (s *Store) Close() error {
	var firstErr error
	if s.ro != nil {
		if err := s.ro.Close(); err != nil {
			firstErr = err
		}
	}
	if err := s.db.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Optimize runs SQLite's PRAGMA optimize — a lightweight stats refresh that
// returns in milliseconds when nothing's changed and only does real work when
// query-planner stats have gone stale (e.g. after a large index batch). Call
// this at the tail of a large Index() run; cheap insurance for Cypher query
// planning as the symbol table grows.
func (s *Store) Optimize() error {
	_, err := s.db.Exec("PRAGMA optimize")
	return err
}

// CheckpointTruncate runs PRAGMA wal_checkpoint(TRUNCATE), folding the WAL
// back into the main DB and physically truncating the WAL file. Cheap when
// the WAL is already small. Call at the tail of a large Index() to force
// the WAL back toward zero before queries resume — the natural quiet point
// for a checkpoint with no readers waiting on the older snapshot.
//
// Returns an error only if the SQL itself fails; a checkpoint that can't
// truncate (e.g. a reader is still on the old snapshot) succeeds quietly
// and the caller can ignore that case.
func (s *Store) CheckpointTruncate() error {
	_, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// Vacuum runs SQLite's VACUUM — it rewrites the entire database file,
// reclaiming pages freed by DeleteProject / DeleteSymbolsForFile so the
// file on disk actually shrinks. This is the sharp edge: VACUUM holds an
// exclusive lock for the duration and on a multi-GB DB can take a while,
// which is exactly why it lives behind a deliberate `pincher vacuum` CLI
// step (#732) rather than in the hot MCP path. CheckpointTruncate runs
// first so the WAL is folded in before the rewrite.
//
// #1149: returns walReaderBusy=true when the pre-VACUUM checkpoint
// couldn't fully roll forward — typically a running MCP server child
// holding an open reader snapshot. Pre-fix, that case silently
// reclaimed 0 B and the user concluded `pincher vacuum` was a no-op.
// VacuumResult lets the CLI surface a targeted advisory.
func (s *Store) Vacuum() (VacuumResult, error) {
	var res VacuumResult
	// Run a probing checkpoint first so we can detect open readers
	// before VACUUM swallows the time. wal_checkpoint(TRUNCATE) returns
	// (busy, log, checkpointed). busy=1 means another connection
	// prevented full truncation — equivalent to "a reader is on an
	// older snapshot." Surface that to the caller.
	var busy, logFrames, ckptFrames int64
	if err := s.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &ckptFrames); err != nil {
		return res, err
	}
	res.WalReaderBusy = busy != 0
	if _, err := s.db.Exec("VACUUM"); err != nil {
		return res, err
	}
	// In WAL mode the VACUUM rewrite lands in the WAL, not the main file.
	// Checkpoint again so the on-disk `pincher.db` actually shrinks — the
	// whole point of the command — rather than just shuffling the bytes
	// into a fat WAL the caller can't see.
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return res, err
	}
	// #1219 step 3: PRAGMA optimize — re-analyzes table stats so the
	// query planner picks good indexes on subsequent queries. Cheap,
	// idempotent; recommended after any bulk delete/insert. We just
	// rewrote the entire file via VACUUM, so the planner's prior stats
	// are stale by definition.
	if _, err := s.db.Exec("PRAGMA optimize"); err != nil {
		// Optimize is advisory — fail soft and let the vacuum win
		// stand. Surface via a separate field so the CLI can mention
		// it in the receipt if a user asks why their queries are
		// still slow post-vacuum.
		res.OptimizeError = err.Error()
	}
	// #1219 step 4: FTS5 inverted-index compaction. Each per-corpus
	// vtab maintains its own segment list; long-running indexers
	// accumulate fragments that slow BM25 ranking and bloat the index
	// pages on disk. The 'optimize' command merges segments down. Run
	// once per FTS5 vtab — there are three (code/config/docs split,
	// #106 v12 migration). Failures here are also advisory.
	for _, vtab := range []string{"symbols_code_fts", "symbols_config_fts", "symbols_docs_fts"} {
		stmt := fmt.Sprintf("INSERT INTO %s(%s) VALUES('optimize')", vtab, vtab)
		if _, err := s.db.Exec(stmt); err != nil {
			if res.FTSOptimizeError == "" {
				res.FTSOptimizeError = fmt.Sprintf("%s: %v", vtab, err)
			} else {
				res.FTSOptimizeError += fmt.Sprintf("; %s: %v", vtab, err)
			}
		}
	}
	return res, nil
}

// VacuumResult carries diagnostic signals from the VACUUM run so the
// CLI can render a targeted advisory. WalReaderBusy=true is the
// #1149 signal: an open reader pinned the freelist pages VACUUM would
// have reclaimed; advise the user to close the running MCP child (or
// retry post-/mcp-reconnect) and re-vacuum.
//
// OptimizeError + FTSOptimizeError are advisory: the post-VACUUM
// PRAGMA optimize and per-vtab FTS5 'optimize' calls (#1219 steps
// 3-4) are best-effort. If they fail, the load-bearing VACUUM has
// already happened — the caller should know about the failure but
// not treat it as a vacuum failure.
type VacuumResult struct {
	WalReaderBusy    bool
	OptimizeError    string
	FTSOptimizeError string
}

// RebuildFTS drops every per-corpus FTS5 vtab (`symbols_code_fts` /
// `symbols_config_fts` / `symbols_docs_fts`) and their sync triggers,
// recreates them from canonical DDL, and bulk-loads them from the
// symbols table. Returns the total number of symbol rows ingested
// (sum across the three corpora — each row lands in exactly one).
//
// The legacy `symbols_fts` index (and its three triggers) was removed
// in #106's v12 migration; this function also drops them defensively
// in case a stale instance remains on a partially-migrated DB.
//
// This is the FTS5 escape hatch — for situations where the trigger-driven
// index has drifted from `symbols`: an interrupted index that left FTS5
// shadow tables inconsistent, a bug in a previous version that bypassed
// the triggers, or a SQLite/FTS5 version mismatch on the underlying
// module.
//
// Operates atomically inside a single transaction: if any step fails
// the original FTS5 state is preserved. Cost is proportional to the
// symbol count — not a hot path, expect seconds-to-minutes on large
// repos.
func (s *Store) RebuildFTS() (rows int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Drop triggers BEFORE the vtabs so the trigger DROPs don't try to
	// fire against a missing vtab, and so a subsequent insert from
	// symbols isn't shadow-written by a stale trigger.
	for _, stmt := range []string{
		// Legacy index — removed in v12 (#106). Defensive drop for any
		// partially-migrated DB.
		`DROP TRIGGER IF EXISTS sym_fts_insert`,
		`DROP TRIGGER IF EXISTS sym_fts_delete`,
		`DROP TRIGGER IF EXISTS sym_fts_update`,
		`DROP TABLE IF EXISTS symbols_fts`,
		// Per-corpus indexes (v9, #32 part 1) — only present after the
		// v9 migration ran, but DROP IF EXISTS handles pre-v9 DBs.
		`DROP TRIGGER IF EXISTS sym_fts_corpus_insert`,
		`DROP TRIGGER IF EXISTS sym_fts_corpus_delete`,
		`DROP TRIGGER IF EXISTS sym_fts_corpus_update`,
		`DROP TABLE IF EXISTS symbols_code_fts`,
		`DROP TABLE IF EXISTS symbols_config_fts`,
		`DROP TABLE IF EXISTS symbols_docs_fts`,
	} {
		if _, err = tx.Exec(stmt); err != nil {
			return 0, fmt.Errorf("drop %s: %w", stmt, err)
		}
	}

	// Recreate per-corpus DDL — the v9 migration body is the source of
	// truth (vtab DDL + sync triggers + bulk backfill). Re-exec it here
	// so a future change to ftsCorpusSplitDDL's backfill rules
	// propagates to RebuildFTS automatically.
	if _, err = tx.Exec(ftsCorpusSplitDDL); err != nil {
		return 0, fmt.Errorf("recreate corpus fts: %w", err)
	}

	// Sum rows across the three corpora — each symbol is in exactly one.
	// The single source of truth is `symbols` itself; counting from there
	// avoids any FTS5-shadow-table quirk where COUNT(*) on a vtab can
	// over-report (each row indexed by its tokens, plus internal docsize
	// rows on some queries — the SQL planner hits a shadow table that
	// holds N rows per stored symbol). The symbol count also exactly
	// matches what the backfill inserted (the WHERE clauses partition
	// `symbols` into three disjoint subsets).
	if err = tx.QueryRow(`SELECT COUNT(*) FROM symbols`).Scan(&rows); err != nil {
		return 0, fmt.Errorf("count rebuilt rows: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return rows, nil
}

// FTS5CorpusFragmentation summarises one per-corpus FTS5 vtab's
// fragmentation state. data_rows / idx_rows is the practical
// fragmentation ratio: a freshly-rebuilt FTS5 hovers around 1–3x;
// values above ~10x indicate accumulated micro-segments worth
// merging via `rebuild_fts`. See #1612.
type FTS5CorpusFragmentation struct {
	Corpus     string // "code" / "config" / "docs"
	IdxRows    int64
	DataRows   int64
	Ratio      float64 // data_rows / idx_rows, 0 when idx_rows == 0
	NeedsRebuild bool  // ratio crossed the advisory threshold
}

// FTS5FragmentationThreshold is the ratio above which `pincher doctor`
// surfaces the fragmentation advisory. Revised in #1663 from 10x to
// 25x after live dogfood data showed post-rebuild ratios are corpus-
// dependent: code corpus settles at 2x post-rebuild, but config
// corpus on a 27-project install with heavy YAML/JSON/TOML naturally
// settles at 10–11x immediately after `rebuild_fts`. A 10x threshold
// false-positived every time the config corpus was inspected, training
// the user to ignore the advisory. 25x sits well above the observed
// post-rebuild floor for the largest known healthy case (10.4x) while
// still catching the real degradation case (62.7x observed on the
// pre-rebuild config corpus that drove #1612 in the first place).
//
// If a corpus turns up that legitimately sits above 25x post-rebuild,
// the threshold deserves another revisit — but we'd need a third
// data point first.
const FTS5FragmentationThreshold = 25.0

// FTS5Fragmentation returns per-corpus fragmentation stats for the
// three per-corpus FTS5 virtual tables (`symbols_code_fts`,
// `symbols_config_fts`, `symbols_docs_fts`). Reader-routed; uses the
// FTS5 shadow tables (`_idx` and `_data`) directly because the
// public FTS5 API exposes only `integrity-check` not fragmentation
// numbers. Returns an empty slice if any of the vtabs is missing
// (e.g., schema migrations haven't run yet).
//
// Cheap — three COUNT(*) queries against bounded shadow tables.
// Latency is dominated by the SQLite open path, not these counts.
func (s *Store) FTS5Fragmentation() ([]FTS5CorpusFragmentation, error) {
	corpora := []string{"code", "config", "docs"}
	out := make([]FTS5CorpusFragmentation, 0, len(corpora))
	for _, c := range corpora {
		row := FTS5CorpusFragmentation{Corpus: c}
		if err := s.ro.QueryRow(
			fmt.Sprintf(`SELECT COUNT(*) FROM symbols_%s_fts_idx`, c),
		).Scan(&row.IdxRows); err != nil {
			// Missing vtab — pre-v9 schema or partial migration. Return
			// what we have so the advisory just stays silent.
			return out, nil
		}
		if err := s.ro.QueryRow(
			fmt.Sprintf(`SELECT COUNT(*) FROM symbols_%s_fts_data`, c),
		).Scan(&row.DataRows); err != nil {
			return out, nil
		}
		if row.IdxRows > 0 {
			row.Ratio = float64(row.DataRows) / float64(row.IdxRows)
			row.NeedsRebuild = row.Ratio > FTS5FragmentationThreshold
		}
		out = append(out, row)
	}
	return out, nil
}

// MigrationInvalidates declares what previously-extracted data a
// migration makes stale (#1497). Used by upstream consumers (doctor,
// the future binaryDriftForce gate) to distinguish "schema bumped but
// no extraction-output table touched" migrations (sessions metrics,
// diagnostic surfaces) from migrations that materially affect what
// queries return against pre-migration data (new columns on symbols
// or edges, new tables populated only on re-extraction).
//
// Defaults to All (full reindex required) when unsure — under-
// invalidating is a silent correctness bug; over-invalidating is just
// a perf miss.
type MigrationInvalidates struct {
	// All is true when the migration changes data that any query is
	// likely to consult. Default-conservative for any column added to
	// symbols/edges/pending_edges, or any new table populated only
	// via the per-file extraction goroutine.
	All bool

	// Languages, when non-empty, narrows the invalidation to files of
	// the listed languages (matches `ast.SupportedLanguages()` keys).
	// Reserved for a future per-language hash invalidation pass; not
	// currently consumed by the indexer but recorded for completeness
	// so the future gate can compute the precise file set.
	Languages []string
}

// invalidatesNothing: sentinel for migrations that touch no
// extraction-output table — sessions metrics, diagnostic surfaces,
// metadata-only columns. A user upgrading across only-Nothing
// migrations could skip the binaryDriftForce reindex entirely once
// that gate is wired through (#1497 follow-up).
var invalidatesNothing = MigrationInvalidates{}

// invalidatesAll: sentinel for migrations that affect extraction-
// output data — new symbols/edges columns, new tables populated by
// the per-file extractor, trigger changes that re-route corpus
// assignment, etc. Default for any migration whose data shape
// couldn't be classified as session-only.
var invalidatesAll = MigrationInvalidates{All: true}

// schemaMigrations is an ordered list of incremental SQL migrations applied
// after the baseline schema. migrations[i] upgrades version (i+1) → (i+2).
// To add a schema change: append a SQL string here AND append a matching
// MigrationInvalidates entry to schemaMigrationInvalidates below — the
// version number is derived from the slice length automatically.
//
// Rules:
//   - Never edit an existing entry (deployed databases have already run it).
//   - Each entry must be idempotent where possible (use IF NOT EXISTS / IF EXISTS).
//   - Keep entries small and focused (one logical change per entry).
var schemaMigrations = []string{
	// v1 → v2: extraction_confidence column on symbols lets callers distinguish
	// go/ast-exact results (1.0) from regex-approximate results (0.6–0.9).
	`ALTER TABLE symbols ADD COLUMN extraction_confidence REAL NOT NULL DEFAULT 1.0`,

	// v2 → v3: symbol_moves table tracks stale IDs so agents holding an old
	// symbol ID (from before a file was moved/renamed) can still resolve it.
	// The index on (project_id, qualified_name, kind) supports move detection
	// queries that look up existing symbols by qualified name.
	`CREATE TABLE IF NOT EXISTS symbol_moves (
		old_id     TEXT    NOT NULL,
		new_id     TEXT    NOT NULL,
		project_id TEXT    NOT NULL,
		moved_at   INTEGER NOT NULL,
		PRIMARY KEY (old_id, project_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_sym_qnkind ON symbols(project_id, qualified_name, kind)`,

	// v3 → v4: sessions table records per-session savings so ROI is provable
	// across reconnects and over time. Each row is one MCP connection (session_id
	// is generated at startup). The server upserts this row periodically so data
	// survives even without a clean shutdown.
	`CREATE TABLE IF NOT EXISTS sessions (
		session_id   TEXT    NOT NULL PRIMARY KEY,
		started_at   INTEGER NOT NULL,
		last_seen    INTEGER NOT NULL,
		calls        INTEGER NOT NULL DEFAULT 0,
		tokens_used  INTEGER NOT NULL DEFAULT 0,
		tokens_saved INTEGER NOT NULL DEFAULT 0,
		cost_avoided REAL    NOT NULL DEFAULT 0.0
	)`,

	// v5 → v6: add a generated `symbol_id` column on `symbols` that mirrors `id`.
	// The symbols_fts virtual table uses external content (content='symbols') and
	// declares its first column as `symbol_id`, while the source column is `id`.
	// Any FTS op that needs to consult the source table by FTS column name —
	// integrity-check, optimize, snippet()/highlight() aux functions, raw
	// SELECT * FROM symbols_fts — issues `SELECT symbol_id, ... FROM symbols
	// WHERE rowid = ?` and fails with `no such column: T.symbol_id` without
	// this column. The MATCH-then-JOIN query path used by SearchSymbols never
	// triggers a content lookup, which is why the bug stayed latent.
	// VIRTUAL = computed at read time, zero storage, no FTS rebuild required.
	`ALTER TABLE symbols ADD COLUMN symbol_id TEXT GENERATED ALWAYS AS (id) VIRTUAL`,

	// v6 → v7: extraction_failures table — first piece of #42's diagnostic
	// surface. Captures parse failures, panics, and sanity-heuristic flags
	// from the indexer so users can see what pincher couldn't index without
	// dropping into the SQLite file.
	//
	// UNIQUE(project_id, file_path, reason) means re-indexing a file with
	// a persistent error doesn't multiply rows; instead INSERT OR REPLACE
	// updates last_seen_at to the current time. Old rows for files whose
	// errors have been fixed will get cleaned up by a future "ageing"
	// pass; for now they remain visible in the failure report and the
	// user can manually clear them via `pincher index --force`.
	//
	// reason is machine-readable (e.g. "parse_error", "extractor_panicked",
	// "byte_range_negative"); details is human-readable (the first line of
	// the error message, or the suspicious value). Both are bounded —
	// the indexer truncates details to 1024 chars before INSERT.
	//
	// Table + index combined into one migration entry (semicolon-separated)
	// so the schema-version bump is coherent: v6 → v7 in one logical step.
	`CREATE TABLE IF NOT EXISTS extraction_failures (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id    TEXT    NOT NULL REFERENCES projects(id),
		file_path     TEXT    NOT NULL,
		language      TEXT    NOT NULL,
		reason        TEXT    NOT NULL,
		details       TEXT,
		first_seen_at INTEGER NOT NULL,
		last_seen_at  INTEGER NOT NULL,
		UNIQUE(project_id, file_path, reason)
	);
	CREATE INDEX IF NOT EXISTS idx_extraction_failures_recent
	   ON extraction_failures(project_id, last_seen_at DESC);`,

	// v7 → v8: slow_queries log — second piece of #42's diagnostic surface.
	// Captures every tool call whose latency exceeds the configured threshold
	// (--slow-query-ms, default 0 = disabled). Lets users see which queries
	// are taking too long without external profiling.
	//
	// project_id is nullable: cross-project tools (e.g. list, health) don't
	// have a project context. arguments is JSON-encoded after secret-shaped
	// values are redacted ([redacted] sentinel) — never stores raw API keys
	// or tokens passed in tool args.
	`CREATE TABLE IF NOT EXISTS slow_queries (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		tool        TEXT    NOT NULL,
		project_id  TEXT,
		duration_ms INTEGER NOT NULL,
		arguments   TEXT,
		occurred_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_slow_queries_recent
	   ON slow_queries(occurred_at DESC);`,

	// v8 → v9: per-corpus FTS5 split (#32 part 1).
	// Creates three new FTS5 vtabs alongside legacy `symbols_fts`. Each
	// covers a slice of the symbol corpus (code / config / docs), routed
	// at trigger time by language (and by kind for Document). Sync triggers
	// fire on every INSERT/UPDATE/DELETE on `symbols`, alongside the legacy
	// triggers — all four indexes stay populated.
	//
	// This migration intentionally produces ZERO observable change to
	// callers. SearchSymbols still queries `symbols_fts` exclusively.
	// Part 2 will add a `corpus=` parameter on SearchSymbols. Part 3 will
	// flip the default to `corpus=code` and deprecate the legacy index.
	//
	// Routing rules (mirror internal/db/corpus.go's ClassifyCorpus):
	//   docs:   kind = 'Document'  OR  language = 'Markdown'
	//   config: language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML')
	//   code:   everything else (default — adding a new code language doesn't
	//           require a migration update; adding a new config/docs
	//           language requires updating both this trigger DDL and the
	//           ClassifyCorpus parity gate)
	//
	// Backfill at the tail copies all existing rows into the appropriate
	// new vtab so freshly-upgraded DBs aren't missing data. Cost: ~5s on
	// a 1M-symbol repo, one-time only.
	ftsCorpusSplitDDL,

	// v9 → v10: add TOML to the config-corpus language list. Fresh v10+
	// DBs get TOML routing for free via the (now-updated) ftsCorpusSplitDDL
	// they ran at v9. Existing v9 DBs already executed the pre-TOML
	// trigger SQL, so we DROP-and-RECREATE the three corpus triggers here
	// with TOML in the WHERE clauses.
	//
	// No backfill needed: pre-v10 builds had no .toml extractor registered,
	// so existing v9 DBs cannot have any TOML symbols to re-route.
	`DROP TRIGGER IF EXISTS sym_fts_corpus_insert;
DROP TRIGGER IF EXISTS sym_fts_corpus_delete;
DROP TRIGGER IF EXISTS sym_fts_corpus_update;

CREATE TRIGGER IF NOT EXISTS sym_fts_corpus_insert AFTER INSERT ON symbols BEGIN
    INSERT INTO symbols_code_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind = 'Document' OR new.language IN ('Markdown', 'HTML');
END;

CREATE TRIGGER IF NOT EXISTS sym_fts_corpus_delete AFTER DELETE ON symbols BEGIN
    INSERT INTO symbols_code_fts(symbols_code_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(symbols_config_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(symbols_docs_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind = 'Document' OR old.language IN ('Markdown', 'HTML');
END;

CREATE TRIGGER IF NOT EXISTS sym_fts_corpus_update AFTER UPDATE ON symbols BEGIN
    INSERT INTO symbols_code_fts(symbols_code_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(symbols_config_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(symbols_docs_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind = 'Document' OR old.language IN ('Markdown', 'HTML');

    INSERT INTO symbols_code_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind = 'Document' OR new.language IN ('Markdown', 'HTML');
END;`,

	// v10 → v11: HTTP-server discovery columns on `sessions`.
	//
	// Adds `http_url` and `http_pid` so a separate `pincher web` invocation
	// can locate the running HTTP dashboard without scanning ports or
	// reading a side-channel statefile. The flusher writes both columns
	// when the server has bound an HTTP listener; PID is used by `web` to
	// liveness-check stale rows (same pattern as internal/index/lockfile.go).
	//
	// Defaults are empty / zero so pre-v11 callers that never bind HTTP
	// look identical to existing rows. SQLite's ALTER TABLE only adds one
	// column per statement, so two statements are bundled into a single
	// migration entry — the version bump remains a coherent v10→v11.
	`ALTER TABLE sessions ADD COLUMN http_url TEXT NOT NULL DEFAULT '';
	 ALTER TABLE sessions ADD COLUMN http_pid INTEGER NOT NULL DEFAULT 0;`,

	// v11 → v12: remove the legacy `symbols_fts` virtual table and its
	// three sync triggers (#106). The per-corpus FTS5 split (#32 part 1,
	// landed at v9) has been carrying every search query for two
	// minor-version cycles via the per-corpus vtabs (`symbols_code_fts`
	// / `symbols_config_fts` / `symbols_docs_fts`). The legacy index
	// has been double-populated alongside ever since, paying a 4×
	// write-amp tax for callers nobody actually has — the MCP search
	// handler soft-redirects `corpus=all` (the only caller-facing path
	// to the legacy index) to `corpus=code` since #78.
	//
	// Drop order matters: triggers first so the next symbol upsert
	// doesn't try to write to a dropped vtab; then the vtab itself
	// (which removes its 5 shadow tables: _config, _content, _data,
	// _docsize, _idx).
	//
	// On long-running daily DBs this reclaims roughly half the FTS5
	// disk footprint immediately (per the estimates in #87 / #106).
	// Fresh DBs already skip legacy creation in the updated baseline
	// schema; this migration handles existing v9–v11 DBs.
	`DROP TRIGGER IF EXISTS sym_fts_insert;
	 DROP TRIGGER IF EXISTS sym_fts_delete;
	 DROP TRIGGER IF EXISTS sym_fts_update;
	 DROP TABLE IF EXISTS symbols_fts;`,

	// v12 → v13: route HTML to the docs corpus alongside Markdown
	// (#100 — HTML extractor). Existing v12 DBs already created the
	// per-corpus triggers with `language = 'Markdown'` for docs and
	// `language NOT IN ('Markdown', 'YAML', 'JSON', 'HCL', 'TOML')`
	// for code; an HTML symbol on those DBs would route to code,
	// which is wrong. This migration drops + recreates the three
	// corpus triggers with HTML in both predicates. The vtabs
	// themselves are unchanged.
	//
	// Existing HTML symbols (if any — none indexed yet pre-#100) are
	// re-routed via the same fully-qualified rebuild that
	// `pincher rebuild-fts` performs: the post-migration trigger
	// pulls them into the right vtab on the next index pass. No
	// inline backfill needed since fresh installs hit the updated
	// baseline schema directly.
	`DROP TRIGGER IF EXISTS sym_fts_corpus_insert;
	 DROP TRIGGER IF EXISTS sym_fts_corpus_delete;
	 DROP TRIGGER IF EXISTS sym_fts_corpus_update;

CREATE TRIGGER sym_fts_corpus_insert AFTER INSERT ON symbols BEGIN
    INSERT INTO symbols_code_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind = 'Document' OR new.language IN ('Markdown', 'HTML');
END;

CREATE TRIGGER sym_fts_corpus_delete AFTER DELETE ON symbols BEGIN
    INSERT INTO symbols_code_fts(symbols_code_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(symbols_config_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(symbols_docs_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind = 'Document' OR old.language IN ('Markdown', 'HTML');
END;

CREATE TRIGGER sym_fts_corpus_update AFTER UPDATE ON symbols BEGIN
    INSERT INTO symbols_code_fts(symbols_code_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(symbols_config_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(symbols_docs_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind = 'Document' OR old.language IN ('Markdown', 'HTML');

    INSERT INTO symbols_code_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind = 'Document' OR new.language IN ('Markdown', 'HTML');
END;`,

	// v13 → v14: route XML to the config corpus alongside YAML/JSON/HCL/TOML
	// (#101 — XML extractor). Existing v13 DBs have config-corpus triggers
	// matching only `('YAML', 'JSON', 'HCL', 'TOML')`; an XML symbol on
	// those DBs would fall through to the code corpus, which is wrong.
	// This drops + recreates the three corpus triggers with XML in both
	// the config-include predicate and the code-exclude predicate. The
	// vtabs themselves are unchanged.
	`DROP TRIGGER IF EXISTS sym_fts_corpus_insert;
	 DROP TRIGGER IF EXISTS sym_fts_corpus_delete;
	 DROP TRIGGER IF EXISTS sym_fts_corpus_update;

CREATE TRIGGER sym_fts_corpus_insert AFTER INSERT ON symbols BEGIN
    INSERT INTO symbols_code_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind = 'Document' OR new.language IN ('Markdown', 'HTML');
END;

CREATE TRIGGER sym_fts_corpus_delete AFTER DELETE ON symbols BEGIN
    INSERT INTO symbols_code_fts(symbols_code_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(symbols_config_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(symbols_docs_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind = 'Document' OR old.language IN ('Markdown', 'HTML');
END;

CREATE TRIGGER sym_fts_corpus_update AFTER UPDATE ON symbols BEGIN
    INSERT INTO symbols_code_fts(symbols_code_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(symbols_config_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(symbols_docs_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind = 'Document' OR old.language IN ('Markdown', 'HTML');

    INSERT INTO symbols_code_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind = 'Document' OR new.language IN ('Markdown', 'HTML');
END;`,

	// v14 → v15: add projects.schema_version_at_index (#236). Surfaces
	// "this project was last indexed against schema vN" so pincher list
	// / doctor can flag projects that predate later extractor or
	// migration work and would benefit from a re-index. Existing rows
	// get NULL (no backfill — we genuinely don't know what version
	// indexed them); UpsertProject stamps the column on every future
	// upsert so re-indexing populates it for real users naturally.
	`ALTER TABLE projects ADD COLUMN schema_version_at_index INTEGER`,

	// v15 → v16: per-language call counts on sessions (#240, reported
	// by nbarari). JSON-encoded language→count map serialized on every
	// session flush. NULL on rows that pre-date the column. Lets
	// `pincher stats` surface "agent did 0 Markdown calls in a 2-hour
	// doc-rewrite session" — bypass detection that closes the
	// adoption-priming feedback loop.
	`ALTER TABLE sessions ADD COLUMN calls_by_language TEXT`,

	// v16 → v17: query-failure / retry-rate counters on sessions (#241).
	// queries_total counts every query-shaped tool call (search, query,
	// trace, neighborhood). queries_zero_result counts those that
	// returned 0 results. queries_retried_succeeded counts the cases
	// where a zero-result call was followed by the same tool with
	// equivalent args (modulo confidence/limit knobs) returning ≥1
	// result — i.e. the agent learned, retried, and recovered.
	// tokens_burned_on_failures sums tokens_used across the zero-result
	// calls. Surfaced in `pincher stats` (#1494 renamed the JSON field
	// + human label "Retry rate" → "Zero-result rate" — the original
	// naming was metric-honesty-broken, the value is
	// queries_zero_result/queries_total which is dominated by audit-
	// shape queries on healthy codebases). Users can see the rate and
	// act — lower default min_confidence in CLAUDE.md, or file an
	// extractor issue — instead of paying retry tokens forever without
	// aggregate visibility.
	`ALTER TABLE sessions ADD COLUMN queries_total            INTEGER NOT NULL DEFAULT 0;
	 ALTER TABLE sessions ADD COLUMN queries_zero_result      INTEGER NOT NULL DEFAULT 0;
	 ALTER TABLE sessions ADD COLUMN queries_retried_succeeded INTEGER NOT NULL DEFAULT 0;
	 ALTER TABLE sessions ADD COLUMN tokens_burned_on_failures INTEGER NOT NULL DEFAULT 0`,

	// v17 → v18: capture the binary version that produced each
	// project's index data (#304). When the running server version
	// differs from the project's stored version, the CALLS edges
	// (and other resolution-dependent fields) may reflect older
	// rules — e.g. data indexed before #285 lacks receiver-method
	// resolution. health surfaces this as a re-index recommendation
	// so trace doesn't silently return wrong "0 callers" results.
	// Empty string on rows that pre-date this migration; rendered
	// as "indexed by unknown binary version".
	`ALTER TABLE projects ADD COLUMN binary_version TEXT NOT NULL DEFAULT ''`,

	// v18 → v19: pending_edges — persisted per-file deferred edge
	// candidates (#457). The indexer's CALLS / IMPORTS / READS / WRITES
	// passes resolve cross-file by accumulating "from this file's
	// extraction we saw a reference to NAME" rows and matching them
	// against the symbol table once every file has been processed.
	// Pre-v19, the accumulator was an in-memory slice scoped to a
	// single Index() call — so watcher-driven incremental runs that
	// only re-extracted CHANGED files had no candidates from skipped
	// files, and edges from skipped files to changed files vanished
	// (#427).
	//
	// Persisting per-file: the indexer DELETEs rows for a file before
	// re-extracting it, then INSERTs the new candidates. Skipped (hash-
	// matched) files keep their existing rows. At resolve time, the
	// resolver SELECTs ALL rows for the project — so re-resolution
	// operates on the FULL corpus of candidates, not just this run's.
	//
	// UNIQUE (project_id, from_file, kind, from_qn, to_name) lets us
	// INSERT OR IGNORE without ever growing duplicate rows. Confidence
	// is preserved per-candidate so resolveCalls can weight CALLS
	// (0.7) above READS (0.5) below the resolution path.
	`CREATE TABLE IF NOT EXISTS pending_edges (
		project_id  TEXT    NOT NULL,
		from_file   TEXT    NOT NULL,
		kind        TEXT    NOT NULL,
		from_qn     TEXT    NOT NULL,
		to_name     TEXT    NOT NULL,
		confidence  REAL    NOT NULL DEFAULT 1.0,
		UNIQUE(project_id, from_file, kind, from_qn, to_name)
	);
	CREATE INDEX IF NOT EXISTS idx_pending_edges_project_kind ON pending_edges(project_id, kind);
	CREATE INDEX IF NOT EXISTS idx_pending_edges_from_file ON pending_edges(project_id, from_file);`,

	// v19 → v20: edges.source — tag each row with its origin so the
	// indexer can atomically replace resolve-pass output without
	// nuking per-file edges (#475). Two values:
	//   - 'per_file'     — written by the per-file extractor goroutine
	//                      (both endpoints resolved in nameToID).
	//                      Cascade-deleted when the source file gets
	//                      DeleteSymbolsForFile'd.
	//   - 'resolve_pass' — written by resolveCalls/Imports/Reads at
	//                      the tail of Index(). DELETE'd in bulk
	//                      before each resolve pass and re-INSERTed
	//                      fresh; current rules ALWAYS win.
	//
	// Existing rows default to 'per_file' on migration. This means
	// pre-v20 stale resolve-pass edges (e.g. #465 polymorphic-method
	// leak) aren't auto-cleaned by this migration alone — recommended
	// migration is one final `pincher index <path> --force` after
	// upgrading to v0.18. Future rule changes converge automatically
	// thereafter.
	`ALTER TABLE edges ADD COLUMN source TEXT NOT NULL DEFAULT 'per_file';
	CREATE INDEX IF NOT EXISTS idx_edges_source ON edges(project_id, kind, source);`,

	// v20 → v21: celebrations — one-shot record of cumulative
	// tokens_saved milestones (#494). Each threshold (100k, 500k,
	// 1M, 5M, 10M, 50M, 100M, 500M, 1B) fires exactly once per
	// installation, ever. PRIMARY KEY on threshold_tokens is the
	// one-shot guarantee — INSERT OR IGNORE on race. Tracked
	// globally (not per-project) because the sessions table is
	// project-agnostic and "you've saved a million tokens" reads
	// better than "you've saved a million tokens on this repo".
	`CREATE TABLE IF NOT EXISTS celebrations (
		threshold_tokens INTEGER NOT NULL PRIMARY KEY,
		fired_at         INTEGER NOT NULL
	)`,

	// v21 → v22: receiver-type tracking for Go method calls (#423 piece 2).
	//
	// Two persistence channels for the resolver to use in piece 3:
	//
	//   1. pending_edges.receiver_type — when a CALLS candidate was
	//      extracted from inside a Go method body, this is the method's
	//      receiver type expression (e.g. "*Supervisor"). Empty for
	//      plain functions and non-Go languages, which keeps every
	//      pre-#423 row valid under the NOT NULL DEFAULT.
	//
	//   2. struct_fields — for each Go struct symbol, one row per
	//      field. The resolver consults this to follow recv.field.method
	//      calls: look up the receiver's struct, find the field, get its
	//      type, resolve the method on that type. Without this table the
	//      resolver has no way to know what type "s.spawnFn" is, and
	//      methods like Supervisor.spawnFn target's type get flagged
	//      dead by dead_code (the #493 / #423 root cause).
	//
	// PRIMARY KEY (project_id, struct_id, field_name) lets DeleteSymbolsForFile-
	// shaped writes use INSERT OR REPLACE without growing duplicate
	// rows on re-extraction. The (project_id, field_name) index supports
	// reverse lookup ("find every struct with a field named X") which
	// the resolver doesn't need today but is cheap and probably useful.
	`ALTER TABLE pending_edges ADD COLUMN receiver_type TEXT NOT NULL DEFAULT '';
	CREATE TABLE IF NOT EXISTS struct_fields (
		project_id TEXT NOT NULL,
		struct_id  TEXT NOT NULL,
		field_name TEXT NOT NULL,
		field_type TEXT NOT NULL,
		PRIMARY KEY (project_id, struct_id, field_name)
	);
	CREATE INDEX IF NOT EXISTS idx_struct_fields_proj_name ON struct_fields(project_id, field_name);`,

	// v22 → v23: interface_methods table — interface method names
	// per Interface symbol (#493). The dead_code query joins
	// against this table to exclude project-internal methods whose
	// name matches any interface method name in the same project.
	// Cheap heuristic: name-match only, no full method-set
	// comparison. Trade-off: over-includes (a Method named String
	// gets spared even if no real interface uses it) — but the
	// dead_code direction prefers false-negatives (miss a dead
	// method) over false-positives (suggest deletion of a method
	// that's actually called via interface dispatch and would
	// silently break runtime).
	`CREATE TABLE IF NOT EXISTS interface_methods (
		project_id   TEXT NOT NULL,
		interface_id TEXT NOT NULL,
		method_name  TEXT NOT NULL,
		PRIMARY KEY (project_id, interface_id, method_name)
	);
	CREATE INDEX IF NOT EXISTS idx_iface_methods_proj_name ON interface_methods(project_id, method_name);`,

	// v23 → v24: hook_invocations telemetry (#626). Logs every
	// `pincher hook-check` invocation: the proposed tool call, the
	// hook's decision (pass-through vs redirect), and — after a
	// post-hoc joiner walks the session's subsequent tool calls —
	// whether the agent took the recommendation within 3 calls.
	// Conversion rate (`taken / redirects`) is the v0.37 headline.
	//
	// session_id is nullable because hook-check may run outside an
	// MCP session (e.g. when wired into a CLI-only workflow). Values
	// are local to the user's pincher.db; nothing phones home.
	`CREATE TABLE IF NOT EXISTS hook_invocations (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		ts                  INTEGER NOT NULL,
		session_id          TEXT,
		tool_name           TEXT NOT NULL,
		file_path           TEXT,
		file_bytes          INTEGER,
		decision            TEXT NOT NULL,
		suggested_tool      TEXT,
		suggested_args      TEXT,
		next_tool_within_3  TEXT,
		took_recommendation INTEGER
	);
	CREATE INDEX IF NOT EXISTS idx_hook_session ON hook_invocations(session_id);
	CREATE INDEX IF NOT EXISTS idx_hook_ts ON hook_invocations(ts);
	CREATE INDEX IF NOT EXISTS idx_hook_pending_join ON hook_invocations(took_recommendation, ts) WHERE took_recommendation IS NULL;`,

	// v24 → v25: closure table — materialized transitive closure of the
	// edges graph for fast trace queries (#652 phase 1, #403 design). When
	// PINCHER_CLOSURE_TABLES=1 is set at index time, the indexer's tail-pass
	// builds (from_id, to_id, depth, project_id) tuples up to a configurable
	// max-depth (default 3, override via PINCHER_CLOSURE_MAX_DEPTH). Trace
	// queries then become a single indexed SELECT instead of an N-deep
	// recursive CTE — millisecond instead of 5–50 ms on a project with
	// thousands of edges.
	//
	// Storage cost: per #639 measurement, 50k edges → 50k–60k closure rows
	// at depth=3 (~10 MB), 100k–120k at depth=5 (~25 MB). 10k-file repo
	// linear extrapolation: ~325 MB at depth=3 — comfortably under the
	// 500 MB budget for v0.54 phase 1. depth=5 is opt-in only.
	//
	// PRIMARY KEY (project_id, from_id, to_id) WITHOUT ROWID is the right
	// shape for the typical trace query (`WHERE project_id=? AND from_id=?`)
	// — primary-key lookup beats secondary-index for this access pattern.
	// `depth` is a payload column, not part of the key — the same (from, to)
	// pair through different paths records the MIN depth (insert-or-ignore
	// + ascending BFS guarantees the first insert is the shortest path).
	//
	// Empty by default — even with the schema present, the table stays
	// empty until PINCHER_CLOSURE_TABLES=1 triggers the builder. No cost
	// to existing deployments; opt-in feature behind an env flag for v0.54.
	`CREATE TABLE IF NOT EXISTS closure (
		project_id TEXT NOT NULL REFERENCES projects(id),
		from_id    TEXT NOT NULL,
		to_id      TEXT NOT NULL,
		depth      INTEGER NOT NULL,
		PRIMARY KEY (project_id, from_id, to_id)
	) WITHOUT ROWID;
	CREATE INDEX IF NOT EXISTS idx_closure_to ON closure(project_id, to_id);`,

	// v25 → v26: pending_edges.base_type — for a Go READS candidate
	// extracted from a non-package selector `base.Sel`, this is the
	// declared type of `base` as written, stripped of leading `*` and
	// `[]` (e.g. "ast.ExtractedEdge" for `e.Confidence` where `e`
	// ranges over a `[]ast.ExtractedEdge`). The #565 binding pass
	// (resolveReads) consults it to tell a struct-field read from a
	// function-value reference: if base_type names a project struct
	// with a field of the READS edge's to_name, the read is a field
	// access and the pass must NOT emit a false CALLS edge to a
	// same-named Method (#760 — `e.Confidence` no longer false-binds
	// to `*hclExtractor.Confidence`). Empty for non-selector reads,
	// unresolved base types, non-Go languages, and pre-v26 rows —
	// keeps every existing row valid under the NOT NULL DEFAULT and
	// the binding pass falls back to its pre-#760 heuristic.
	`ALTER TABLE pending_edges ADD COLUMN base_type TEXT NOT NULL DEFAULT '';`,

	// v26 → v27: session_tool_calls — per-call event log feeding the
	// v0.64 dashboard "triangulating panels" (#635). The existing
	// session_stats table aggregates per-session totals; per-call
	// breakdown was never persisted, so the dashboard couldn't
	// compute tool-call entropy, response-size distribution, or
	// per-tier saved-percentage medians without writing each call
	// to disk. This table is the minimum substrate for all three.
	//
	// Schema:
	//   session_id        FK to sessions.session_id (text, indexed)
	//   tool              MCP tool name (search/symbol/...)
	//   complexity_tier   lite/standard/heavy from toolComplexityTiers
	//   response_bytes    marshalled JSON size (excluding _meta)
	//   tokens_used       db.ApproxTokens(response_body)
	//   tokens_saved      computed savings vs baseline_method
	//   tokens_saved_pct  saved / (saved + used) * 100, rounded
	//   ts                UnixNano of call completion
	//   request_id        UUID stamped on _meta.request_id (#657)
	//
	// Indexes: (session_id), (ts), (tool, ts) — supports the three
	// canonical query shapes the dashboard runs (per-session
	// entropy, trailing-7d window scans, per-tool aggregation).
	//
	// Retention: not gated here. The dashboard panels filter on a
	// trailing window (typically 7d); a separate housekeeping pass
	// in a later release can prune. Pre-1.0 with low call volume
	// (single-user SQLite), table growth is bounded by usage so
	// "delete rows older than 30d on session-start" is the
	// expected v0.65+ follow-up.
	`
	CREATE TABLE IF NOT EXISTS session_tool_calls (
		session_id        TEXT    NOT NULL,
		tool              TEXT    NOT NULL,
		complexity_tier   TEXT    NOT NULL DEFAULT '',
		response_bytes    INTEGER NOT NULL DEFAULT 0,
		tokens_used       INTEGER NOT NULL DEFAULT 0,
		tokens_saved      INTEGER,
		tokens_saved_pct  REAL,
		ts                INTEGER NOT NULL,
		request_id        TEXT    NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_session_tool_calls_session ON session_tool_calls(session_id);
	CREATE INDEX IF NOT EXISTS idx_session_tool_calls_ts ON session_tool_calls(ts);
	CREATE INDEX IF NOT EXISTS idx_session_tool_calls_tool_ts ON session_tool_calls(tool, ts);
	`,

	// v27 → v28: composite PRIMARY KEY (project_id, id) on symbols.
	//
	// #1231 ROOT CAUSE: pre-v28 the bare id was the symbols PK.
	// MakeSymbolID returns "{file_path}::{qualified_name}#{kind}" with
	// no project scope, so two projects sharing the same relative file
	// path with the same qualified-name+kind produced colliding rows.
	// INSERT OR REPLACE silently flipped the row's project_id to the
	// latest writer. Live shape: pincher-repo's server.go showed 8 of
	// 75 Methods because sniffer (an older pincher mirror) also
	// indexed internal/server/server.go and clobbered the 67
	// pre-existing rows.
	//
	// SQLite can't ALTER PRIMARY KEY in place, so the migration
	// rebuilds the table:
	//   1. Drop FTS5 triggers (they reference `symbols` directly).
	//      The vtabs themselves stay (they're independent storage);
	//      the triggers get re-created from the same ftsCorpusSplitDDL
	//      that produced them.
	//   2. Create symbols_new with composite PK (project_id, id).
	//   3. INSERT INTO symbols_new SELECT * FROM symbols. Cross-
	//      project ID collisions in the pre-v28 DB are already
	//      irrecoverable (one project's row WAS overwritten); the
	//      migration just preserves whatever's there. ON CONFLICT IGNORE
	//      is defensive against any composite-PK violation discovered
	//      post-collision-cleanup; in practice the source rows are
	//      already (id, project_id)-unique because the bare-id PK
	//      enforced uniqueness on id alone.
	//   4. DROP + RENAME.
	//   5. Recreate indexes including the new idx_sym_id for bare-id
	//      lookup (most byte-offset retrieval paths pass id without
	//      project scope; without this index the composite-PK lookup
	//      degrades to a full scan).
	//   6. Recreate FTS5 triggers via the canonical ftsCorpusSplitDDL
	//      body — same source of truth the baseline schema uses.
	//
	// JOIN sites in db.go / cypher/engine.go are updated in the same
	// PR to add `AND symbols.project_id = edges.project_id` so cross-
	// project edge traversal can't surface a different project's
	// symbol row even when ids collide. Schema is the structural
	// guard; the JOIN updates are the query-time guard.
	v28RebuildSymbolsCompositePK + ftsCorpusSplitDDL,

	// v28 → v29: bench_runs + bench_results tables for `pincher bench
	// --persist`. Persists per-run summary + per-tool aggregates so the
	// dashboard can render "predicted vs actual" over time for any
	// project pincher bench has ever run against (per user mid-session
	// ask, #1263 follow-up: "having the results pop up on the http would
	// probably be good. then long term you keep estimated results with
	// actual result for any project that pincher bench ran on").
	//
	// Cascade delete: when a project is removed via `pincher project rm`,
	// its bench history goes too — bench results are pinned to the
	// project's index at a point in time and are meaningless after the
	// project is gone.
	`
	CREATE TABLE IF NOT EXISTS bench_runs (
		run_id          TEXT PRIMARY KEY,
		project_id      TEXT NOT NULL,
		started_at      DATETIME NOT NULL,
		n_samples       INTEGER NOT NULL,
		trace_depth     INTEGER NOT NULL,
		binary_version  TEXT NOT NULL DEFAULT '',
		FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_bench_runs_project_started
		ON bench_runs(project_id, started_at DESC);

	CREATE TABLE IF NOT EXISTS bench_results (
		run_id              TEXT NOT NULL,
		tool_name           TEXT NOT NULL,
		calls               INTEGER NOT NULL,
		p50_latency_ms      REAL NOT NULL,
		p95_latency_ms      REAL NOT NULL,
		mean_latency_ms     REAL NOT NULL,
		mean_tokens_actual  INTEGER NOT NULL,
		mean_tokens_baseline INTEGER NOT NULL,
		savings_pct         REAL NOT NULL,
		PRIMARY KEY (run_id, tool_name),
		FOREIGN KEY (run_id) REFERENCES bench_runs(run_id) ON DELETE CASCADE
	);
	`,

	// v29 → v30: closure.via_kind for #685 phase 2 — record the last-hop
	// edge kind on every closure row so the fast-path trace can populate
	// the Via field that's been empty since v0.54 phase 1. Existing rows
	// get '' which preserves the phase-1 behaviour for any pre-migration
	// closure data that hasn't been rebuilt yet. BuildClosure now also
	// filters source edges to the default trace kind set ({CALLS,
	// HTTP_CALLS, ASYNC_CALLS}) so the closure data semantically matches
	// what the CTE returns under the trace tool's default kinds filter —
	// pre-fix the closure traversed ALL edge kinds + the trace fast-path
	// returned a superset that disagreed with the CTE path (#1162
	// measurement caught this).
	`ALTER TABLE closure ADD COLUMN via_kind TEXT NOT NULL DEFAULT ''`,

	// v30 → v31: branch column on symbols / edges / files / pending_edges
	// for the branch-aware index invalidation work (#1303 Phase 1).
	// Pre-fix the index was single-branch — switching branches required
	// a full re-index, and the post-checkout hook from #1261 §1 fired
	// unconditionally even for same-branch fresh checkouts.
	//
	// Phase 1 (this migration): add the column with DEFAULT '' so every
	// existing row carries the empty-branch sentinel meaning "indexed
	// before branch awareness landed; treat as current branch for back-
	// compat queries." The indexer doesn't yet stamp branch — that's
	// Phase 2's work. The column existing now means subsequent re-
	// indexes can start populating it without another schema migration.
	//
	// PRIMARY KEY / UNIQUE constraints intentionally NOT widened in
	// this migration. Multi-branch coexistence (Phase 2) requires
	// rebuilding the symbols / edges / files tables with branch in the
	// composite key, plus rebuilding FTS5 vtabs that depend on the
	// symbols rowids. That's the v28-PK-rebuild shape and is its own
	// PR per the issue's design choice. Same-branch-overwrite stays
	// the semantic until Phase 2.
	`
	ALTER TABLE symbols       ADD COLUMN branch TEXT NOT NULL DEFAULT '';
	ALTER TABLE edges         ADD COLUMN branch TEXT NOT NULL DEFAULT '';
	ALTER TABLE files         ADD COLUMN branch TEXT NOT NULL DEFAULT '';
	ALTER TABLE pending_edges ADD COLUMN branch TEXT NOT NULL DEFAULT '';
	`,

	// v31 → v32: projects.current_branch — the git branch the project
	// was last indexed on. Captured via `git rev-parse --abbrev-ref HEAD`
	// at index time. Empty when the project root isn't a git working
	// tree or HEAD is detached (the indexer falls back to the commit SHA
	// in that case — see indexer.detectGitBranch).
	//
	// Purpose: enables a doctor advisory that fires when the user's
	// currently-checked-out branch differs from the last-indexed branch,
	// surfacing the stale-index condition that's silently bitten users
	// who switch branches between sessions (#1303 root cause).
	//
	// Pre-existing rows default to '' meaning "indexed before branch
	// awareness landed; the next re-index will populate it." The
	// `pincher index` flow stamps this column unconditionally on
	// completion so the advisory becomes reliable after one re-index.
	//
	// PK widening on symbols/edges/files (Phase 2b — true multi-branch
	// coexistence) intentionally deferred to a follow-up PR. That
	// migration requires the v28-style table rebuild + FTS5 vtab
	// recreate + coordinated query-layer changes; this PR is the safer
	// stamping-only slice that proves out branch detection on real
	// pre-release dogfooding before compounding the schema risk.
	`ALTER TABLE projects ADD COLUMN current_branch TEXT NOT NULL DEFAULT '';`,

	// v32 → v33: extraction_failures.binary_version_at_failure —
	// the pincher binary version that produced the failure. Lets
	// doctor distinguish stale failures (extracted by an older
	// binary that has since shipped the fix) from recurring
	// failures (current binary still fails on these files).
	//
	// Purpose: with thousands of extraction_failures rows on a
	// large multi-project install, the agent can't tell which
	// rows are actionable. Pre-fix every row required cross-
	// referencing with the project's `binary_version` in list/
	// doctor to guess the originating binary — and truncation
	// meant most rows didn't even appear. With this column the
	// row carries its own provenance.
	//
	// NULL = pre-migration row (recorded before binary tracking
	// landed). RecordExtractionFailureWithBinary populates the
	// column on every new write; the legacy
	// RecordExtractionFailure stays as a thin wrapper that
	// passes "" so callers without binary context keep working.
	//
	// Future enhancement (#1421 bonus): a `fresh_only` doctor
	// filter that drops rows whose binary_version_at_failure
	// differs from the project's current binary_version — the
	// "this binary still produces these failures" slice that's
	// what operators usually want to act on.
	`ALTER TABLE extraction_failures ADD COLUMN binary_version_at_failure TEXT NOT NULL DEFAULT '';`,

	// v33 → v34: split queries_zero_result into expected (audit-shape)
	// vs unexpected (caller-surprised) sub-counters. The original
	// queries_zero_result column stays as the durable sum so existing
	// readers and dashboards keep working without changes; the two new
	// columns let `pincher stats` surface the actionable rate.
	//
	// Background: #1494 renamed the misleading `retry_rate` JSON field
	// to `zero_result_rate` (queries_zero_result / queries_total) but
	// flagged that the value conflates two populations:
	//
	//   - Expected zero (audit-shape): query tool with a property
	//     predicate ({is_documented:false}, {kind:'Function',is_used:0})
	//     — empty rows is a healthy-codebase signal, not a friction
	//     event.
	//   - Unexpected zero (caller-surprised): search for a symbol
	//     expecting >=1 match, trace for callers, neighborhood lookup
	//     by id — empty rows is a usage-killer. The agent rarely
	//     refines (queries_retried_succeeded is <1% of zero-results
	//     in dogfood data) so each unexpected zero is a near-permanent
	//     loss of downstream pincher usage for that call.
	//
	// Mixing them masks the actionable rate. With the split, the
	// new headline metric is zero_unexpected_rate
	// (queries_zero_unexpected / queries_total) — that's the dial
	// that translates "pincher gets called" into "pincher gets called
	// the SECOND time". Closes #1494 half 1 / #1632.
	`ALTER TABLE sessions ADD COLUMN queries_zero_expected   INTEGER NOT NULL DEFAULT 0;
	 ALTER TABLE sessions ADD COLUMN queries_zero_unexpected INTEGER NOT NULL DEFAULT 0;`,
}

// schemaMigrationInvalidates classifies each migration in schemaMigrations
// by what previously-extracted data the migration makes stale (#1497).
// Index i refers to the (i+1) → (i+2) migration — the same indexing as
// schemaMigrations. Slices must stay the same length; init() guards.
//
// Classification rationale (audit 2026-05-18):
//
//   - **Nothing** (22 entries): migrations that touch only sessions,
//     diagnostics, metadata, or pure DDL operations whose effects are
//     evident on existing data without re-extraction. New tables with
//     trigger-driven backfill, virtual generated columns, dropped
//     legacy indexes / vtabs, ALTER TABLE on sessions / projects
//     metadata, corpus-routing trigger updates for languages that
//     had no rows pre-migration.
//
//   - **All** (5 entries): migrations whose data shape requires
//     re-extraction:
//       v18→v19 (pending_edges table) — cross-file edge resolution
//         model switched from in-memory to persisted. Pre-v19 projects
//         have empty pending_edges; resolveCalls/Imports/Reads operate
//         on the empty pool until a re-extract repopulates.
//       v19→v20 (edges.source) — migration comment explicitly says
//         "recommended migration is one final `pincher index --force`
//         after upgrading to v0.18."
//       v21→v22 (pending_edges.receiver_type + struct_fields) — the
//         #423/#493 receiver-type resolver depends on these being
//         populated by re-extraction of Go files.
//       v25→v26 (pending_edges.base_type) — the #565 binding pass
//         (resolveReads) consults it to tell a struct-field read
//         from a function-value reference.
//       v30→v31 (branch column on symbols/edges/files/pending_edges)
//         — queries that filter by branch miss pre-migration rows
//         whose branch field is the empty-sentinel default.
//
// The classification feeds doctor visibility today; future work
// (#1497 follow-up) gates binaryDriftForce on the union of
// invalidates across applied migrations.
var schemaMigrationInvalidates = []MigrationInvalidates{
	// NOTE on indexing: a few of the "v→v" headers in the schemaMigrations
	// comment blocks span MULTIPLE slice entries (e.g. v2→v3's CREATE TABLE
	// + CREATE INDEX are two separate strings). The migrate() loop bumps
	// the schema_version by 1 per slice element, so the actual v→v step
	// each entry effects is `index+1 → index+2`. The labels here use the
	// actual step, not the comment-block grouping.
	invalidatesNothing, // [ 0] v1→v2:  symbols.extraction_confidence column DEFAULT 1.0 (over-counts old rows under min_confidence filter; acceptable until natural reindex)
	invalidatesNothing, // [ 1] v2→v3:  CREATE TABLE symbol_moves (new table; populated only by future move events)
	invalidatesNothing, // [ 2] v3→v4:  CREATE INDEX idx_sym_qnkind (pure index; SQLite builds from existing rows)
	invalidatesNothing, // [ 3] v4→v5:  CREATE TABLE sessions (per-session ROI metrics)
	invalidatesNothing, // [ 4] v5→v6:  symbols.symbol_id VIRTUAL GENERATED column (computed at read, zero storage)
	invalidatesNothing, // [ 5] v6→v7:  CREATE TABLE extraction_failures (diagnostic surface only)
	invalidatesNothing, // [ 6] v7→v8:  CREATE TABLE slow_queries (diagnostic surface only)
	invalidatesNothing, // [ 7] v8→v9:  per-corpus FTS5 split with backfill (backfill copies existing rows into new vtabs)
	invalidatesNothing, // [ 8] v9→v10: TOML in config-corpus triggers (no TOML extractor pre-v10, no rows to re-route)
	invalidatesNothing, // [ 9] v10→v11: sessions.http_url + sessions.http_pid (HTTP discovery metadata)
	invalidatesNothing, // [10] v11→v12: DROP legacy symbols_fts (removes unused vtab; per-corpus vtabs unaffected)
	invalidatesNothing, // [11] v12→v13: HTML to docs-corpus triggers (no HTML symbols pre-#100)
	invalidatesNothing, // [12] v13→v14: XML to config-corpus triggers (no XML symbols pre-#101)
	invalidatesNothing, // [13] v14→v15: projects.schema_version_at_index (metadata only)
	invalidatesNothing, // [14] v15→v16: sessions.calls_by_language (per-session bypass-detection metric)
	invalidatesNothing, // [15] v16→v17: sessions retry-rate counters (per-session metrics)
	invalidatesNothing, // [16] v17→v18: projects.binary_version (metadata, stamped on next index)
	invalidatesAll,     // [17] v18→v19: pending_edges table (NEW cross-file resolver model; pre-v19 data flows through old path but new resolutions need this table populated per file)
	invalidatesAll,     // [18] v19→v20: edges.source DEFAULT 'per_file' (migration comment explicitly recommends `pincher index --force` after upgrade)
	invalidatesNothing, // [19] v20→v21: celebrations table (one-shot milestone tracker, global)
	invalidatesAll,     // [20] v21→v22: pending_edges.receiver_type + struct_fields table (Go method-call resolver needs these populated by re-extraction; #423/#493 root cause)
	invalidatesNothing, // [21] v22→v23: interface_methods table (NEW table, empty until next reindex; dead_code's behaviour without the table populated is identical to pre-v22, so no regression on old projects — methods just stay un-excluded as they were before)
	invalidatesNothing, // [22] v23→v24: hook_invocations table (telemetry only)
	invalidatesNothing, // [23] v24→v25: closure table (opt-in via PINCHER_CLOSURE_TABLES env, empty by default)
	invalidatesAll,     // [24] v25→v26: pending_edges.base_type (Go READS resolver consults this — #565 binding pass)
	invalidatesNothing, // [25] v26→v27: session_tool_calls table (per-call event log for dashboard)
	invalidatesNothing, // [26] v27→v28: composite PRIMARY KEY (project_id, id) on symbols (table rebuild preserves rows in-place; no data shape change for queries)
	invalidatesNothing, // [27] v28→v29: bench_runs + bench_results tables (bench history, unrelated to extraction)
	invalidatesNothing, // [28] v29→v30: closure.via_kind (closure is opt-in via env, defaults to '')
	invalidatesAll,     // [29] v30→v31: branch column on symbols/edges/files/pending_edges (queries that filter by branch miss pre-migration rows)
	invalidatesNothing, // [30] v31→v32: projects.current_branch (metadata, stamped on next index)
	invalidatesNothing, // [31] v32→v33: extraction_failures.binary_version_at_failure (metadata on diagnostic table)
	invalidatesNothing, // [32] v33→v34: sessions.queries_zero_expected + queries_zero_unexpected (per-session metric split; pre-migration rows hold zero on both)
}

func init() {
	if len(schemaMigrations) != len(schemaMigrationInvalidates) {
		panic(fmt.Sprintf(
			"schemaMigrations and schemaMigrationInvalidates length mismatch: %d vs %d — "+
				"adding a new migration requires updating both slices",
			len(schemaMigrations), len(schemaMigrationInvalidates),
		))
	}
}

// v28RebuildSymbolsCompositePK is the SQL portion of v28 that drops
// FTS5 triggers + vtabs, rebuilds the symbols table with a composite
// PRIMARY KEY (project_id, id), recreates indexes, then leaves the
// FTS5 vtabs + triggers + populate-from-symbols work to a concat of
// ftsCorpusSplitDDL (the canonical source of truth — see line ~1854).
// Splitting it this way means the FTS5 setup never drifts from the
// migrate-on-fresh-DB path: both run the same DDL.
const v28RebuildSymbolsCompositePK = `
	DROP TRIGGER IF EXISTS sym_fts_corpus_insert;
	DROP TRIGGER IF EXISTS sym_fts_corpus_delete;
	DROP TRIGGER IF EXISTS sym_fts_corpus_update;
	-- Drop FTS5 vtabs entirely. Pre-v28 entries reference the old
	-- symbols table's rowids; after the rebuild those rowids no
	-- longer match. ftsCorpusSplitDDL (appended) recreates fresh
	-- vtabs in external-content mode tied to the new symbols table.
	DROP TABLE IF EXISTS symbols_code_fts;
	DROP TABLE IF EXISTS symbols_config_fts;
	DROP TABLE IF EXISTS symbols_docs_fts;

	CREATE TABLE symbols_new (
		id             TEXT    NOT NULL,
		project_id     TEXT    NOT NULL REFERENCES projects(id),
		file_path      TEXT    NOT NULL,
		name           TEXT    NOT NULL,
		qualified_name TEXT    NOT NULL,
		kind           TEXT    NOT NULL,
		language       TEXT    NOT NULL,
		start_byte     INTEGER NOT NULL,
		end_byte       INTEGER NOT NULL,
		start_line     INTEGER NOT NULL,
		end_line       INTEGER NOT NULL,
		signature      TEXT,
		return_type    TEXT,
		docstring      TEXT,
		parent         TEXT,
		complexity     INTEGER DEFAULT 0,
		is_exported    INTEGER DEFAULT 0,
		is_test        INTEGER DEFAULT 0,
		is_entry_point INTEGER DEFAULT 0,
		file_hash      TEXT,
		extraction_confidence REAL NOT NULL DEFAULT 1.0,
		symbol_id      TEXT GENERATED ALWAYS AS (id) VIRTUAL,
		PRIMARY KEY (project_id, id)
	);

	INSERT OR IGNORE INTO symbols_new (
		id, project_id, file_path, name, qualified_name, kind, language,
		start_byte, end_byte, start_line, end_line,
		signature, return_type, docstring, parent,
		complexity, is_exported, is_test, is_entry_point, file_hash,
		extraction_confidence
	)
	SELECT
		id, project_id, file_path, name, qualified_name, kind, language,
		start_byte, end_byte, start_line, end_line,
		signature, return_type, docstring, parent,
		complexity, is_exported, is_test, is_entry_point, file_hash,
		extraction_confidence
	FROM symbols;

	DROP TABLE symbols;
	ALTER TABLE symbols_new RENAME TO symbols;

	CREATE INDEX IF NOT EXISTS idx_sym_project ON symbols(project_id);
	CREATE INDEX IF NOT EXISTS idx_sym_id      ON symbols(id);
	CREATE INDEX IF NOT EXISTS idx_sym_file    ON symbols(project_id, file_path);
	CREATE INDEX IF NOT EXISTS idx_sym_kind    ON symbols(project_id, kind);
	CREATE INDEX IF NOT EXISTS idx_sym_name    ON symbols(project_id, name);
	CREATE INDEX IF NOT EXISTS idx_sym_qn      ON symbols(project_id, qualified_name);
	CREATE INDEX IF NOT EXISTS idx_sym_qnkind  ON symbols(project_id, qualified_name, kind);
`

// migrate applies the baseline schema then runs any pending numbered migrations.
//
// Versioning: a single-row schema_version table tracks how far migrations have
// been applied. New databases start at v1 (baseline). Existing databases that
// pre-date this system also start at v1 because the baseline schema is fully
// idempotent (IF NOT EXISTS throughout) — running it on an existing database
// is always safe.
func (s *Store) migrate() error {
	// Step 1: apply baseline DDL — safe to run on any existing database.
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("baseline schema: %w", err)
	}

	// Step 2: bootstrap version-tracking table.
	if _, err := s.db.Exec(
		`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`,
	); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	// Insert v1 only if the table is empty (fresh DB or pre-versioning DB).
	// RowsAffected tells us whether this was a brand-new DB so we can seed
	// sqlite_stat1 once at the end of migrate.
	res, err := s.db.Exec(
		`INSERT INTO schema_version(version) SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM schema_version)`,
	)
	if err != nil {
		return fmt.Errorf("init schema_version: %w", err)
	}
	freshDB := false
	if n, raErr := res.RowsAffected(); raErr == nil && n > 0 {
		freshDB = true
	}

	// Step 3: read current version.
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	// Step 3.5: refuse to proceed against a database newer than this binary
	// understands. Without this check an older binary would read/write rows
	// using its older schema knowledge and silently corrupt newer columns.
	// The highest version this binary knows is baseline (v1) + len(migrations).
	maxKnown := len(schemaMigrations) + 1
	if version > maxKnown {
		return fmt.Errorf(
			"pincher database is at schema v%d but this binary only understands up to v%d — upgrade pincher to continue, or restore an older pincher.db from backup",
			version, maxKnown,
		)
	}

	// Step 4: apply any migrations the database hasn't seen yet.
	// Also record the union of invalidates across applied migrations
	// (#1497) so doctor / the future binaryDriftForce gate can decide
	// whether the bump genuinely requires re-extraction or is a pure
	// schema-shape DDL with no impact on previously-extracted data.
	s.lastStartupMigrationInvalidates = invalidatesNothing
	s.lastStartupMigrationsAppliedFrom = version
	s.lastStartupMigrationsAppliedTo = version
	for i := version - 1; i < len(schemaMigrations); i++ {
		if _, err := s.db.Exec(schemaMigrations[i]); err != nil {
			return fmt.Errorf("schema migration v%d→v%d: %w", i+1, i+2, err)
		}
		next := i + 2
		if _, err := s.db.Exec(`UPDATE schema_version SET version = ?`, next); err != nil {
			return fmt.Errorf("bump schema version to %d: %w", next, err)
		}
		inv := schemaMigrationInvalidates[i]
		if inv.All {
			s.lastStartupMigrationInvalidates.All = true
		}
		s.lastStartupMigrationInvalidates.Languages = append(
			s.lastStartupMigrationInvalidates.Languages, inv.Languages...,
		)
		s.lastStartupMigrationsAppliedTo = next
	}

	// Step 4.5: idempotent schema-parity repairs. These run on every Open()
	// and self-skip when the target state is already in place. They exist
	// for DBs that took divergent migration paths and ended up at the
	// current schema_version with structural deviations. See #83.
	if err := s.ensureSymbolIDColumn(); err != nil {
		return fmt.Errorf("symbol_id column repair: %w", err)
	}

	// Step 4.6: dedupe project rows that resolved to the same canonical
	// path. Pre-#84 `ProjectIDFromPath` returned the literal abs path,
	// so case-insensitive filesystems (macOS APFS, Windows NTFS)
	// accumulated duplicates when the user invoked pincher with
	// different casings. Self-skips when no duplicates exist.
	if err := s.dedupProjectsByCanonicalPath(); err != nil {
		return fmt.Errorf("project dedup: %w", err)
	}

	// Step 5: on a brand-new DB, seed sqlite_stat1 with one ANALYZE pass.
	// PRAGMA optimize (Store.Optimize, run after each Index) is a no-op when
	// sqlite_stat1 doesn't exist, so without this seed the planner has no
	// stats for the first few index runs and Cypher queries pick suboptimal
	// plans. Sub-ms on empty tables; non-fatal on error because optimize
	// will eventually populate stats once enough writes accumulate.
	if freshDB {
		_, _ = s.db.Exec(`ANALYZE`)
	}
	return nil
}

// ensureSymbolIDColumn adds the generated `symbol_id` column to the
// `symbols` table if it's missing — addressing #83.
//
// Background: the v6 migration (PR #21, closing #19) added a
// `symbol_id TEXT GENERATED ALWAYS AS (id) VIRTUAL` column on `symbols`
// to satisfy FTS5's content-table column-name lookup for the legacy
// `symbols_fts` vtab (declared with `symbol_id UNINDEXED` first). The
// per-corpus vtabs added in v9 (#75) inherit the same FTS5 first-column
// shape and rely on the same column.
//
// On most DBs the v6 migration ran cleanly and the column exists. But
// some long-running daily DBs took the now-defunct "Option A" path
// (the deleted `fix/db-fts5-column-rename` branch) for the v5→v6 hop,
// which renamed the FTS5 vtab's first column instead of adding the
// generated column on `symbols`. Those DBs reach v9+ with no
// `symbols.symbol_id` column, so any non-MATCH select against the new
// per-corpus vtabs (e.g. `JOIN ON f.symbol_id=s.id`) fails with
// `no such column: T.symbol_id`.
//
// The repair is idempotent: check `pragma_table_xinfo` for the column,
// add it if missing, no-op if present. SQLite doesn't support
// `ADD COLUMN IF NOT EXISTS` natively, so the pragma check is the
// canonical pattern.
//
// We use `pragma_table_xinfo` rather than `pragma_table_info` because
// the former includes hidden / generated columns. Generated VIRTUAL
// columns are filtered from the regular `table_info` output, which
// would cause this repair to "miss" the column on DBs that already
// have it and try to ALTER again — duplicating the column-name
// error this helper exists to avoid.
//
// Generated columns are virtual (computed on read); adding one is a
// metadata-only operation that doesn't rewrite existing rows.
func (s *Store) ensureSymbolIDColumn() error {
	var present int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_xinfo('symbols') WHERE name = 'symbol_id'`,
	).Scan(&present)
	if err != nil {
		return fmt.Errorf("query pragma_table_xinfo: %w", err)
	}
	if present > 0 {
		return nil
	}
	if _, err := s.db.Exec(
		`ALTER TABLE symbols ADD COLUMN symbol_id TEXT GENERATED ALWAYS AS (id) VIRTUAL`,
	); err != nil {
		return fmt.Errorf("add symbol_id column: %w", err)
	}
	return nil
}

// dedupProjectsByCanonicalPath finds project rows that resolve to the
// same canonical path (per ProjectIDFromPath / CanonicalProjectPath)
// and merges duplicates. Closes the post-fix half of #84: prevention
// (canonical project_id at write time) handles new projects, this
// migration cleans up DBs that already accumulated duplicates from
// pre-fix invocations.
//
// Algorithm:
//
//  1. Read every (id, path) from `projects`.
//  2. Group by canonical(path). The grouping key is what
//     ProjectIDFromPath would produce today; rows whose stored `id`
//     differs from that key are non-canonical.
//  3. For each group with len > 1: pick the row whose stored `id`
//     matches the canonical form as the winner. If none match (both
//     non-canonical), pick the row with the highest sym_count, ties
//     broken by most recent indexed_at.
//  4. Re-key each non-winner's symbols/edges/file_hashes/extraction_failures/
//     adrs/symbol_moves to point at the winner's id, dropping rows
//     that would conflict on the winner's existing data.
//  5. Delete the loser project row.
//
// Conflict resolution at re-key time: SQLite primary keys / unique
// indexes prevent UPDATE from creating duplicates. We use
// `INSERT OR IGNORE ... SELECT` then `DELETE FROM <tbl> WHERE
// project_id = loser` to avoid raising errors. This loses some symbols
// from the loser if they collide with winner — those are recoverable
// by re-indexing, which the user will trigger naturally on next call.
//
// Idempotent: running twice is a no-op (no duplicates exist after
// the first run). Self-skips if no duplicates exist (the first SELECT
// returns no groups with cardinality > 1).
//
// Migration runs every Open() — the dedup logic is sub-ms when there
// are no duplicates, since the only work is the initial SELECT and a
// map-build that finds zero collisions.
func (s *Store) dedupProjectsByCanonicalPath() error {
	rows, err := s.db.Query(`SELECT id, path, sym_count, indexed_at FROM projects`)
	if err != nil {
		return fmt.Errorf("scan projects: %w", err)
	}
	type projRow struct {
		id        string
		path      string
		symCount  int
		indexedAt int64
	}
	var all []projRow
	for rows.Next() {
		var p projRow
		if err := rows.Scan(&p.id, &p.path, &p.symCount, &p.indexedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		all = append(all, p)
	}
	rows.Close()

	// Group by canonical(path). The canonical form may not equal any
	// row's stored id (nbarari's reproducer: both /Users/nick/Projects
	// and /Users/nick/projects canonicalize to the lowercased form).
	groups := make(map[string][]projRow)
	for _, p := range all {
		canon := CanonicalProjectPath(p.path)
		groups[canon] = append(groups[canon], p)
	}

	for canon, members := range groups {
		if len(members) <= 1 {
			continue
		}
		// Pick winner: prefer the row whose stored id == canonical form.
		// If none match, pick highest sym_count; ties → most recent.
		winnerIdx := 0
		for i, m := range members {
			if m.id == canon {
				winnerIdx = i
				break
			}
		}
		if members[winnerIdx].id != canon {
			// No row already at canonical form — pick by sym_count + age.
			for i, m := range members {
				w := members[winnerIdx]
				if m.symCount > w.symCount ||
					(m.symCount == w.symCount && m.indexedAt > w.indexedAt) {
					winnerIdx = i
				}
			}
		}
		winner := members[winnerIdx]

		// Re-key losers' data to the winner's id.
		for i, loser := range members {
			if i == winnerIdx {
				continue
			}
			if err := s.mergeProjectInto(loser.id, winner.id); err != nil {
				return fmt.Errorf("merge %s → %s: %w", loser.id, winner.id, err)
			}
		}

		// If the winner's stored id differs from the canonical form
		// (e.g. all rows used non-canonical casing), rename the winner
		// row to the canonical id so future invocations match.
		if winner.id != canon {
			if err := s.renameProjectID(winner.id, canon); err != nil {
				return fmt.Errorf("rename winner %s → %s: %w", winner.id, canon, err)
			}
		}

		// Recompute denormalised counts on the survivor. mergeProjectInto
		// re-keys the loser's rows onto the winner but leaves
		// projects.sym_count / file_count / edge_count at the winner's
		// pre-merge values, so `pincher list` would display stale numbers
		// until the next full re-index. Source of truth: the symbols /
		// files / edges tables themselves.
		if err := s.recomputeProjectCounts(canon); err != nil {
			return fmt.Errorf("recompute counts for %s: %w", canon, err)
		}
	}
	return nil
}

// recomputeProjectCounts refreshes projects.sym_count / file_count /
// edge_count from the canonical row tables for `projectID`. Used by
// dedupProjectsByCanonicalPath after merging; the merge re-keys rows
// but doesn't touch the denormalised counts. Cheap (3 indexed
// COUNT(*)s + 1 UPDATE) and only fires when a duplicate group existed.
func (s *Store) recomputeProjectCounts(projectID string) error {
	_, err := s.db.Exec(`
		UPDATE projects SET
		  sym_count  = (SELECT COUNT(*) FROM symbols WHERE project_id = ?),
		  file_count = (SELECT COUNT(*) FROM files   WHERE project_id = ?),
		  edge_count = (SELECT COUNT(*) FROM edges   WHERE project_id = ?)
		WHERE id = ?`,
		projectID, projectID, projectID, projectID)
	return err
}

// mergeProjectInto re-points every project_id-keyed row from `loser`
// onto `winner`, dropping any row that would collide with existing
// winner data. After this, the loser project has zero rows pointing
// at it; the caller deletes the project row itself.
//
// Order matters because of foreign keys: tables that REFERENCE
// projects(id) (symbols, edges, extraction_failures) must be re-keyed
// before the loser project row can be deleted. Tables without FK
// constraints (files, adrs, symbol_moves, slow_queries) follow.
func (s *Store) mergeProjectInto(loser, winner string) error {
	// Each statement uses the same conflict-tolerant pattern: try to
	// move every row, then delete the rows whose ID already existed on
	// the winner (UPDATE would have failed for those due to PK/UNIQUE).
	//
	// The `WHERE id NOT IN (SELECT id FROM <tbl> WHERE project_id =
	// winner)` guard prevents UPDATE from raising; the subsequent
	// DELETE cleans up the rows we couldn't move.

	for _, op := range []struct {
		table string
		// columns that constitute a uniqueness boundary on (project_id, ...)
		// — UPDATE only when no row on the winner already covers this key.
		uniqueKeyCols string
	}{
		{"symbols", "id"},
		{"edges", "from_id, to_id, kind"},
		{"extraction_failures", "file_path, reason"},
		{"files", "path"},
		{"adrs", "key"},
		{"symbol_moves", "old_id"},
	} {
		// Move rows that don't conflict.
		moveSQL := `UPDATE ` + op.table + ` SET project_id = ? WHERE project_id = ? AND NOT EXISTS (` +
			`SELECT 1 FROM ` + op.table + ` w WHERE w.project_id = ? AND ` +
			joinEqClauses(op.uniqueKeyCols, op.table) + `)`
		if _, err := s.db.Exec(moveSQL, winner, loser, winner); err != nil {
			return fmt.Errorf("move %s: %w", op.table, err)
		}
		// Drop the remainder (the conflicts).
		if _, err := s.db.Exec(`DELETE FROM `+op.table+` WHERE project_id = ?`, loser); err != nil {
			return fmt.Errorf("drop loser %s: %w", op.table, err)
		}
	}

	// slow_queries: project_id is nullable + no FK; just re-key.
	if _, err := s.db.Exec(`UPDATE slow_queries SET project_id = ? WHERE project_id = ?`, winner, loser); err != nil {
		return fmt.Errorf("re-key slow_queries: %w", err)
	}

	// Finally drop the loser project row.
	if _, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, loser); err != nil {
		return fmt.Errorf("delete loser projects: %w", err)
	}
	return nil
}

// renameProjectID rewrites every project_id reference from `from` to
// `to`. Used when no existing project row was already at the canonical
// form — we rename the winner to bring it to canonical without going
// through the merge path.
//
// This is a write-amplification operation (every row touching project_id
// gets updated) but only fires when a duplicate group existed in the
// first place, which is rare and one-shot.
func (s *Store) renameProjectID(from, to string) error {
	// Foreign keys would block UPDATE on the projects.id column;
	// disable temporarily for this batch.
	if _, err := s.db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable fk: %w", err)
	}
	defer func() { _, _ = s.db.Exec(`PRAGMA foreign_keys = ON`) }()

	for _, table := range []string{
		"projects", "symbols", "edges", "extraction_failures",
		"files", "adrs", "symbol_moves", "slow_queries",
	} {
		col := "project_id"
		if table == "projects" {
			col = "id"
		}
		if _, err := s.db.Exec(`UPDATE `+table+` SET `+col+` = ? WHERE `+col+` = ?`, to, from); err != nil {
			return fmt.Errorf("rename in %s: %w", table, err)
		}
	}
	return nil
}

// joinEqClauses turns "a, b, c" into "w.a = <table>.a AND w.b =
// <table>.b AND w.c = <table>.c" for use in the conflict-detection
// subquery in mergeProjectInto.
func joinEqClauses(cols, table string) string {
	parts := make([]string, 0, 3)
	for _, c := range strings.Split(cols, ",") {
		c = strings.TrimSpace(c)
		parts = append(parts, "w."+c+" = "+table+"."+c)
	}
	return strings.Join(parts, " AND ")
}

// ─────────────────────────────────────────────────────────────────────────────
// Schema
// ─────────────────────────────────────────────────────────────────────────────

// schema is the complete pincherMCP database layout.
//
// Three-layer design:
//
//	Layer 1 — Byte-Offset Symbol Store
//	  symbols.start_byte / end_byte → os.File.Seek + Read → O(1) source
//	  No re-parsing on retrieval, no line scanning.
//
//	Layer 2 — Knowledge Graph
//	  nodes = symbols rows (shared — no duplication)
//	  edges = CALLS / IMPORTS / INHERITS / IMPLEMENTS etc.
//	  Supports Cypher-like MATCH → SQL translation → sub-ms queries
//
//	Layer 3 — FTS5 Full-Text Search
//	  Three per-corpus virtual tables with built-in BM25 ranking:
//	    symbols_code_fts   — Function/Method/Class/etc.
//	    symbols_config_fts — YAML/JSON/HCL/TOML/XML Settings/Resources/Outputs
//	    symbols_docs_fts   — Markdown sections + fetched Documents
//	  Auto-synced via AFTER INSERT/UPDATE/DELETE triggers
//	  (The legacy single-corpus `symbols_fts` was removed in #106.)

// ftsCorpusSplitDDL is the v8→v9 migration that adds three corpus-specific
// FTS5 vtabs alongside the legacy `symbols_fts`, plus their sync triggers
// and a one-time backfill from existing symbols.
//
// **Naming**: `symbols_<corpus>_fts` (e.g. `symbols_code_fts`), NOT
// `symbols_fts_<corpus>`. The latter collides with FTS5's internal
// shadow-table naming convention — `symbols_fts` creates a shadow table
// called `symbols_fts_config`, so a new vtab named `symbols_fts_config`
// errors with "already exists". The `<table>_<corpus>_fts` order avoids
// every shadow-table prefix the legacy index produces.
//
// Routing is encoded in the trigger WHERE clauses; corpus.go's
// ClassifyCorpus is the Go mirror. The TestClassifyCorpus_MatchesSQLTriggerRouting
// parity gate guards against drift between Go and SQL.
//
// Pattern: each trigger event (insert/delete/update) has three INSERT…SELECT
// statements, one per corpus. The WHERE clause routes via NEW.language /
// NEW.kind (or OLD for delete/update-old). The "code" branch uses NOT IN
// rather than IN so adding a new code language is cheap (no migration
// edit required) — only "config" and "docs" enumerate.
//
// RebuildFTS knows about all four vtabs so the escape hatch stays valid.
const ftsCorpusSplitDDL = `
CREATE VIRTUAL TABLE IF NOT EXISTS symbols_code_fts USING fts5(
    symbol_id UNINDEXED,
    name,
    qualified_name,
    signature,
    docstring,
    content='symbols',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 1'
);
CREATE VIRTUAL TABLE IF NOT EXISTS symbols_config_fts USING fts5(
    symbol_id UNINDEXED,
    name,
    qualified_name,
    signature,
    docstring,
    content='symbols',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 1'
);
CREATE VIRTUAL TABLE IF NOT EXISTS symbols_docs_fts USING fts5(
    symbol_id UNINDEXED,
    name,
    qualified_name,
    signature,
    docstring,
    content='symbols',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 1'
);

-- INSERT trigger: route the new symbol into exactly one of the three vtabs.
CREATE TRIGGER IF NOT EXISTS sym_fts_corpus_insert AFTER INSERT ON symbols BEGIN
    INSERT INTO symbols_code_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind = 'Document' OR new.language IN ('Markdown', 'HTML');
END;

-- DELETE trigger: emit the FTS5 'delete' command to whichever vtab held the row.
CREATE TRIGGER IF NOT EXISTS sym_fts_corpus_delete AFTER DELETE ON symbols BEGIN
    INSERT INTO symbols_code_fts(symbols_code_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(symbols_config_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(symbols_docs_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind = 'Document' OR old.language IN ('Markdown', 'HTML');
END;

-- UPDATE trigger: delete the OLD row from its vtab, insert the NEW row into
-- its vtab. The OLD and NEW corpora may differ if a symbol's language or
-- kind changes — though in practice that's a re-extraction, which goes
-- through DELETE+INSERT, not UPDATE.
CREATE TRIGGER IF NOT EXISTS sym_fts_corpus_update AFTER UPDATE ON symbols BEGIN
    INSERT INTO symbols_code_fts(symbols_code_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(symbols_config_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind != 'Document'
      AND old.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(symbols_docs_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT 'delete', old.rowid, old.id, old.name, old.qualified_name,
           COALESCE(old.signature,''), COALESCE(old.docstring,'')
    WHERE old.kind = 'Document' OR old.language IN ('Markdown', 'HTML');

    INSERT INTO symbols_code_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_config_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind != 'Document'
      AND new.language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

    INSERT INTO symbols_docs_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    SELECT new.rowid, new.id, new.name, new.qualified_name,
           COALESCE(new.signature,''), COALESCE(new.docstring,'')
    WHERE new.kind = 'Document' OR new.language IN ('Markdown', 'HTML');
END;

-- One-time backfill from existing symbols. Migrations only run once per
-- DB version, so this is safe (no idempotency concern).
INSERT INTO symbols_code_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
SELECT rowid, id, name, qualified_name, COALESCE(signature,''), COALESCE(docstring,'')
FROM symbols
WHERE kind != 'Document'
  AND language NOT IN ('Markdown', 'HTML', 'YAML', 'JSON', 'HCL', 'TOML', 'XML');

INSERT INTO symbols_config_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
SELECT rowid, id, name, qualified_name, COALESCE(signature,''), COALESCE(docstring,'')
FROM symbols
WHERE kind != 'Document'
  AND language IN ('YAML', 'JSON', 'HCL', 'TOML', 'XML');

INSERT INTO symbols_docs_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
SELECT rowid, id, name, qualified_name, COALESCE(signature,''), COALESCE(docstring,'')
FROM symbols
WHERE kind = 'Document' OR language = 'Markdown';
`

const schema = `
CREATE TABLE IF NOT EXISTS projects (
    id          TEXT    PRIMARY KEY,
    path        TEXT    NOT NULL,
    name        TEXT    NOT NULL,
    indexed_at  INTEGER,
    file_count  INTEGER DEFAULT 0,
    sym_count   INTEGER DEFAULT 0,
    edge_count  INTEGER DEFAULT 0
    -- schema_version_at_index column added by v15 migration (#236).
    -- Intentionally not in baseline so the ALTER TABLE in the migration
    -- doesn't double-declare on a fresh-DB path (CREATE TABLE creates
    -- the column, then ALTER TABLE fails with "duplicate column name").
    -- The migration runs for both fresh and upgrading DBs.
);

-- Stable ID format: "{file_path}::{qualified_name}#{kind}"
-- Stable across re-indexing so agents can persist symbol references.
-- v28 (#1231): primary key is COMPOSITE (project_id, id). Pre-v28 the
-- bare id was PK, which collided across projects when two repos share
-- the same relative file path containing the same qualified-name +
-- kind. INSERT OR REPLACE silently flipped the row's project_id to
-- the latest writer; ~89% of one file's methods vanished from queries
-- scoped to the original project. Composite PK eliminates the collision
-- structurally — same id in two projects is two rows.
CREATE TABLE IF NOT EXISTS symbols (
    id             TEXT    NOT NULL,
    project_id     TEXT    NOT NULL REFERENCES projects(id),
    file_path      TEXT    NOT NULL,
    name           TEXT    NOT NULL,
    qualified_name TEXT    NOT NULL,
    kind           TEXT    NOT NULL,
    language       TEXT    NOT NULL,

    -- Layer 1: Byte-Offset Retrieval
    -- Retrieval = 1 SQL lookup + 1 file seek (seek to start_byte, read end_byte-start_byte bytes).
    -- Zero re-parsing. Zero line scanning.
    start_byte     INTEGER NOT NULL,
    end_byte       INTEGER NOT NULL,

    start_line     INTEGER NOT NULL,
    end_line       INTEGER NOT NULL,
    signature      TEXT,
    return_type    TEXT,
    docstring      TEXT,
    parent         TEXT,

    -- Layer 2: Graph properties
    complexity     INTEGER DEFAULT 0,
    is_exported    INTEGER DEFAULT 0,
    is_test        INTEGER DEFAULT 0,
    is_entry_point INTEGER DEFAULT 0,

    file_hash      TEXT,
    PRIMARY KEY (project_id, id)
);
CREATE INDEX IF NOT EXISTS idx_sym_project ON symbols(project_id);
-- v28: bare-id lookup index. Most byte-offset retrieval paths
-- (symbol, context) pass the id without a project scope because
-- callers cache IDs across sessions. Without this index the composite-
-- PK lookup degrades to a full scan on bare-id queries. The index
-- covers the symbol-id-only access pattern; cross-project resolution
-- still goes through the explicit project_id filter on top.
CREATE INDEX IF NOT EXISTS idx_sym_id      ON symbols(id);
CREATE INDEX IF NOT EXISTS idx_sym_file    ON symbols(project_id, file_path);
CREATE INDEX IF NOT EXISTS idx_sym_kind    ON symbols(project_id, kind);
CREATE INDEX IF NOT EXISTS idx_sym_name    ON symbols(project_id, name);
CREATE INDEX IF NOT EXISTS idx_sym_qn      ON symbols(project_id, qualified_name);

-- Layer 3: FTS5 full-text search with BM25 ranking is set up by the v9
-- migration (ftsCorpusSplitDDL): three per-corpus vtabs
-- (symbols_code_fts / symbols_config_fts / symbols_docs_fts) plus their
-- sync triggers. Fresh DBs run all migrations after baseline, so the
-- corpus-split DDL fires on first Open(). The legacy single-corpus
-- symbols_fts vtab — which used to live in this baseline schema —
-- was removed in #106 (v12 migration drops it on existing DBs).
CREATE TABLE IF NOT EXISTS edges (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id TEXT    NOT NULL REFERENCES projects(id),
    from_id    TEXT    NOT NULL,
    to_id      TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    confidence REAL    DEFAULT 1.0,
    properties TEXT,
    UNIQUE(project_id, from_id, to_id, kind)
);
CREATE INDEX IF NOT EXISTS idx_edge_from ON edges(from_id);
CREATE INDEX IF NOT EXISTS idx_edge_to   ON edges(to_id);
CREATE INDEX IF NOT EXISTS idx_edge_kind ON edges(project_id, kind);

CREATE TABLE IF NOT EXISTS files (
    project_id TEXT    NOT NULL,
    path       TEXT    NOT NULL,
    hash       TEXT    NOT NULL,
    indexed_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, path)
);

CREATE TABLE IF NOT EXISTS adrs (
    project_id TEXT    NOT NULL,
    key        TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, key)
);
`

// ─────────────────────────────────────────────────────────────────────────────
// Domain types
// ─────────────────────────────────────────────────────────────────────────────

// Symbol is a code entity extracted by the AST indexer.
type Symbol struct {
	ID            string
	ProjectID     string
	FilePath      string
	Name          string
	QualifiedName string
	Kind          string // Function|Method|Class|Interface|Enum|Type|Variable|Module
	Language      string
	StartByte     int
	EndByte       int
	StartLine     int
	EndLine       int
	Signature     string
	ReturnType    string
	Docstring     string
	Parent        string
	Complexity             int
	IsExported             bool
	IsTest                 bool
	IsEntryPoint           bool
	FileHash               string
	ExtractionConfidence   float64 // 1.0 = AST-exact (Go); <1.0 = regex-approximate
	// Branch is the git branch the symbol was extracted on, captured
	// from `git rev-parse --abbrev-ref HEAD` at index time. Empty
	// when the project root isn't a git working tree, when the
	// indexer ran before #1303 Phase 2 wired branch stamping, or when
	// HEAD is detached (the indexer falls back to the commit SHA in
	// that case — Phase 2's call). #1303 Phase 1: column exists but
	// the indexer doesn't populate it yet; all rows carry '' until
	// Phase 2 lands.
	Branch                 string
}

// MakeSymbolID produces the stable, human-readable symbol ID.
func MakeSymbolID(filePath, qualifiedName, kind string) string {
	return filePath + "::" + qualifiedName + "#" + kind
}

// Edge is a directed relationship between two symbols.
type Edge struct {
	ID         int64
	ProjectID  string
	FromID     string
	ToID       string
	Kind       string
	Confidence float64
	Properties map[string]any
	// Source is the origin marker added in schema v20 (#475). One of:
	//   - "per_file"     (default; per-file extractor goroutine output)
	//   - "resolve_pass" (resolveCalls / resolveImports / resolveReads)
	// BulkUpsertEdges treats empty as "per_file" so older callers keep
	// working unchanged. The indexer's tail-resolve passes set
	// "resolve_pass" so a project-wide DELETE-then-INSERT can atomically
	// replace cross-file output without nuking per-file edges.
	Source string
	// Branch is the git branch the edge was emitted on. Empty until
	// #1303 Phase 2 wires index-time branch stamping. Phase 2 will
	// widen the UNIQUE constraint to include branch so the same
	// (from_id, to_id, kind) tuple can coexist across branches.
	Branch string
}

// Project summarises an indexed repository.
type Project struct {
	// ID is the canonical join key. Lowercased on case-insensitive
	// filesystems (Windows NTFS, macOS APFS) via CanonicalProjectPath
	// so symlink + casing variants of the same physical directory
	// dedup to one row. Use this for project lookups and joins (#277).
	ID string `json:"id"`
	// Path is the display + filesystem-operation value. Original
	// casing preserved so callers can concatenate it with relative
	// paths and have file operations work on case-sensitive volumes.
	// On case-insensitive filesystems, Path may differ in casing
	// from ID — that's intentional, not a bug (#277).
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	IndexedAt time.Time `json:"indexed_at"`
	FileCount int       `json:"file_count"`
	SymCount  int       `json:"symbol_count"`
	EdgeCount int       `json:"edge_count"`
	// SchemaVersionAtIndex is the schema_version_at_index column added in
	// v15 (#236). Surfaces "this project was last indexed against schema
	// vN" so pincher list / doctor can flag projects that predate later
	// extractor or migration work and would benefit from a re-index. NULL
	// (as a *int with a nil value) means the row predates the column —
	// rendered as "stale (unknown)" because it was definitely indexed
	// before v15. Non-nil = exact value, compare against the running
	// binary's max-known schema version.
	SchemaVersionAtIndex *int `json:"schema_version_at_index,omitempty"`
	// BinaryVersion is the indexer binary version that produced this
	// project's index data (#304, schema v18). Empty when the row
	// pre-dates the column or was inserted by a binary that didn't
	// stamp it. health uses this to surface re-index recommendations
	// when extraction or call-resolution rules have evolved.
	BinaryVersion string `json:"binary_version,omitempty"`
	// CurrentBranch is the git branch the project was last indexed on
	// (#1303 Phase 2a, schema v32). Captured via `git rev-parse
	// --abbrev-ref HEAD` at index time. Empty when the project root
	// isn't a git working tree; when HEAD is detached, the indexer
	// falls back to a short commit SHA. Doctor uses this to surface a
	// stale-index advisory when the user's checked-out branch has
	// drifted from the last-indexed branch.
	//
	// JSON tag is `last_indexed_branch` (renamed from `current_branch`
	// in #1388 — pre-1.0 surface clean-up). The Go field name stays
	// `CurrentBranch` for internal source-compat with the dozens of
	// call sites that read it; only the wire format changes. The DB
	// column also stays `current_branch` (internal-only, never
	// surfaces to MCP / HTTP consumers). The original name read like
	// "what branch IS the project on right now" but actually meant
	// "what branch was the project last indexed on" — every new
	// integrator would misread it.
	CurrentBranch string `json:"last_indexed_branch,omitempty"`
}

// SearchResult is a FTS5 match returned by SearchSymbols.
type SearchResult struct {
	Symbol Symbol
	Score  float64
}

// ─────────────────────────────────────────────────────────────────────────────
// Project operations
// ─────────────────────────────────────────────────────────────────────────────

// UpsertProject creates or updates a project record. The schema_version_at_index
// column (#236, v15) is stamped with the current binary's max-known schema
// version on every call — that's the moment the indexer "vouches for" the
// project's freshness. Subsequent re-index runs by a binary at a higher
// schema bump it again; binaries that don't re-index leave it stale.
func (s *Store) UpsertProject(p Project) error {
	currentSchema := len(schemaMigrations) + 1
	// #724: monotonic guard on the freshness-tracking columns. Multiple
	// pincher processes can Watch() the same project against one shared
	// DB — and an orphaned old process (one whose parent died but whose
	// Watch() loop lives on) would otherwise stomp schema_version_at_index
	// and binary_version back to its stale values on every poll, breaking
	// the index_drift detector and CLAUDE.md's freshness check.
	//
	// A binary running an older schema is, by definition, an older binary.
	// So: never let schema_version_at_index go backwards, and only adopt
	// the incoming binary_version when its schema is >= the stored one.
	// Path/name/counts always update — those are cheap and a stale
	// re-walk of the same files is still accurate for them. The full
	// reaping fix (parent-liveness self-exit) is the other half of #724.
	_, err := s.db.Exec(`
		INSERT INTO projects(id, path, name, indexed_at, file_count, sym_count, edge_count, schema_version_at_index, binary_version, current_branch)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			path=excluded.path, name=excluded.name, indexed_at=excluded.indexed_at,
			file_count=excluded.file_count, sym_count=excluded.sym_count, edge_count=excluded.edge_count,
			binary_version=CASE
				WHEN excluded.schema_version_at_index >= schema_version_at_index
				THEN excluded.binary_version ELSE binary_version END,
			schema_version_at_index=MAX(schema_version_at_index, excluded.schema_version_at_index),
			current_branch=excluded.current_branch`,
		p.ID, p.Path, p.Name, p.IndexedAt.Unix(),
		p.FileCount, p.SymCount, p.EdgeCount, currentSchema, p.BinaryVersion, p.CurrentBranch,
	)
	return err
}

// UpdateProjectCounts writes only the cached file/symbol/edge counts for a
// project. Used to refresh `pincher list` output during a long index run so
// callers see in-flight progress instead of zeros until the final
// UpsertProject at the end of Index().
func (s *Store) UpdateProjectCounts(projectID string, files, syms, edges int) error {
	_, err := s.db.Exec(
		`UPDATE projects SET file_count=?, sym_count=?, edge_count=? WHERE id=?`,
		files, syms, edges, projectID,
	)
	return err
}

// UpsertProjectMeta upserts a project's metadata WITHOUT touching the
// cached file/symbol/edge counts. Used at the start of Index() where
// path/name/indexed_at/binary_version need to land but the counts are
// still zero (struct zero values from the just-constructed Project) —
// the existing UpsertProject would overwrite the prior run's accurate
// counts with zeros, so `health` reads as "0 symbols / 0 files / 0
// edges" for the brief window between start-of-index and the first
// UpdateProjectCounts call (#894).
//
// Counts continue to flow through UpdateProjectCounts during the run
// and the final UpsertProject at the end of Index() writes the
// authoritative totals.
//
// The #724 monotonic guard on schema_version_at_index / binary_version
// is preserved exactly — a stale orphan re-walker can't stomp newer
// schema metadata even via this path.
//
// #1154: the schema-version guard alone isn't enough across orphan
// processes of the same schema_version. Two pincher children of
// the same minor version (different git-commit-count dev builds)
// race: the older one's watcher stamps `binary_version` back over
// the newer one's write because schema equality lets the CASE pass.
// Read-then-compare in Go preserves the correct binary_version when
// an older writer races a newer one. Same shape as the schema guard,
// just one level down for matching-schema dev-build pairs.
func (s *Store) UpsertProjectMeta(p Project) error {
	currentSchema := len(schemaMigrations) + 1
	// #1154 guard: if an older binary_version would overwrite a newer
	// one (same schema), skip the binary_version write but still
	// update path/name/indexed_at. Done in Go because semver-with-
	// commit-count comparison (`0.58.0-44-g91e9c0f` vs `0.58.0-10-
	// gdeb797d`) is impractical in pure SQL — the dash-separated
	// commit count is the deciding bit for dev builds and isn't a
	// single sortable lexical prefix.
	var existingBV sql.NullString
	if err := s.ro.QueryRow(
		`SELECT binary_version FROM projects WHERE id = ?`, p.ID,
	).Scan(&existingBV); err != nil && err != sql.ErrNoRows {
		return err
	}
	binaryToWrite := p.BinaryVersion
	if existingBV.Valid && existingBV.String != "" {
		if compareBinaryVersion(p.BinaryVersion, existingBV.String) < 0 {
			// Incoming is older — don't downgrade. Keep the existing
			// value by writing back what's already there.
			binaryToWrite = existingBV.String
		}
	}
	_, err := s.db.Exec(`
		INSERT INTO projects(id, path, name, indexed_at, file_count, sym_count, edge_count, schema_version_at_index, binary_version, current_branch)
		VALUES (?,?,?,?,0,0,0,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			path=excluded.path, name=excluded.name, indexed_at=excluded.indexed_at,
			-- #1086: COALESCE the existing schema_version_at_index to 0 so
			-- pre-v18 projects (NULL value) can be re-stamped on re-index.
			-- Pre-fix, 'excluded.schema_version_at_index >= NULL' evaluated
			-- to NULL (false in CASE WHEN) and 'MAX(NULL, 26)' returned NULL
			-- (scalar SQLite MAX propagates NULL), so binary_version and
			-- schema_version_at_index stayed NULL forever — the drift
			-- warning fired permanently even after a force re-index.
			--
			-- #1154: binary_version is already version-clamped in Go above
			-- (binaryToWrite param). The schema_version_at_index guard
			-- below still catches cross-schema downgrades.
			binary_version=CASE
				WHEN excluded.schema_version_at_index >= COALESCE(schema_version_at_index, 0)
				THEN excluded.binary_version ELSE binary_version END,
			schema_version_at_index=MAX(COALESCE(schema_version_at_index, 0), excluded.schema_version_at_index),
			-- #1303 Phase 2a: current_branch flips on every UpsertProjectMeta
			-- (the start-of-Index() call) so the value reflects the branch
			-- that's about to be indexed, not the prior one. This makes the
			-- post-checkout-then-reindex doctor advisory accurate even if
			-- the indexing run crashes midway.
			current_branch=excluded.current_branch`,
		p.ID, p.Path, p.Name, p.IndexedAt.Unix(),
		currentSchema, binaryToWrite, p.CurrentBranch,
	)
	return err
}

// compareBinaryVersion compares pincher build version strings of the
// form "0.58.0", "0.58.0-44-g91e9c0f", or "dev". Returns -1 if a < b,
// 0 if equal, +1 if a > b. Parse-failure falls back to string compare
// so callers that pass weird values get a deterministic-if-arbitrary
// answer rather than panicking — and a totally-unparseable incoming
// version never silently displaces a real one because the equal-
// string case skips the write either way.
//
// Format expected: [v]MAJOR.MINOR.PATCH[-COMMITS-gSHA]
// "dev" (the unstamped go build sentinel) is treated as the lowest
// possible version — it must never displace a real release stamp.
func compareBinaryVersion(a, b string) int {
	if a == b {
		return 0
	}
	if a == "dev" && b != "dev" {
		return -1
	}
	if b == "dev" && a != "dev" {
		return 1
	}
	ai, aOK := parseBinaryVersion(a)
	bi, bOK := parseBinaryVersion(b)
	if !aOK || !bOK {
		// Unparseable — fall back to string compare for determinism.
		if a < b {
			return -1
		}
		if a > b {
			return 1
		}
		return 0
	}
	for i := 0; i < 4; i++ {
		if ai[i] < bi[i] {
			return -1
		}
		if ai[i] > bi[i] {
			return 1
		}
	}
	return 0
}

// parseBinaryVersion extracts the four numeric components from a
// pincher build version: [v]MAJOR.MINOR.PATCH[-COMMITS-gSHA]. Returns
// [major, minor, patch, commits] and an ok flag. COMMITS defaults to
// 0 when the version is a clean release tag. Returns (zeros, false)
// on unrecognizable input.
func parseBinaryVersion(s string) ([4]int, bool) {
	var out [4]int
	v := strings.TrimPrefix(s, "v")
	// Split off "-COMMITS-gSHA" if present.
	core := v
	if dash := strings.Index(v, "-"); dash >= 0 {
		core = v[:dash]
		rest := v[dash+1:]
		// rest looks like "44-g91e9c0f" — take the leading digits.
		if dash2 := strings.Index(rest, "-"); dash2 > 0 {
			rest = rest[:dash2]
		}
		commits, err := strconv.Atoi(rest)
		if err != nil {
			return out, false
		}
		out[3] = commits
	}
	parts := strings.SplitN(core, ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ListProjects returns all indexed projects.
func (s *Store) ListProjects() ([]Project, error) {
	// Reader pool (#51).
	rows, err := s.ro.Query(
		`SELECT id, path, name, indexed_at, file_count, sym_count, edge_count, schema_version_at_index, binary_version, current_branch FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var ts int64
		var schemaVer sql.NullInt64
		var binVer sql.NullString
		if err := rows.Scan(&p.ID, &p.Path, &p.Name, &ts, &p.FileCount, &p.SymCount, &p.EdgeCount, &schemaVer, &binVer, &p.CurrentBranch); err != nil {
			return nil, err
		}
		p.IndexedAt = time.Unix(ts, 0)
		if schemaVer.Valid {
			v := int(schemaVer.Int64)
			p.SchemaVersionAtIndex = &v
		}
		if binVer.Valid {
			p.BinaryVersion = binVer.String
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectsContainingPath returns every project whose canonical path is a
// strict ancestor of `target` (i.e. `target` is nested under that
// project). Returns the empty slice when no enclosing project exists.
//
// Used by the indexer (#235) to warn the user before silently
// duplicating symbols of a child path that's already covered by a
// parent index. The lookup is case-sensitive on Unix and
// case-insensitive on Windows / macOS via filepath.EqualFold-style
// comparison; we hand-roll the prefix check rather than using SQL
// LIKE because path separator handling differs across platforms and
// SQLite's collations don't cover it.
//
// `target` should already be canonicalised (Abs + EvalSymlinks)
// before passing in — callers like the indexer always work with
// resolved paths anyway.
func (s *Store) ProjectsContainingPath(target string) ([]Project, error) {
	target = filepath.Clean(target)
	all, err := s.ListProjects()
	if err != nil {
		return nil, err
	}
	var out []Project
	for _, p := range all {
		ppath := filepath.Clean(p.Path)
		if pathContains(ppath, target) {
			out = append(out, p)
		}
	}
	return out, nil
}

// pathContains reports whether `child` is a strict descendant of
// `parent`. Equal paths return false (the indexer's caller wants
// "nested under", not "is the same as"). Path separators are
// normalised; on case-insensitive filesystems the comparison is
// case-insensitive.
func pathContains(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	// Rel returns a "../..." when child is outside parent, and a
	// relative dotted path when it's inside. Reject the "../" case
	// so we don't false-positive on siblings.
	if strings.HasPrefix(rel, "..") {
		return false
	}
	return rel != "." && rel != ""
}

// GetProject returns a single project by ID, or nil if not found.
func (s *Store) GetProject(id string) (*Project, error) {
	// Reader pool (#51).
	row := s.ro.QueryRow(
		`SELECT id, path, name, indexed_at, file_count, sym_count, edge_count, schema_version_at_index, binary_version, current_branch FROM projects WHERE id=?`, id)
	var p Project
	var ts int64
	var schemaVer sql.NullInt64
	var binVer sql.NullString
	if err := row.Scan(&p.ID, &p.Path, &p.Name, &ts, &p.FileCount, &p.SymCount, &p.EdgeCount, &schemaVer, &binVer, &p.CurrentBranch); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	p.IndexedAt = time.Unix(ts, 0)
	if schemaVer.Valid {
		v := int(schemaVer.Int64)
		p.SchemaVersionAtIndex = &v
	}
	if binVer.Valid {
		p.BinaryVersion = binVer.String
	}
	return &p, nil
}

// CurrentSchemaVersion returns the maximum schema version this binary
// understands. Used by callers like `pincher list` and `pincher doctor`
// to compare against per-project SchemaVersionAtIndex (#236).
func CurrentSchemaVersion() int {
	return len(schemaMigrations) + 1
}

// LanguageCoverage describes extraction quality for one language.
type LanguageCoverage struct {
	Language   string  `json:"language"`
	Parser     string  `json:"parser"`   // "AST" or "Regex"
	Confidence float64 `json:"confidence"` // avg extraction_confidence for this language
	Symbols    int     `json:"symbols"`
	// ByKind breaks the language's coverage down per symbol kind so the
	// `health` tool can surface "your YAML extractor produces low-confidence
	// Settings" without a separate query (#34 Phase 3). Empty for projects
	// indexed before Phase 3.
	ByKind []KindCoverage `json:"by_kind,omitempty"`
}

// KindCoverage holds confidence statistics for one (language, kind) pair.
// p10 / p50 are computed in Go from the per-symbol confidence column — the
// pure-Go SQLite driver doesn't expose `percentile_cont`, so we sort the
// per-group symbols and slice the percentiles directly. Acceptable cost
// because health is user-triggered, not on the hot path.
type KindCoverage struct {
	Kind          string  `json:"kind"`
	Symbols       int     `json:"symbols"`
	AvgConfidence float64 `json:"avg_confidence"`
	P10           float64 `json:"p10"`
	P50           float64 `json:"p50"`
}

// HealthReport is the output of HealthCheck.
type HealthReport struct {
	SchemaVersion  int                `json:"schema_version"`
	Project        *Project           `json:"project,omitempty"`
	StalenessSecs  int64              `json:"staleness_seconds"`
	StalenessHuman string             `json:"staleness_human"`
	Coverage       []LanguageCoverage `json:"extraction_coverage"`
	DBPath         string             `json:"db_path"`
}

// HealthCheck returns diagnostic information for the given project.
// projectID may be empty, in which case Project and coverage are omitted.
func (s *Store) HealthCheck(projectID string) (*HealthReport, error) {
	// #328: pre-allocate Coverage so the JSON shape is stable. A nil
	// slice marshals to `null`, which forces every consumer to null-check
	// before iterating; an empty slice marshals to `[]`, which they can
	// always range over safely. Same field, same endpoint — same shape.
	report := &HealthReport{DBPath: s.Path, Coverage: []LanguageCoverage{}}

	// Schema version — route through the reader pool so health probes
	// don't queue behind the single-writer connection during indexing
	// (#960 binaryDriftForce can hold the writer for tens of seconds
	// on a full corpus re-extract). Pure SELECT — safe by the reader-
	// pool invariant in CLAUDE.md.
	if err := s.ro.QueryRow(`SELECT version FROM schema_version`).Scan(&report.SchemaVersion); err != nil {
		report.SchemaVersion = -1
	}

	if projectID == "" {
		return report, nil
	}

	// Project staleness
	p, err := s.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	if p != nil {
		report.Project = p
		stale := time.Since(p.IndexedAt)
		report.StalenessSecs = int64(stale.Seconds())
		report.StalenessHuman = formatStaleness(stale)
	}

	// Per-language extraction coverage — reader pool, same rationale.
	rows, err := s.ro.Query(`
		SELECT language, AVG(extraction_confidence), COUNT(*)
		FROM symbols
		WHERE project_id = ?
		GROUP BY language
		ORDER BY COUNT(*) DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var lc LanguageCoverage
		if err := rows.Scan(&lc.Language, &lc.Confidence, &lc.Symbols); err != nil {
			return nil, err
		}
		if lc.Confidence >= 0.99 {
			lc.Parser = "AST"
		} else {
			lc.Parser = "Regex"
		}
		report.Coverage = append(report.Coverage, lc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Per-(language, kind) percentiles. We pull all (language, kind, conf)
	// tuples in one query, group in Go, then sort each group to compute
	// p10/p50. For projects with N symbols this is O(N) memory + O(N log N)
	// time — fine for a user-triggered tool.
	if err := s.populateKindCoverage(report, projectID); err != nil {
		return nil, err
	}
	return report, nil
}

// populateKindCoverage attaches per-kind percentile stats to each
// LanguageCoverage entry already in report. Pulled into its own method
// so HealthCheck stays scannable.
func (s *Store) populateKindCoverage(report *HealthReport, projectID string) error {
	type langKindRow struct {
		Language string
		Kind     string
		Conf     float64
	}
	tupleRows, err := s.ro.Query(`
		SELECT language, kind, extraction_confidence
		FROM symbols
		WHERE project_id = ?`, projectID)
	if err != nil {
		return err
	}
	defer tupleRows.Close()

	// (language, kind) → sorted confidence slice, built up incrementally.
	groups := make(map[string]map[string][]float64)
	for tupleRows.Next() {
		var r langKindRow
		if err := tupleRows.Scan(&r.Language, &r.Kind, &r.Conf); err != nil {
			return err
		}
		byLang, ok := groups[r.Language]
		if !ok {
			byLang = make(map[string][]float64)
			groups[r.Language] = byLang
		}
		byLang[r.Kind] = append(byLang[r.Kind], r.Conf)
	}
	if err := tupleRows.Err(); err != nil {
		return err
	}

	// Stitch into the existing language ordering.
	for i := range report.Coverage {
		lang := report.Coverage[i].Language
		byKind, ok := groups[lang]
		if !ok {
			continue
		}
		report.Coverage[i].ByKind = computeKindCoverages(byKind)
	}
	return nil
}

// computeKindCoverages turns the per-(kind) confidence slices into a sorted
// list of KindCoverage records. Uses index-based percentile (no
// interpolation) — for our N (typically 1-1000 symbols per kind) the
// difference between linear-interpolated and index-based p10 is in the
// fourth decimal, well below the resolution that matters for diagnostics.
func computeKindCoverages(byKind map[string][]float64) []KindCoverage {
	out := make([]KindCoverage, 0, len(byKind))
	for kind, confs := range byKind {
		sort.Float64s(confs)
		n := len(confs)
		if n == 0 {
			continue
		}
		var sum float64
		for _, c := range confs {
			sum += c
		}
		out = append(out, KindCoverage{
			Kind:          kind,
			Symbols:       n,
			AvgConfidence: sum / float64(n),
			P10:           confs[percentileIdx(n, 10)],
			P50:           confs[percentileIdx(n, 50)],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		// Stable, deterministic ordering by symbol count desc then kind name.
		if out[i].Symbols != out[j].Symbols {
			return out[i].Symbols > out[j].Symbols
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// percentileIdx returns the index into a sorted slice of length n that
// represents the p-th percentile (p in 0..100). Conservative on the lower
// side — at p=10 with n=1, returns index 0 (the only element).
func percentileIdx(n, p int) int {
	if n <= 1 {
		return 0
	}
	idx := (p * (n - 1)) / 100
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return idx
}

func formatStaleness(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// DeleteProject removes a project and ALL its per-project rows.
//
// #799: this must cover every table carrying a project_id, not just the
// obvious four. `closure` and `extraction_failures` carry a
// `REFERENCES projects(id)` FK — omitting them made the final
// `DELETE FROM projects` fail outright with "FOREIGN KEY constraint
// failed" for any project that had extraction failures or a built
// closure table. The unconstrained per-project tables (pending_edges,
// struct_fields, interface_methods, symbol_moves, slow_queries) didn't
// hard-fail but leaked orphan rows on every delete — a direct
// contributor to the #732 DB bloat. `projects` stays last so every FK
// child is gone first. (sessions / celebrations / hook_invocations are
// intentionally global, not per-project — not touched here.)
func (s *Store) DeleteProject(id string) error {
	return s.withTx(func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM edges               WHERE project_id=?`,
			`DELETE FROM symbols             WHERE project_id=?`,
			`DELETE FROM files               WHERE project_id=?`,
			`DELETE FROM adrs                WHERE project_id=?`,
			`DELETE FROM extraction_failures WHERE project_id=?`,
			`DELETE FROM closure             WHERE project_id=?`,
			`DELETE FROM pending_edges       WHERE project_id=?`,
			`DELETE FROM struct_fields       WHERE project_id=?`,
			`DELETE FROM interface_methods   WHERE project_id=?`,
			`DELETE FROM symbol_moves        WHERE project_id=?`,
			`DELETE FROM slow_queries        WHERE project_id=?`,
			`DELETE FROM projects            WHERE id=?`,
		} {
			if _, err := tx.Exec(q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteEmptyProjects removes every project with zero symbols AND zero edges
// (the "ghost" projects that accumulate from SessionStart hooks running in
// non-code directories). Returns the number of projects deleted.
//
// Safe to call alongside active indexing — rows with any symbols or edges
// are untouched, so a project still being populated won't be swept.
func (s *Store) DeleteEmptyProjects() (int, error) {
	var ids []string
	rows, err := s.db.Query(`SELECT id FROM projects WHERE sym_count = 0 AND edge_count = 0`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	for _, id := range ids {
		if err := s.DeleteProject(id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Symbol operations
// ─────────────────────────────────────────────────────────────────────────────

// BulkUpsertSymbols inserts or replaces symbols in a single transaction.
// FTS5 triggers fire automatically per row — no extra calls needed.
func (s *Store) BulkUpsertSymbols(syms []Symbol) error {
	if len(syms) == 0 {
		return nil
	}
	return s.withTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`
		INSERT OR REPLACE INTO symbols
			(id, project_id, file_path, name, qualified_name, kind, language,
			 start_byte, end_byte, start_line, end_line,
			 signature, return_type, docstring, parent,
			 complexity, is_exported, is_test, is_entry_point, file_hash,
			 extraction_confidence, branch)
		VALUES (?,?,?,?,?,?,?, ?,?,?,?, ?,?,?,?, ?,?,?,?,?, ?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := range syms {
			sym := &syms[i]
			conf := sym.ExtractionConfidence
			if conf == 0 {
				conf = 1.0 // default: exact (callers that don't set it are Go/AST)
			}
			_, err := stmt.Exec(
				sym.ID, sym.ProjectID, sym.FilePath, sym.Name, sym.QualifiedName, sym.Kind, sym.Language,
				sym.StartByte, sym.EndByte, sym.StartLine, sym.EndLine,
				ns(sym.Signature), ns(sym.ReturnType), ns(sym.Docstring), ns(sym.Parent),
				sym.Complexity, bi(sym.IsExported), bi(sym.IsTest), bi(sym.IsEntryPoint),
				ns(sym.FileHash), conf, sym.Branch,
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteSymbolsForFile removes all symbols (and edges) from one file.
//
// Cascade order: edges → struct_fields → interface_methods → symbols. Each
// cascade is a single set-based DELETE that filters via a subquery against
// the file's symbol IDs, rather than the pre-#1474 per-symbol loop that
// issued one parse-plan-exec for every (symbol × cascade-table) pair.
//
// Why this matters: CPU profiling of the cold/force Index() path showed
// ~42% of total time in SQLite statement parsing (`_sqlite3RunParser` +
// `_sqlite3Prepare` family) vs ~16% in actual execution. On a 700-file
// project the pre-fix code issued `1 + 3N` SQL parses per file (~30 with
// average ~10 symbols/file), totalling ~21,000 parses just for cleanup.
// Post-fix it issues exactly 4 parses per file regardless of symbol count,
// a 7.75× reduction in parse work.
//
// Correctness invariants preserved:
//   - All three cascade tables (edges / struct_fields / interface_methods)
//     are cleared before `symbols`, so foreign-key-style orphans cannot
//     appear mid-transaction.
//   - The non-correlated subquery is evaluated once by SQLite (constant
//     across the outer DELETE's row scan), so the symbols → cascade
//     ordering doesn't race with the symbols table being trimmed last.
//   - All four statements run inside the same withTx transaction; partial
//     failure rolls everything back.
func (s *Store) DeleteSymbolsForFile(projectID, filePath string) error {
	return s.withTx(func(tx *sql.Tx) error {
		// Cascade 1: edges referencing any symbol in this file (either
		// endpoint). The doubled subquery is necessary because edges has
		// no project_id column — from_id/to_id ARE the only join keys.
		if _, err := tx.Exec(
			`DELETE FROM edges
			   WHERE from_id IN (SELECT id FROM symbols WHERE project_id=? AND file_path=?)
			      OR to_id   IN (SELECT id FROM symbols WHERE project_id=? AND file_path=?)`,
			projectID, filePath, projectID, filePath,
		); err != nil {
			return err
		}
		// Cascade 2: struct_fields (#423 piece 2). Without this, every
		// file re-extraction would orphan its previous fields.
		if _, err := tx.Exec(
			`DELETE FROM struct_fields
			   WHERE project_id=? AND struct_id IN (
			     SELECT id FROM symbols WHERE project_id=? AND file_path=?
			   )`,
			projectID, projectID, filePath,
		); err != nil {
			return err
		}
		// Cascade 3: interface_methods (#493). Same rationale.
		if _, err := tx.Exec(
			`DELETE FROM interface_methods
			   WHERE project_id=? AND interface_id IN (
			     SELECT id FROM symbols WHERE project_id=? AND file_path=?
			   )`,
			projectID, projectID, filePath,
		); err != nil {
			return err
		}
		// Cascade 4: the file's symbols themselves.
		_, err := tx.Exec(`DELETE FROM symbols WHERE project_id=? AND file_path=?`, projectID, filePath)
		return err
	})
}

// GetSymbol returns a symbol by its stable ID, or nil if not found.
//
// **Cross-project caveat**: pre-#1 fix, the symbol_id format
// (`{file_path}::{qualified_name}#{kind}`) is not project-scoped, so two
// indexed projects with a `main.go::main.main#Function` collision can
// shadow each other under SQLite's `INSERT OR REPLACE` PK rule. Callers
// that have a project context — every server tool except `list` and
// `health` — should prefer `GetSymbolScoped(projectID, id)` so the
// returned row is verified to belong to the requested project.
//
// This unscoped variant remains for cases where the project is unknown
// (e.g. ID came from outside the active session) and for legacy
// internal callers; the scoped variant is the safer default for any
// MCP tool handler.
func (s *Store) GetSymbol(id string) (*Symbol, error) {
	// Reader pool (#51) — pure SELECT.
	row := s.ro.QueryRow(symSelectFrom+` WHERE id=?`, id)
	return scanOneSymbol(row)
}

// GetSymbolScoped returns a symbol by ID, but only if it belongs to the
// requested project. Returns (nil, nil) if no row matches both filters
// — same shape as GetSymbol's not-found case so callers don't need
// parallel error paths.
//
// Why both: the global symbol-ID format collides on identically-laid-out
// repos (two Go projects each with `cmd/main.go::main.main#Function`).
// Without project scoping, a `symbol` MCP request authenticated against
// project A could return project B's row whenever its ID happens to
// match. The scoped lookup is structural defence-in-depth that closes
// the leak even when the underlying ID is ambiguous (#2).
func (s *Store) GetSymbolScoped(projectID, id string) (*Symbol, error) {
	if projectID == "" {
		return nil, fmt.Errorf("GetSymbolScoped: projectID required (use GetSymbol when project is unknown)")
	}
	row := s.ro.QueryRow(symSelectFrom+` WHERE id=? AND project_id=?`, id, projectID)
	return scanOneSymbol(row)
}

// GetSymbolsByIDs returns rows for a batch of IDs in a single SQL round
// trip. When projectID is non-empty, also filters on project_id (the same
// defence-in-depth guard as GetSymbolScoped — an ID collision between two
// repos can't surface the wrong row). Returns a map keyed by ID; missing
// IDs simply don't appear in the result. Empty ids slice → empty map.
//
// Replaces the per-ID loop the MCP `symbols` batch handler used to do (one
// SELECT per ID) — for a 100-ID batch that's 100 round trips collapsed to 1,
// which dominates handler latency on cached corpora.
func (s *Store) GetSymbolsByIDs(projectID string, ids []string) (map[string]*Symbol, error) {
	out := make(map[string]*Symbol, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// Build `WHERE id IN (?, ?, …)` with one placeholder per ID. SQLite's
	// default expr-tree depth limit is 1000, so a few-hundred-ID batch is
	// well within bounds; the MCP layer additionally caps at 100 (#10).
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	q := symSelectFrom + ` WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	if projectID != "" {
		q += ` AND project_id=?`
		args = append(args, projectID)
	}
	syms, err := s.querySymbols(q, args...)
	if err != nil {
		return nil, err
	}
	for i := range syms {
		out[syms[i].ID] = &syms[i]
	}
	return out, nil
}

// GetSymbolsByName finds symbols by short name across a project.
func (s *Store) GetSymbolsByName(projectID, name string, limit int) ([]Symbol, error) {
	return s.querySymbols(symSelectFrom+` WHERE project_id=? AND name=? LIMIT ?`, projectID, name, limit)
}

// GetSymbolsByQN finds symbols by qualified name in a project.
func (s *Store) GetSymbolsByQN(projectID, qn string) ([]Symbol, error) {
	return s.querySymbols(symSelectFrom+` WHERE project_id=? AND qualified_name=?`, projectID, qn)
}

// LoadAllSymbolsByQN returns every symbol in projectID grouped by
// qualified_name. Used by the index resolve pass to avoid one DB
// query per unique QN — pre-#1338 the indexer ran N GetSymbolsByQN
// calls during resolveCalls/resolveReads, each a SQLite query plus
// row scan, accounting for ~20% of cold-path allocations. One bulk
// SELECT amortizes the cost.
//
// Ordering: within each value slice, symbols are sorted by id so
// downstream pickCanonical (#428) produces the same lexicographically
// smallest ID as the per-QN query did.
func (s *Store) LoadAllSymbolsByQN(projectID string) (map[string][]Symbol, error) {
	syms, err := s.querySymbols(symSelectFrom+` WHERE project_id=? ORDER BY qualified_name, id`, projectID)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]Symbol, len(syms)/2)
	for _, sym := range syms {
		out[sym.QualifiedName] = append(out[sym.QualifiedName], sym)
	}
	return out, nil
}

// GetSymbolsForFile returns all symbols in a file ordered by byte offset.
func (s *Store) GetSymbolsForFile(projectID, filePath string) ([]Symbol, error) {
	return s.querySymbols(symSelectFrom+` WHERE project_id=? AND file_path=? ORDER BY start_byte`, projectID, filePath)
}

// GetDeadCode returns symbols with no inbound edges of any kind
// (CALLS, REFERENCES, READS, WRITES, IMPORTS), filtered to internal
// callable symbols that *should* have callers — i.e., not exported
// (would be public API), not entry points (main/init), not test
// functions (test runners call them externally).
//
// Caller-supplied filters:
//   - kinds: SQL `IN`-list of symbol kinds. Pass nil/empty to default
//     to {"Function", "Method"}; the only kinds where in-graph
//     callers are extracted with high precision today.
//   - language: optional single-language filter. Pass empty to span
//     all languages — but be aware that regex-tier extractors (most
//     non-Go languages) under-resolve cross-file CALLS edges, so
//     dead-code results outside Go land at higher false-positive
//     rates. Default 0.95 confidence floor encodes this.
//   - minConfidence: extraction_confidence floor. 0.95 by default
//     to bias toward Go AST + JSON/YAML/HCL parser-backed
//     extractors. Drop to 0.0 to include regex-tier languages at
//     known false-positive cost.
//
// SQL uses NOT EXISTS rather than LEFT JOIN ... IS NULL so the
// edges table's (project_id, to_id) index dominates the plan.
func (s *Store) GetDeadCode(projectID string, kinds []string, language string, minConfidence float64, limit int) ([]Symbol, error) {
	if limit <= 0 {
		limit = 100
	}
	if len(kinds) == 0 {
		kinds = []string{"Function", "Method"}
	}
	q := `
		SELECT s.id, s.project_id, s.file_path, s.name, s.qualified_name, s.kind, s.language,
		       s.start_byte, s.end_byte, s.start_line, s.end_line,
		       s.signature, s.return_type, s.docstring, s.parent,
		       s.complexity, s.is_exported, s.is_test, s.is_entry_point, s.file_hash,
		       s.extraction_confidence, s.branch
		FROM symbols s
		WHERE s.project_id = ?
		  AND s.is_exported = 0
		  AND s.is_entry_point = 0
		  AND s.is_test = 0
		  AND s.extraction_confidence >= ?
		  AND s.kind IN (` + inPlaceholders(len(kinds)) + `)
		  AND NOT EXISTS (
		      SELECT 1 FROM edges e
		      WHERE e.project_id = s.project_id
		        AND e.to_id = s.id
		  )
		  -- #493: exclude Methods whose name matches any interface
		  -- method declared in the same project. Cheap heuristic
		  -- avoids the false-positive class where the only caller
		  -- goes through interface dispatch (invisible to the static
		  -- call graph). Over-includes — a Method named String gets
		  -- spared even if no interface in the project references it.
		  -- That direction is safer: dead_code suggesting deletion of
		  -- a method actually called via interface dispatch breaks
		  -- runtime silently.
		  AND NOT (
		      s.kind = 'Method' AND EXISTS (
		          SELECT 1 FROM interface_methods im
		          WHERE im.project_id = s.project_id
		            AND im.method_name = s.name
		      )
		  )`
	args := []any{projectID, minConfidence}
	for _, k := range kinds {
		args = append(args, k)
	}
	if language != "" {
		q += " AND s.language = ?"
		args = append(args, language)
	}
	q += " ORDER BY s.file_path, s.start_line LIMIT ?"
	args = append(args, limit)
	return s.querySymbols(q, args...)
}

// inPlaceholders returns "?,?,...?" with n placeholders. Local copy
// to avoid importing the cypher package (db is the lower layer).
func inPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	s := strings.Repeat("?,", n)
	return s[:len(s)-1]
}

// GetHotspots returns the most-called symbols (highest in-degree) for a project.
func (s *Store) GetHotspots(projectID string, limit int) ([]Symbol, error) {
	return s.querySymbols(`
		SELECT s.id, s.project_id, s.file_path, s.name, s.qualified_name, s.kind, s.language,
		       s.start_byte, s.end_byte, s.start_line, s.end_line,
		       s.signature, s.return_type, s.docstring, s.parent,
		       s.complexity, s.is_exported, s.is_test, s.is_entry_point, s.file_hash,
		       s.extraction_confidence, s.branch
		FROM symbols s
		JOIN (SELECT to_id, COUNT(*) AS cnt FROM edges WHERE project_id=? GROUP BY to_id) e ON s.id=e.to_id
		WHERE s.project_id=?
		ORDER BY cnt DESC LIMIT ?`, projectID, projectID, limit)
}

// ─────────────────────────────────────────────────────────────────────────────
// FTS5 Search (Layer 3)
// ─────────────────────────────────────────────────────────────────────────────

// SearchSymbols performs BM25-ranked full-text search.
// query uses FTS5 match syntax (e.g. "auth*", "login authenticate").
//
// Shim that delegates to SearchSymbolsByCorpus with an empty corpus —
// which means the **code** corpus. Callers that need config/docs
// results pass an explicit `corpus=config` / `corpus=docs` (the legacy
// `corpus=all` mixed index was removed in #106).
func (s *Store) SearchSymbols(projectID, query, kind, language string, limit int) ([]SearchResult, error) {
	return s.SearchSymbolsByCorpus(projectID, query, kind, language, "", limit)
}

// SearchSymbolsByCorpus performs BM25-ranked full-text search against a
// specific corpus index (#32).
//
// corpus parameter:
//   - ""        → `symbols_code_fts` (default — same as "code")
//   - "code"    → `symbols_code_fts`   (Function/Method/Class/etc)
//   - "config"  → `symbols_config_fts` (YAML/JSON/HCL Settings, Resources, etc)
//   - "docs"    → `symbols_docs_fts`   (Markdown sections, Documents)
//
// Anything else returns an error so a typo doesn't silently fall back to
// the wrong index. The corpus → vtab mapping mirrors ClassifyCorpus +
// the v9 trigger routing.
//
// **Legacy `corpus=all` removed in #106**. The MCP search handler
// soft-redirects `corpus=all` to `corpus=code` for backwards compat
// with older callers; this function rejects the literal value.
func (s *Store) SearchSymbolsByCorpus(projectID, query, kind, language, corpus string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	vtab, err := corpusVtab(corpus)
	if err != nil {
		return nil, err
	}
	q := `
		SELECT s.id, s.project_id, s.file_path, s.name, s.qualified_name, s.kind, s.language,
		       s.start_byte, s.end_byte, s.start_line, s.end_line,
		       s.signature, s.return_type, s.docstring, s.parent,
		       s.complexity, s.is_exported, s.is_test, s.is_entry_point, s.file_hash,
		       s.extraction_confidence, s.branch,
		       bm25(` + vtab + `) AS score
		FROM ` + vtab + `
		JOIN symbols s ON s.rowid = ` + vtab + `.rowid
		WHERE ` + vtab + ` MATCH ?`
	args := []any{query}
	if projectID != "" {
		q += " AND s.project_id = ?"
		args = append(args, projectID)
	}
	if kind != "" {
		q += " AND s.kind = ?"
		args = append(args, kind)
	}
	if language != "" {
		q += " AND s.language = ?"
		args = append(args, language)
	}
	// qualified_name tiebreak makes ranking deterministic when BM25 scores
	// tie. Without it, ordering depends on rowid, which is set by concurrent
	// indexer insertion order and varies per run — flaky for snapshot tests
	// and surprising for users who rely on stable pagination.
	q += " ORDER BY score, s.qualified_name LIMIT ?"
	args = append(args, limit)

	// Reader pool (#51) — FTS5 MATCH + JOIN is read-only.
	rows, err := s.ro.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var sym Symbol
		var score float64
		if err := scanSymbolRow(rows, &sym, &score); err != nil {
			return nil, err
		}
		results = append(results, SearchResult{Symbol: sym, Score: -score}) // negate: lower bm25 = better
	}
	return results, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// Edge operations (Knowledge Graph — Layer 2)
// ─────────────────────────────────────────────────────────────────────────────

// BulkUpsertEdges inserts edges, ignoring duplicates. Empty Source
// is normalized to "per_file" — the default for the per-file extractor
// path. The resolve-pass callers set "resolve_pass" explicitly (#475).
func (s *Store) BulkUpsertEdges(edges []Edge) error {
	if len(edges) == 0 {
		return nil
	}
	return s.withTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO edges(project_id, from_id, to_id, kind, confidence, properties, source, branch)
		VALUES (?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := range edges {
			e := &edges[i]
			propsJSON := ""
			if len(e.Properties) > 0 {
				b, _ := json.Marshal(e.Properties)
				propsJSON = string(b)
			}
			source := e.Source
			if source == "" {
				source = "per_file"
			}
			if _, err := stmt.Exec(e.ProjectID, e.FromID, e.ToID, e.Kind, e.Confidence, ns(propsJSON), source, e.Branch); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteEdgesByKindAndSource removes all edges of a given kind+source
// for a project. Used by resolveCalls/resolveImports/resolveReads
// (#475) to atomically clear the prior pass's output before re-running
// resolution with current rules. Per-file edges are not touched.
func (s *Store) DeleteEdgesByKindAndSource(projectID, kind, source string) error {
	return s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`DELETE FROM edges WHERE project_id=? AND kind=? AND source=?`,
			projectID, kind, source,
		)
		return err
	})
}

// DeleteResolvePassEdgesByKindForSourceFiles deletes only the
// resolve_pass edges of the given kind whose from-side symbol lives
// in one of the provided source files (#1629 v0.87). Convenience
// wrapper around DeleteEdgesByKindAndSourceForSourceFiles with
// source="resolve_pass".
func (s *Store) DeleteResolvePassEdgesByKindForSourceFiles(projectID, kind string, files []string) error {
	return s.DeleteEdgesByKindAndSourceForSourceFiles(projectID, kind, "resolve_pass", files)
}

// DeleteEdgesByKindAndSourceForSourceFiles deletes edges of (kind,
// source) whose from-side symbol lives in one of the provided source
// files (#1629 v0.87 slice 2). Used by the incremental resolve path
// on watcher ticks: when only a few files re-extracted, we resolve
// only their pending edges and must wipe only THEIR prior resolved
// edges — wiping everything would drop the resolved edges from
// unchanged files that we're NOT going to rebuild this pass.
//
// Source values in use: "resolve_pass" (resolveImports / resolveCalls
// / resolveReads / resolveUsesVar) and "binding_pass" (function-value
// binding CALLS produced by resolveReads).
//
// Empty files list is a no-op. Returns nil on success. Writer-routed.
//
// The deletion joins `edges.from_id` to `symbols.id` to recover the
// source file_path, since `edges` doesn't store it directly. The
// `symbols` table's `file_path` column is the canonical source.
func (s *Store) DeleteEdgesByKindAndSourceForSourceFiles(projectID, kind, source string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	placeholders := make([]string, len(files))
	args := []any{projectID, kind, source, projectID}
	for i, f := range files {
		placeholders[i] = "?"
		args = append(args, f)
	}
	query := `DELETE FROM edges
		WHERE project_id = ? AND kind = ? AND source = ?
		  AND from_id IN (
		    SELECT id FROM symbols
		    WHERE project_id = ? AND file_path IN (` + strings.Join(placeholders, ",") + `)
		  )`
	return s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(query, args...)
		return err
	})
}

// LoadPendingEdgesByKindAndFiles returns pending_edges rows filtered
// to a specific set of source files (#1629 v0.87). Used by the
// incremental resolve path on watcher ticks — load only the pending
// edges that come from the files re-extracted this run, rather than
// the full project-wide pool. On pincher-repo v0.85 measurement, the
// full-pool load + walk dominated the 700ms incremental tick cost;
// scoping to a single-file edit cuts that to ~100 rows.
//
// Empty files list returns nil, nil (no pending edges of this kind
// from no files). Reader-routed.
func (s *Store) LoadPendingEdgesByKindAndFiles(projectID, kind string, files []string) ([]PendingEdge, error) {
	if len(files) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(files))
	args := []any{projectID, kind}
	for i, f := range files {
		placeholders[i] = "?"
		args = append(args, f)
	}
	query := `SELECT project_id, from_file, kind, from_qn, to_name, confidence, receiver_type, base_type
		FROM pending_edges
		WHERE project_id = ? AND kind = ?
		  AND from_file IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.ro.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingEdge
	for rows.Next() {
		var e PendingEdge
		if err := rows.Scan(&e.ProjectID, &e.FromFile, &e.Kind, &e.FromQN, &e.ToName, &e.Confidence, &e.ReceiverType, &e.BaseType); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PendingEdge is a per-file deferred edge candidate persisted in the
// pending_edges table (#457). Re-resolution after an incremental
// watcher tick sources the FULL set from this table, so edges from
// hash-skipped files to changed files no longer get dropped (#427).
//
// FromQN is the caller's qualified name (already known at extraction
// time — guaranteed in-file). ToName is whatever the extractor saw
// at the call site — may be a bare name, a qualified name, or a
// `receiver.method` pair. The resolver does the lookup.
type PendingEdge struct {
	ProjectID  string
	FromFile   string
	Kind       string // CALLS | IMPORTS | READS | WRITES
	FromQN     string
	ToName     string
	Confidence float64
	// ReceiverType is set (in schema v22+) when this candidate was
	// extracted from inside a Go method body — the method's receiver
	// type expression, e.g. "*Supervisor". The piece-3 resolver uses
	// it to follow recv.field.method calls via struct_fields. Empty
	// for plain functions, non-Go languages, and pre-v22 rows.
	ReceiverType string
	// BaseType is set (in schema v26+) on a Go READS candidate whose
	// source AST node was a non-package selector `base.Sel` and whose
	// base's declared type the extractor resolved — the type as
	// written, stripped of leading `*` and `[]`. The #760 binding pass
	// uses it to suppress the false CALLS edge from a struct-field read
	// (`e.Confidence`) to a same-named Method. Empty otherwise.
	BaseType string
}

// ReplacePendingEdgesForFile atomically deletes any existing
// pending_edges rows for (project_id, from_file) and inserts the
// caller's new set. INSERT OR IGNORE on the UNIQUE constraint —
// duplicates within the input set silently dedup. Called by the
// indexer's per-file goroutine after a successful extraction.
//
// On a hash-skipped file the indexer never re-extracts, so this is
// not called, and the existing rows remain — that's the whole point
// of persistence (#457).
func (s *Store) ReplacePendingEdgesForFile(projectID, fromFile string, edges []PendingEdge) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM pending_edges WHERE project_id=? AND from_file=?`, projectID, fromFile); err != nil {
			return err
		}
		if len(edges) == 0 {
			return nil
		}
		stmt, err := tx.Prepare(`
			INSERT OR IGNORE INTO pending_edges(project_id, from_file, kind, from_qn, to_name, confidence, receiver_type, base_type)
			VALUES (?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := range edges {
			e := &edges[i]
			if _, err := stmt.Exec(projectID, fromFile, e.Kind, e.FromQN, e.ToName, e.Confidence, e.ReceiverType, e.BaseType); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeletePendingEdgesForFile is the GC hook for files removed from
// disk (#326 tail-pass) — without it, rows from a deleted file's
// last successful extraction would linger forever and re-resolve as
// dangling candidates. Writer-routed (mutates).
func (s *Store) DeletePendingEdgesForFile(projectID, fromFile string) error {
	_, err := s.db.Exec(`DELETE FROM pending_edges WHERE project_id=? AND from_file=?`, projectID, fromFile)
	return err
}

// StructField is one row of the v22 struct_fields table — the
// receiver-type resolver's lookup target (#423). For each Go struct
// symbol the indexer writes one row per field; the resolver reads
// (struct_id, field_name) → field_type to follow `recv.field.method`
// calls.
type StructField struct {
	ProjectID string
	StructID  string // db.MakeSymbolID for the Class symbol
	FieldName string
	FieldType string // Go-syntax type expression (e.g. "io.Writer", "*exec.Cmd")
}

// ReplaceStructFieldsForFile mirrors ReplacePendingEdgesForFile's
// pattern: atomically delete every struct_fields row for symbols
// in (project_id, file_path) then INSERT the freshly extracted set.
// Called by the indexer's per-file goroutine after extraction.
//
// The DELETE is keyed by file path via a join on `symbols`, not by
// struct_id directly, so a struct that gets renamed or removed in
// this file's edit also clears its rows. Writer-routed (mutates).
func (s *Store) ReplaceStructFieldsForFile(projectID, filePath string, fields []StructField) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`DELETE FROM struct_fields
			   WHERE project_id=? AND struct_id IN (
			     SELECT id FROM symbols WHERE project_id=? AND file_path=?
			   )`,
			projectID, projectID, filePath,
		); err != nil {
			return err
		}
		if len(fields) == 0 {
			return nil
		}
		stmt, err := tx.Prepare(
			`INSERT OR REPLACE INTO struct_fields(project_id, struct_id, field_name, field_type)
			 VALUES (?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := range fields {
			f := &fields[i]
			if _, err := stmt.Exec(projectID, f.StructID, f.FieldName, f.FieldType); err != nil {
				return err
			}
		}
		return nil
	})
}

// LoadStructFields returns every struct_fields row for the project.
// The resolver loads them once per pass and indexes them in memory:
// map[StructID]map[FieldName]FieldType. Reader-routed — pure SELECT.
func (s *Store) LoadStructFields(projectID string) ([]StructField, error) {
	rows, err := s.ro.Query(
		`SELECT project_id, struct_id, field_name, field_type
		 FROM struct_fields WHERE project_id=?`,
		projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StructField
	for rows.Next() {
		var f StructField
		if err := rows.Scan(&f.ProjectID, &f.StructID, &f.FieldName, &f.FieldType); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// InterfaceMethod is one row of the v23 interface_methods table —
// the dead_code interface-reachability lookup target (#493). For
// each Go interface symbol the indexer writes one row per declared
// method name. The dead_code query joins against this table to
// exclude project-internal methods whose name matches any
// interface method name.
type InterfaceMethod struct {
	ProjectID   string
	InterfaceID string // db.MakeSymbolID for the Interface symbol
	MethodName  string
}

// ReplaceInterfaceMethodsForFile mirrors ReplaceStructFieldsForFile.
// Atomically delete every interface_methods row for symbols in
// (project_id, file_path) then INSERT the freshly extracted set.
// Writer-routed.
func (s *Store) ReplaceInterfaceMethodsForFile(projectID, filePath string, methods []InterfaceMethod) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`DELETE FROM interface_methods
			   WHERE project_id=? AND interface_id IN (
			     SELECT id FROM symbols WHERE project_id=? AND file_path=?
			   )`,
			projectID, projectID, filePath,
		); err != nil {
			return err
		}
		if len(methods) == 0 {
			return nil
		}
		stmt, err := tx.Prepare(
			`INSERT OR REPLACE INTO interface_methods(project_id, interface_id, method_name)
			 VALUES (?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := range methods {
			m := &methods[i]
			if _, err := stmt.Exec(projectID, m.InterfaceID, m.MethodName); err != nil {
				return err
			}
		}
		return nil
	})
}

// FileExtractionCommit groups every per-file post-extract write into a
// single payload so the writer pool sees one transaction per file
// instead of four (#1627 v0.86). v0.85 observability showed
// extraction-phase wall-clock is 73% writer-pool serialization, not
// extractor CPU — collapsing four separate Begin/Commit cycles per
// file into one removes the dominant source of contention.
//
// Empty slices skip their DELETE+INSERT pass cleanly (no-op for files
// that produce zero pending edges / struct fields / interface methods).
// FileHash is always written — that's how the next-pass hash-check
// skips unchanged files.
type FileExtractionCommit struct {
	ProjectID        string
	FilePath         string  // path relative to project root, forward-slash
	FileHash         string  // xxh3 of file bytes; written unconditionally
	PendingEdges     []PendingEdge
	StructFields     []StructField
	InterfaceMethods []InterfaceMethod
}

// CommitFileExtraction does in one transaction what
// ReplacePendingEdgesForFile + ReplaceStructFieldsForFile +
// ReplaceInterfaceMethodsForFile + SetFileHash previously did in four.
// Atomicity is strengthened (all-or-nothing per file vs four
// independent commits); the writer pool sees one Begin/Commit cycle
// per file vs four (#1627 v0.86).
//
// The individual methods stay for direct callers and tests; the
// indexer's per-file goroutine routes through this to amortize the
// writer-mutex acquisition cost. Writer-routed (mutates).
func (s *Store) CommitFileExtraction(c FileExtractionCommit) error {
	return s.withTx(func(tx *sql.Tx) error {
		// 1. pending_edges — delete then INSERT OR IGNORE the fresh
		//    set. Mirrors ReplacePendingEdgesForFile.
		if _, err := tx.Exec(
			`DELETE FROM pending_edges WHERE project_id=? AND from_file=?`,
			c.ProjectID, c.FilePath,
		); err != nil {
			return err
		}
		if len(c.PendingEdges) > 0 {
			stmt, err := tx.Prepare(`
				INSERT OR IGNORE INTO pending_edges(project_id, from_file, kind, from_qn, to_name, confidence, receiver_type, base_type)
				VALUES (?,?,?,?,?,?,?,?)`)
			if err != nil {
				return err
			}
			for i := range c.PendingEdges {
				e := &c.PendingEdges[i]
				if _, err := stmt.Exec(c.ProjectID, c.FilePath, e.Kind, e.FromQN, e.ToName, e.Confidence, e.ReceiverType, e.BaseType); err != nil {
					stmt.Close()
					return err
				}
			}
			stmt.Close()
		}

		// 2. struct_fields — same shape as ReplaceStructFieldsForFile.
		//    DELETE keyed by file path via a join on `symbols`.
		if _, err := tx.Exec(
			`DELETE FROM struct_fields
			   WHERE project_id=? AND struct_id IN (
			     SELECT id FROM symbols WHERE project_id=? AND file_path=?
			   )`,
			c.ProjectID, c.ProjectID, c.FilePath,
		); err != nil {
			return err
		}
		if len(c.StructFields) > 0 {
			stmt, err := tx.Prepare(
				`INSERT OR REPLACE INTO struct_fields(project_id, struct_id, field_name, field_type)
				 VALUES (?,?,?,?)`)
			if err != nil {
				return err
			}
			for i := range c.StructFields {
				f := &c.StructFields[i]
				if _, err := stmt.Exec(c.ProjectID, f.StructID, f.FieldName, f.FieldType); err != nil {
					stmt.Close()
					return err
				}
			}
			stmt.Close()
		}

		// 3. interface_methods — same shape as
		//    ReplaceInterfaceMethodsForFile.
		if _, err := tx.Exec(
			`DELETE FROM interface_methods
			   WHERE project_id=? AND interface_id IN (
			     SELECT id FROM symbols WHERE project_id=? AND file_path=?
			   )`,
			c.ProjectID, c.ProjectID, c.FilePath,
		); err != nil {
			return err
		}
		if len(c.InterfaceMethods) > 0 {
			stmt, err := tx.Prepare(
				`INSERT OR REPLACE INTO interface_methods(project_id, interface_id, method_name)
				 VALUES (?,?,?)`)
			if err != nil {
				return err
			}
			for i := range c.InterfaceMethods {
				m := &c.InterfaceMethods[i]
				if _, err := stmt.Exec(c.ProjectID, m.InterfaceID, m.MethodName); err != nil {
					stmt.Close()
					return err
				}
			}
			stmt.Close()
		}

		// 4. files.hash — mirrors SetFileHash. Written unconditionally
		//    so an empty-extraction file still records the hash to
		//    skip re-extraction next pass.
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO files(project_id, path, hash, indexed_at) VALUES (?,?,?,?)`,
			c.ProjectID, c.FilePath, c.FileHash, time.Now().Unix(),
		); err != nil {
			return err
		}
		return nil
	})
}

// LoadInterfaceMethods returns every interface_methods row for the
// project. The dead_code query uses it directly via SQL JOIN; this
// reader is here for tests + future heuristics. Reader-routed.
func (s *Store) LoadInterfaceMethods(projectID string) ([]InterfaceMethod, error) {
	rows, err := s.ro.Query(
		`SELECT project_id, interface_id, method_name
		 FROM interface_methods WHERE project_id=?`,
		projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InterfaceMethod
	for rows.Next() {
		var m InterfaceMethod
		if err := rows.Scan(&m.ProjectID, &m.InterfaceID, &m.MethodName); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LoadPendingEdges returns every persisted candidate for the project
// of the given kind. Reader-routed — pure SELECT.
func (s *Store) LoadPendingEdges(projectID, kind string) ([]PendingEdge, error) {
	rows, err := s.ro.Query(
		`SELECT project_id, from_file, kind, from_qn, to_name, confidence, receiver_type, base_type
		 FROM pending_edges WHERE project_id=? AND kind=?`,
		projectID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingEdge
	for rows.Next() {
		var e PendingEdge
		if err := rows.Scan(&e.ProjectID, &e.FromFile, &e.Kind, &e.FromQN, &e.ToName, &e.Confidence, &e.ReceiverType, &e.BaseType); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EdgesFrom returns all edges originating from a symbol ID.
func (s *Store) EdgesFrom(fromID string, kinds []string) ([]Edge, error) {
	return s.queryEdges("from_id", fromID, kinds)
}

// EdgesTo returns all edges pointing to a symbol ID.
func (s *Store) EdgesTo(toID string, kinds []string) ([]Edge, error) {
	return s.queryEdges("to_id", toID, kinds)
}

func (s *Store) queryEdges(col, id string, kinds []string) ([]Edge, error) {
	q := `SELECT id, project_id, from_id, to_id, kind, confidence, properties FROM edges WHERE ` + col + `=?`
	args := []any{id}
	if len(kinds) > 0 {
		in := ""
		for i, k := range kinds {
			if i > 0 {
				in += ","
			}
			in += "?"
			args = append(args, k)
		}
		q += " AND kind IN (" + in + ")"
	}
	// Reader pool (#51) — covers EdgesFrom + EdgesTo.
	rows, err := s.ro.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		var propsStr sql.NullString
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.FromID, &e.ToID, &e.Kind, &e.Confidence, &propsStr); err != nil {
			return nil, err
		}
		if propsStr.Valid && propsStr.String != "" {
			_ = json.Unmarshal([]byte(propsStr.String), &e.Properties)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// Symbol move tracking — stale ID resolution
// ─────────────────────────────────────────────────────────────────────────────

// RecordSymbolMove persists an old_id → new_id mapping so agents holding
// stale IDs from before a file move can still resolve them.
func (s *Store) RecordSymbolMove(projectID, oldID, newID string) error {
	_, err := s.db.Exec(
		`INSERT INTO symbol_moves(old_id, new_id, project_id, moved_at)
		 VALUES (?,?,?,?)
		 ON CONFLICT(old_id, project_id) DO UPDATE SET new_id=excluded.new_id, moved_at=excluded.moved_at`,
		oldID, newID, projectID, time.Now().Unix(),
	)
	return err
}

// ResolveStaleID looks up a new ID for a stale symbol ID.
// Returns (newID, true) if a redirect exists, ("", false) otherwise.
func (s *Store) ResolveStaleID(projectID, oldID string) (string, bool) {
	var newID string
	err := s.db.QueryRow(
		`SELECT new_id FROM symbol_moves WHERE old_id=? AND project_id=?`,
		oldID, projectID,
	).Scan(&newID)
	if err != nil {
		return "", false
	}
	return newID, true
}

// DetectAndRecordMoves checks whether any of the incoming symbols previously
// existed under a different ID (same qualified_name + kind, different file_path).
// When a match is found it records old_id → new_id in symbol_moves.
// Non-fatal: errors are returned but callers typically log and continue.
func (s *Store) DetectAndRecordMoves(projectID string, newSyms []Symbol) error {
	return s.withTx(func(tx *sql.Tx) error {
		for i := range newSyms {
			sym := &newSyms[i]
			var oldID string
			err := tx.QueryRow(
				`SELECT id FROM symbols WHERE project_id=? AND qualified_name=? AND kind=? AND id != ?`,
				projectID, sym.QualifiedName, sym.Kind, sym.ID,
			).Scan(&oldID)
			if err != nil {
				continue // includes sql.ErrNoRows (no prior symbol at this QN)
			}
			_, _ = tx.Exec(
				`INSERT INTO symbol_moves(old_id, new_id, project_id, moved_at)
			 VALUES (?,?,?,?)
			 ON CONFLICT(old_id, project_id) DO UPDATE SET new_id=excluded.new_id, moved_at=excluded.moved_at`,
				oldID, sym.ID, projectID, time.Now().Unix(),
			)
		}
		return nil
	})
}

// TraceResult is one hop returned by TraceViaCTE.
type TraceResult struct {
	SymbolID string
	Depth    int
	ViaKind  string
}

// TraceViaCTE returns all symbols reachable from startID within maxDepth steps
// using a single recursive CTE per direction (max 2 SQL calls for "both").
// direction: "outbound" | "inbound" | "both"
// Results are deduplicated: each symbol ID appears once at its minimum depth.
//
// **Cross-project caveat**: same as `GetSymbol`, this unscoped variant
// traverses every edge whose endpoints match `startID` regardless of
// which project owns them. With the pre-#1 global symbol-ID format,
// that means a trace can hop into a sibling project if their IDs
// collide. Callers with a project context should prefer
// `TraceViaCTEScoped`; this unscoped form is preserved for legacy
// callers and for the "no project" mode where the trace is genuinely
// cross-corpus.
func (s *Store) TraceViaCTE(startID, direction string, edgeKinds []string, maxDepth int) ([]TraceResult, error) {
	return s.traceViaCTE("", startID, direction, edgeKinds, maxDepth)
}

// TraceViaCTEScoped is TraceViaCTE with the recursive edge join filtered
// to a single project. Catches the cross-project leak that the global
// symbol-ID format opens up: if two indexed repos collide on a symbol
// ID (`cmd/main.go::main.main#Function` is a real-world dupe in the
// dogfood corpus), the unscoped trace would walk edges across the
// boundary. Pass an empty projectID to fall back to the unscoped path
// when the caller deliberately wants cross-project traversal.
func (s *Store) TraceViaCTEScoped(projectID, startID, direction string, edgeKinds []string, maxDepth int) ([]TraceResult, error) {
	return s.traceViaCTE(projectID, startID, direction, edgeKinds, maxDepth)
}

func (s *Store) traceViaCTE(projectID, startID, direction string, edgeKinds []string, maxDepth int) ([]TraceResult, error) {
	if len(edgeKinds) == 0 {
		return nil, fmt.Errorf("TraceViaCTE: edgeKinds must not be empty")
	}
	in := strings.Repeat("?,", len(edgeKinds))
	in = in[:len(in)-1]

	byID := make(map[string]TraceResult)

	runDir := func(dir string) error {
		var joinCond string
		if dir == "outbound" {
			joinCond = "e.from_id = r.id"
		} else {
			joinCond = "e.to_id = r.id"
		}
		var selectNeighbor string
		if dir == "outbound" {
			selectNeighbor = "e.to_id"
		} else {
			selectNeighbor = "e.from_id"
		}

		// projectID="" → unscoped (legacy). Non-empty → add the project
		// filter to the recursive join so traversal can't escape the
		// caller's project.
		projectFilter := ""
		if projectID != "" {
			projectFilter = " AND e.project_id = ?"
		}
		q := `WITH RECURSIVE reach(id, depth, via) AS (
			SELECT ?, 0, ''
			UNION ALL
			SELECT ` + selectNeighbor + `, r.depth + 1, e.kind
			FROM reach r
			JOIN edges e ON ` + joinCond + ` AND e.kind IN (` + in + `)` + projectFilter + `
			WHERE r.depth < ?
		)
		SELECT id, MIN(depth) AS depth, MIN(via) AS via
		FROM reach
		WHERE id != ? AND depth > 0
		GROUP BY id
		ORDER BY MIN(depth)
		LIMIT 500`

		args := []any{startID}
		for _, k := range edgeKinds {
			args = append(args, k)
		}
		if projectID != "" {
			args = append(args, projectID)
		}
		args = append(args, maxDepth, startID)

		rows, err := s.db.Query(q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r TraceResult
			if err := rows.Scan(&r.SymbolID, &r.Depth, &r.ViaKind); err != nil {
				return err
			}
			// Keep minimum depth when merging outbound + inbound
			if existing, ok := byID[r.SymbolID]; !ok || r.Depth < existing.Depth {
				byID[r.SymbolID] = r
			}
		}
		return rows.Err()
	}

	if direction == "outbound" || direction == "both" {
		if err := runDir("outbound"); err != nil {
			return nil, err
		}
	}
	if direction == "inbound" || direction == "both" {
		if err := runDir("inbound"); err != nil {
			return nil, err
		}
	}

	out := make([]TraceResult, 0, len(byID))
	for _, r := range byID {
		out = append(out, r)
	}
	return out, nil
}

// GraphStats returns node and edge counts grouped by kind.
//
// Hot path: PR #28's throttled refresh calls this every 5s during a
// long Index() run, plus dashboard / list / architecture tools call
// it on every request. Reader-pool routing (#51) means these calls
// don't queue behind the active write transaction.
func (s *Store) GraphStats(projectID string) (symCount, edgeCount int, kindCounts, edgeKindCounts map[string]int, err error) {
	kindCounts = make(map[string]int)
	edgeKindCounts = make(map[string]int)

	if err = s.ro.QueryRow(`SELECT COUNT(*) FROM symbols WHERE project_id=?`, projectID).Scan(&symCount); err != nil {
		return
	}
	if err = s.ro.QueryRow(`SELECT COUNT(*) FROM edges WHERE project_id=?`, projectID).Scan(&edgeCount); err != nil {
		return
	}

	rows, err2 := s.ro.Query(`SELECT kind, COUNT(*) FROM symbols WHERE project_id=? GROUP BY kind`, projectID)
	if err2 != nil {
		err = err2
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var c int
		if err = rows.Scan(&k, &c); err != nil {
			return
		}
		kindCounts[k] = c
	}
	if err = rows.Err(); err != nil {
		return
	}

	erows, err3 := s.ro.Query(`SELECT kind, COUNT(*) FROM edges WHERE project_id=? GROUP BY kind`, projectID)
	if err3 != nil {
		err = err3
		return
	}
	defer erows.Close()
	for erows.Next() {
		var k string
		var c int
		if err = erows.Scan(&k, &c); err != nil {
			return
		}
		edgeKindCounts[k] = c
	}
	err = erows.Err()
	return
}

// AvgConfidenceByKind returns the average extraction_confidence per symbol
// kind for a project. Used by the corpus-snapshot tooling (#33) to track
// signal-quality drift over time. Empty map on a project with no symbols.
func (s *Store) AvgConfidenceByKind(projectID string) (map[string]float64, error) {
	out := map[string]float64{}
	// Reader pool (#51) — pure aggregation read.
	rows, err := s.ro.Query(
		`SELECT kind, AVG(extraction_confidence) FROM symbols
		 WHERE project_id=? GROUP BY kind`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var avg float64
		if err := rows.Scan(&k, &avg); err != nil {
			return nil, err
		}
		out[k] = avg
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// Extraction failures (#42 part 1) — diagnostic surface
// ─────────────────────────────────────────────────────────────────────────────

// ExtractionFailure is one row from the extraction_failures table — a file
// the indexer couldn't fully process, plus a reason and human-readable details.
//
// Reasons are stable, machine-readable strings:
//   - "parse_error"           — the language extractor returned an error
//   - "extractor_panicked"    — the extractor panicked; recover() caught it
//   - "byte_range_negative"   — sanity heuristic: a symbol's end_byte <= start_byte
//   - "qualified_name_collision" — sanity heuristic: same QN twice in a file
//   - "file_too_large"        — file size exceeds the indexer's per-file cap (#111).
//                                Skipped before read so memory stays bounded; details
//                                holds the size in bytes and the configured cap.
//
// New reasons can be added by future PRs (e.g. "byte_range_oversized" once
// parent-tracking lands, "confidence_outlier" once #34 ships).
type ExtractionFailure struct {
	ID          int64
	ProjectID   string
	FilePath    string
	Language    string
	Reason      string
	Details     string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	// #1421 (v33): the pincher binary version that produced this
	// failure. Empty when the row was written before the
	// binary_version_at_failure column was added (v33 migration
	// default '') OR when the indexer caller didn't have a
	// version stamp (best-effort — the fixed-row case where this
	// matters is the bulk of writes). Doctor uses this to
	// distinguish "stale failure that a later binary fixed" from
	// "current binary still produces this failure."
	BinaryVersionAtFailure string
}

// extractionFailureDetailsCap caps the persisted details field. The first
// line of an error message is plenty of context; bigger payloads waste DB
// space and clutter the failure report.
const extractionFailureDetailsCap = 1024

// RecordExtractionFailure persists a failure row. Idempotent on
// (project_id, file_path, reason): re-recording the same failure updates
// last_seen_at instead of inserting a duplicate.
//
// details is truncated to extractionFailureDetailsCap characters before
// insert. Pass "" if there's no useful detail (the reason alone is the
// signal).
func (s *Store) RecordExtractionFailure(projectID, filePath, language, reason, details string) error {
	// #1421: legacy thin-wrapper for callers without binary-version
	// context (mostly tests). The indexer uses
	// RecordExtractionFailureWithBinary so production rows carry
	// provenance.
	return s.RecordExtractionFailureWithBinary(projectID, filePath, language, reason, details, "")
}

// RecordExtractionFailureWithBinary is RecordExtractionFailure plus
// the binary version that produced the failure. Stamped on every
// indexer write so doctor can later distinguish stale failures
// (older binary, fixed-since) from recurring failures (current
// binary still fails on these files). Empty binaryVersion is
// permitted — the column accepts NULL/'' for legacy callers.
//
// Idempotent on (project_id, file_path, reason): re-recording the
// same failure updates details, last_seen_at, AND
// binary_version_at_failure — the latter is the key signal,
// because a row updated by the CURRENT binary's pass means the
// failure is recurring under that binary. A row whose
// binary_version_at_failure lags behind project's binary_version
// is stale.
func (s *Store) RecordExtractionFailureWithBinary(projectID, filePath, language, reason, details, binaryVersion string) error {
	if len(details) > extractionFailureDetailsCap {
		details = details[:extractionFailureDetailsCap] + "…[truncated]"
	}
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO extraction_failures (project_id, file_path, language, reason, details, first_seen_at, last_seen_at, binary_version_at_failure)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, file_path, reason) DO UPDATE SET
			details                   = excluded.details,
			last_seen_at              = excluded.last_seen_at,
			binary_version_at_failure = excluded.binary_version_at_failure`,
		projectID, filePath, language, reason, details, now, now, binaryVersion)
	return err
}

// PruneExtractionFailuresForFile deletes extraction_failures rows for
// (projectID, filePath) whose reason is NOT in keepReasons. Used by the
// indexer after a per-file extraction completes — every reason that
// would have fired this pass is in keepReasons; anything else in the
// table is stale evidence from a prior buggy state and should not
// continue to pollute doctor counts, snapshot gates, or dashboards.
//
// #1319 v0.71. Pre-fix the only purge was per-project on project delete
// (db.go:2746); fixed-but-stale rows accumulated indefinitely. User
// repro: README.md `qualified_name_collision` row 8 days old after
// #1207's Markdown suppression made that diagnostic stop firing.
//
// Passing keepReasons=nil deletes ALL rows for the file (the "extraction
// re-ran cleanly with zero failures" case).
//
// Writes via the writer pool — single-writer SQLite, classified in
// writerRoutedStoreMethods.
func (s *Store) PruneExtractionFailuresForFile(projectID, filePath string, keepReasons map[string]struct{}) error {
	if len(keepReasons) == 0 {
		_, err := s.db.Exec(
			`DELETE FROM extraction_failures WHERE project_id = ? AND file_path = ?`,
			projectID, filePath)
		return err
	}
	args := []any{projectID, filePath}
	placeholders := make([]string, 0, len(keepReasons))
	for r := range keepReasons {
		placeholders = append(placeholders, "?")
		args = append(args, r)
	}
	q := fmt.Sprintf(
		`DELETE FROM extraction_failures
		   WHERE project_id = ? AND file_path = ? AND reason NOT IN (%s)`,
		strings.Join(placeholders, ","))
	_, err := s.db.Exec(q, args...)
	return err
}

// ListExtractionFailures returns the most-recent extraction failures for a
// project, ordered by last_seen_at DESC. limit <= 0 returns all rows.
//
// Reads via the reader pool (#51) — pure SELECT.
func (s *Store) ListExtractionFailures(projectID string, limit int) ([]ExtractionFailure, error) {
	q := `SELECT id, project_id, file_path, language, reason, details, first_seen_at, last_seen_at, binary_version_at_failure
	      FROM extraction_failures
	      WHERE project_id = ?
	      ORDER BY last_seen_at DESC`
	args := []any{projectID}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.ro.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExtractionFailure
	for rows.Next() {
		var f ExtractionFailure
		var first, last int64
		var details, binaryVersion sql.NullString
		if err := rows.Scan(&f.ID, &f.ProjectID, &f.FilePath, &f.Language, &f.Reason, &details, &first, &last, &binaryVersion); err != nil {
			return nil, err
		}
		f.Details = details.String
		f.FirstSeenAt = time.Unix(first, 0)
		f.LastSeenAt = time.Unix(last, 0)
		f.BinaryVersionAtFailure = binaryVersion.String
		out = append(out, f)
	}
	return out, rows.Err()
}

// PruneStaleExtractionFailures deletes every extraction_failures row
// whose last_seen_at predates its project's indexed_at — the
// "awaiting re-index to clear" subset the doctor surfaces with
// `is_stale: true` (#1382). One SQL statement so the DB enforces the
// JOIN; per-row Go-side iteration would be slower and racier on a busy
// DB. Returns the number of rows deleted.
//
// #1386. Called by `pincher doctor --fix` (safe-action allowlist) and
// available as a writer method for any future MCP / HTTP wrapper.
//
// Writes via the writer pool — single-writer SQLite; classified in
// writerRoutedStoreMethods.
func (s *Store) PruneStaleExtractionFailures() (int64, error) {
	res, err := s.db.Exec(`
		DELETE FROM extraction_failures
		WHERE EXISTS (
			SELECT 1 FROM projects p
			WHERE p.id = extraction_failures.project_id
			  AND extraction_failures.last_seen_at < p.indexed_at
		)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClearExtractionFailures removes all failure rows for a project. Used by
// `pincher index --force` (when the user wants a clean slate after fixing
// the underlying issues) and by integration tests.
func (s *Store) ClearExtractionFailures(projectID string) error {
	_, err := s.db.Exec(`DELETE FROM extraction_failures WHERE project_id = ?`, projectID)
	return err
}

// ListRecentExtractionFailuresAcrossProjects returns failures across every
// project with last_seen_at >= cutoffUnix, ordered by last_seen_at DESC,
// capped at limit. limit <= 0 returns all rows above the cutoff. The
// returned ExtractionFailure rows carry ProjectID; callers join the
// human-readable project name from an in-memory project list.
//
// Reads via the reader pool — pure SELECT. #1205 collapses doctor's
// per-project N-roundtrip loop into one query, dropping multi-second
// latency to milliseconds on multi-project installs.
func (s *Store) ListRecentExtractionFailuresAcrossProjects(cutoffUnix int64, limit int) ([]ExtractionFailure, error) {
	q := `SELECT id, project_id, file_path, language, reason, details, first_seen_at, last_seen_at, binary_version_at_failure
	      FROM extraction_failures
	      WHERE last_seen_at >= ?
	      ORDER BY last_seen_at DESC`
	args := []any{cutoffUnix}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.ro.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExtractionFailure{}
	for rows.Next() {
		var f ExtractionFailure
		var first, last int64
		var details sql.NullString
		if err := rows.Scan(&f.ID, &f.ProjectID, &f.FilePath, &f.Language, &f.Reason, &details, &first, &last, &f.BinaryVersionAtFailure); err != nil {
			return nil, err
		}
		f.Details = details.String
		f.FirstSeenAt = time.Unix(first, 0)
		f.LastSeenAt = time.Unix(last, 0)
		out = append(out, f)
	}
	return out, rows.Err()
}

// CountRecentExtractionFailuresAcrossProjects returns the count of rows
// above the cutoff across every project. Used by doctor to compute an
// honest extraction_failures_truncated number when the row fetch is
// capped — one cheap COUNT instead of a separate enumeration. #1205.
//
// Reads via the reader pool — pure SELECT.
func (s *Store) CountRecentExtractionFailuresAcrossProjects(cutoffUnix int64) (int, error) {
	var n int
	err := s.ro.QueryRow(
		`SELECT COUNT(*) FROM extraction_failures WHERE last_seen_at >= ?`,
		cutoffUnix,
	).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// EstimateProjectBytes returns a best-effort per-project on-disk byte
// estimate, keyed by project_id. The estimate sums the LENGTH of every
// text column in `symbols` and `edges` plus a flat per-row overhead
// approximating index + b-tree storage, and includes a rough FTS5
// contribution (~50% of the symbols-text payload, since the FTS5 vtab
// re-stores qualified_name + signature + docstring tokens).
//
// This is *not* an exact attribution — SQLite page allocation is whole-
// page granular and pages are shared across projects on multi-project
// installs, so the sum across projects will undershoot the real
// db_size_bytes by 10-40% (the gap is page-fragmentation slack + WAL +
// schema overhead that can't be cheaply attributed per project). The
// load-bearing property is *relative ordering*: doctor consumers use
// these numbers to decide which project to delete first when the DB
// hits multi-GB. Absolute precision would require parsing the b-tree
// (out of scope; see #1219 pincher vacuum for that path). #1220.
//
// Reads via the reader pool — pure SELECT.
func (s *Store) EstimateProjectBytes() (map[string]int64, error) {
	// Symbols contribution: every text column LENGTH'd, plus 64 bytes/row
	// for index entries (idx_sym_project + idx_sym_file + idx_sym_kind +
	// idx_sym_name + idx_sym_qn each add ~12 bytes per symbol).
	symQ := `SELECT project_id, SUM(
	    LENGTH(id) + LENGTH(file_path) + LENGTH(name) + LENGTH(qualified_name) +
	    LENGTH(kind) + LENGTH(language) +
	    LENGTH(COALESCE(signature, '')) + LENGTH(COALESCE(return_type, '')) +
	    LENGTH(COALESCE(docstring, '')) + LENGTH(COALESCE(parent, '')) +
	    LENGTH(COALESCE(file_hash, ''))
	  ) + COUNT(*) * 64 AS bytes
	  FROM symbols
	  GROUP BY project_id`
	out := map[string]int64{}
	rows, err := s.ro.Query(symQ)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var pid string
		var bytes int64
		if err := rows.Scan(&pid, &bytes); err != nil {
			rows.Close()
			return nil, err
		}
		// FTS5 contribution: symbols vtabs re-store the searchable text.
		// 50% of the symbol-text payload is a conservative midpoint of
		// the typical 40-80% range observed on real corpora.
		out[pid] = bytes + bytes/2
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Edges contribution: id-strings + kind + properties + 32 bytes/row
	// for idx_edge_from + idx_edge_to.
	edgeQ := `SELECT project_id, SUM(
	    LENGTH(from_id) + LENGTH(to_id) + LENGTH(kind) + LENGTH(COALESCE(properties, ''))
	  ) + COUNT(*) * 32 AS bytes
	  FROM edges
	  GROUP BY project_id`
	rows2, err := s.ro.Query(edgeQ)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var pid string
		var bytes int64
		if err := rows2.Scan(&pid, &bytes); err != nil {
			return nil, err
		}
		out[pid] += bytes
	}
	return out, rows2.Err()
}

// ExtractionFailureCountsByReason returns a map of reason → count for the
// project's extraction_failures rows. Powers the corpus-snapshot QN-
// collision gate: any non-zero count for `qualified_name_collision` or
// `byte_range_negative` shows up in the snapshot diff at PR time, so a
// new variant of issues #69/#74/#79/#80 fails CI loudly.
//
// Reads via the reader pool (#51) — pure SELECT + GROUP BY.
func (s *Store) ExtractionFailureCountsByReason(projectID string) (map[string]int, error) {
	rows, err := s.ro.Query(
		`SELECT reason, COUNT(*) FROM extraction_failures WHERE project_id = ? GROUP BY reason`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var reason string
		var count int
		if err := rows.Scan(&reason, &count); err != nil {
			return nil, err
		}
		out[reason] = count
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// slow_queries (#42 part 2)
// ─────────────────────────────────────────────────────────────────────────────

// SlowQuery is one row from the slow_queries table — a tool call whose
// latency exceeded the configured --slow-query-ms threshold.
type SlowQuery struct {
	ID         int64
	Tool       string
	ProjectID  string // empty for cross-project tools
	DurationMS int64
	Arguments  string // JSON, with secret-shaped values redacted at record-time
	OccurredAt time.Time
}

// RecordSlowQuery persists a slow-query row. Caller is responsible for
// redacting any secret-shaped values from `arguments` before passing it
// in (the server.go side handles redaction; this layer trusts the input).
func (s *Store) RecordSlowQuery(tool, projectID string, durationMS int64, arguments string) error {
	_, err := s.db.Exec(`
		INSERT INTO slow_queries (tool, project_id, duration_ms, arguments, occurred_at)
		VALUES (?, NULLIF(?, ''), ?, ?, ?)`,
		tool, projectID, durationMS, arguments, time.Now().Unix())
	return err
}

// ListSlowQueries returns the most-recent slow-query rows, ordered by
// occurred_at DESC. limit <= 0 returns all rows.
//
// Reads via the reader pool (#51) — pure SELECT.
func (s *Store) ListSlowQueries(limit int) ([]SlowQuery, error) {
	// Order by occurred_at DESC, then id DESC as tiebreaker — multiple
	// inserts within the same second otherwise have implementation-defined
	// order, which would make ListSlowQueries non-deterministic.
	q := `SELECT id, tool, COALESCE(project_id, ''), duration_ms, COALESCE(arguments, ''), occurred_at
	      FROM slow_queries
	      ORDER BY occurred_at DESC, id DESC`
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.ro.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlowQuery
	for rows.Next() {
		var sq SlowQuery
		var occurred int64
		if err := rows.Scan(&sq.ID, &sq.Tool, &sq.ProjectID, &sq.DurationMS, &sq.Arguments, &occurred); err != nil {
			return nil, err
		}
		sq.OccurredAt = time.Unix(occurred, 0)
		out = append(out, sq)
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// File hash operations (incremental reindex)
// ─────────────────────────────────────────────────────────────────────────────

// GetFileHash returns the stored content hash for a file, or "" if not indexed.
//
// Hot path: indexer's per-file hash check fires for every file in every
// Index() walk. Reader pool (#51) lets these run in parallel during
// re-index of large projects.
func (s *Store) GetFileHash(projectID, path string) string {
	var hash string
	_ = s.ro.QueryRow(`SELECT hash FROM files WHERE project_id=? AND path=?`, projectID, path).Scan(&hash)
	return hash
}

// SetFileHash stores the content hash for a file.
func (s *Store) SetFileHash(projectID, path, hash string) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO files(project_id, path, hash, indexed_at) VALUES (?,?,?,?)`,
		projectID, path, hash, time.Now().Unix())
	return err
}

// DeleteFileHash removes the stored hash for a file.
func (s *Store) DeleteFileHash(projectID, path string) error {
	_, err := s.db.Exec(`DELETE FROM files WHERE project_id=? AND path=?`, projectID, path)
	return err
}

// ClearFileHashesByLanguage drops the stored content-hash row for every
// file in the given project whose extracted symbols include any of the
// listed languages (#1543 v0.84). Deleting from `files` is the canonical
// way to make a file's next indexer pass treat it as new and re-extract:
// the per-file hash compare returns "no prior hash" → extract runs.
//
// Used by the #1497 follow-up gate path: when a binary upgrade ran
// schema migrations classified as `invalidates.Languages=[...]` (not
// `All`), only files of those languages need re-extraction. Cleared
// files re-run on the normal Index() pass without a project-wide
// force-reindex.
//
// Returns the number of file rows deleted so the caller can log the
// scope of the selective invalidation. Empty languages slice is a
// no-op returning 0 (defensive — caller should not invoke with empty).
func (s *Store) ClearFileHashesByLanguage(projectID string, languages []string) (int64, error) {
	if len(languages) == 0 {
		return 0, nil
	}
	// Build the IN list manually since database/sql doesn't support
	// slice placeholders. SQL injection-safe because the languages slice
	// comes from compile-time MigrationInvalidates classifications, not
	// user input.
	placeholders := make([]string, len(languages))
	args := make([]any, 0, len(languages)+1)
	args = append(args, projectID)
	for i, lang := range languages {
		placeholders[i] = "?"
		args = append(args, lang)
	}
	query := fmt.Sprintf(`
		DELETE FROM files
		 WHERE project_id = ?
		   AND path IN (
		     SELECT DISTINCT file_path FROM symbols
		      WHERE project_id = ? AND language IN (%s)
		   )`, strings.Join(placeholders, ","))
	// project_id used twice (outer + subquery): re-prepend.
	args2 := make([]any, 0, len(languages)+2)
	args2 = append(args2, projectID, projectID)
	for _, lang := range languages {
		args2 = append(args2, lang)
	}
	res, err := s.db.Exec(query, args2...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FilesWithEdgesToFile returns distinct relative file paths (other than
// `target` itself) that contain at least one symbol with an outgoing
// edge pointing into a symbol in `target`. Used by the watcher (#427)
// to identify which files need re-extraction when `target` changes:
// their cross-file CALLS / IMPORTS / READS edges to symbols in `target`
// get cascade-deleted on DeleteSymbolsForFile(target), and only a
// re-extraction of the *referencing* files can restore them.
//
// Returns an empty slice when no such files exist. Read-only; uses the
// reader pool.
func (s *Store) FilesWithEdgesToFile(projectID, target string) ([]string, error) {
	rows, err := s.ro.Query(`
		SELECT DISTINCT s_from.file_path
		FROM edges e
		JOIN symbols s_from ON e.from_id = s_from.id AND e.project_id = s_from.project_id
		JOIN symbols s_to   ON e.to_id   = s_to.id   AND e.project_id = s_to.project_id
		WHERE e.project_id = ?
		  AND s_to.file_path = ?
		  AND s_from.file_path != s_to.file_path`,
		projectID, target)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListFilesForProject returns every file path currently recorded in the
// `files` table for projectID. Used by the indexer's tail-pass GC (#326)
// to find symbols whose source file was deleted from disk between runs:
// the walker no longer yields them, so the per-file delete-before-extract
// path never fires.
func (s *Store) ListFilesForProject(projectID string) ([]string, error) {
	rows, err := s.ro.Query(`SELECT path FROM files WHERE project_id=?`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FileHashEntry pairs a file path with its stored content hash. Used
// by `pincher verify` (#1399) to batch-load all files for a project
// in one query instead of N GetFileHash round-trips.
type FileHashEntry struct {
	Path string
	Hash string
}

// ListFilesWithHashesForProject returns every (path, hash) pair from
// the files table for projectID — one bulk SELECT instead of looping
// GetFileHash. Used by `pincher verify` (#1399) to re-hash on-disk
// bytes and surface drift between the stored hash and current file
// content. Drift fires when an indexed file was modified out-of-band
// since its last index pass — the symbol-store byte offsets may point
// at wrong content.
//
// Reads via the reader pool — pure SELECT, classified read.
func (s *Store) ListFilesWithHashesForProject(projectID string) ([]FileHashEntry, error) {
	rows, err := s.ro.Query(`SELECT path, hash FROM files WHERE project_id=? ORDER BY path`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileHashEntry{}
	for rows.Next() {
		var e FileHashEntry
		if err := rows.Scan(&e.Path, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListSymbolFilePaths returns the distinct file paths that currently have
// at least one symbol row for projectID. #756: the #326 tail-pass GC
// iterated only the `files` table, so symbols whose `files` row was
// never written (a crash between flushBatch and SetFileHash) or was
// removed without their symbols stayed orphaned forever — invisible to
// the GC because ListFilesForProject didn't return them. The GC now
// unions both, so any file_path with orphan symbols is reconsidered.
func (s *Store) ListSymbolFilePaths(projectID string) ([]string, error) {
	rows, err := s.ro.Query(`SELECT DISTINCT file_path FROM symbols WHERE project_id=?`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SymbolCountsByFile returns a {file_path: count} map for every file in
// project_id that has at least one persisted symbol. Used by the
// indexer's post-pass parity-check guard (#1231): compare against the
// indexer's in-memory per-file extracted count to detect silent symbol
// loss during persistence. Read-only.
func (s *Store) SymbolCountsByFile(projectID string) (map[string]int, error) {
	rows, err := s.ro.Query(
		`SELECT file_path, COUNT(*) FROM symbols WHERE project_id=? GROUP BY file_path`,
		projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var (
			p string
			c int
		)
		if err := rows.Scan(&p, &c); err != nil {
			return nil, err
		}
		out[p] = c
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// ADR operations
// ─────────────────────────────────────────────────────────────────────────────

// SetADR stores an ADR key-value pair.
func (s *Store) SetADR(projectID, key, value string) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO adrs(project_id, key, value, updated_at) VALUES (?,?,?,?)`,
		projectID, key, value, time.Now().Unix())
	return err
}

// GetADR returns an ADR value by key.
func (s *Store) GetADR(projectID, key string) (string, bool, error) {
	var value string
	// Reader pool (#51).
	err := s.ro.QueryRow(`SELECT value FROM adrs WHERE project_id=? AND key=?`, projectID, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return value, err == nil, err
}

// ListADRs returns all ADR entries for a project.
func (s *Store) ListADRs(projectID string) (map[string]string, error) {
	// Reader pool (#51).
	rows, err := s.ro.Query(`SELECT key, value FROM adrs WHERE project_id=? ORDER BY key`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// DeleteADR removes an ADR entry. Returns the number of rows actually
// deleted so the caller can distinguish "key existed and is gone" from
// "key never existed" — without this, handleADR used to confidently
// report deleted=true on a no-op DELETE, masking typos and wrong-
// project-scope mistakes (#1019).
func (s *Store) DeleteADR(projectID, key string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM adrs WHERE project_id=? AND key=?`, projectID, key)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Session savings persistence
// ─────────────────────────────────────────────────────────────────────────────

// RecordSession upserts the current session's cumulative stats. Call this
// periodically (e.g. every 60 s) and on graceful shutdown. It is idempotent —
// calling it repeatedly with updated values is safe.
//
// httpURL and httpPID identify the session's HTTP listener for the
// `pincher web` discovery flow (#TBD). Pass "" / 0 when no HTTP server
// is bound for this session — the row will still be queryable as a
// pure-MCP session with no http_url advertised.
func (s *Store) RecordSession(sessionID string, startedAt time.Time, calls, tokensUsed, tokensSaved int64, costAvoided float64, httpURL string, httpPID int, callsByLanguage string) error {
	return s.RecordSessionWithMetrics(sessionID, startedAt, calls, tokensUsed, tokensSaved, costAvoided, httpURL, httpPID, callsByLanguage, QueryMetrics{})
}

// RecordSessionWithMetrics is the v17 variant of RecordSession that
// also persists query-failure / retry-rate counters (#241). The
// no-metrics RecordSession wrapper is preserved so existing callers
// (every test that doesn't care about retry stats) keep working
// without a sweeping refactor; only the production flusher path
// needs the new fields populated.
func (s *Store) RecordSessionWithMetrics(sessionID string, startedAt time.Time, calls, tokensUsed, tokensSaved int64, costAvoided float64, httpURL string, httpPID int, callsByLanguage string, qm QueryMetrics) error {
	// callsByLanguage is JSON-encoded {"Go":25,"Markdown":0,...} (#240).
	// Empty string stored as SQL NULL so pre-v15 rows render distinct
	// from "this session genuinely had no per-language data" (which
	// would be `{}`).
	var clbl any
	if callsByLanguage != "" {
		clbl = callsByLanguage
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO sessions(
			session_id, started_at, last_seen, calls, tokens_used, tokens_saved,
			cost_avoided, http_url, http_pid, calls_by_language,
			queries_total, queries_zero_result, queries_retried_succeeded, tokens_burned_on_failures,
			queries_zero_expected, queries_zero_unexpected)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, startedAt.Unix(), time.Now().Unix(), calls, tokensUsed, tokensSaved, costAvoided, httpURL, httpPID, clbl,
		qm.QueriesTotal, qm.QueriesZeroResult, qm.QueriesRetriedSucceeded, qm.TokensBurnedOnFailures,
		qm.QueriesZeroExpected, qm.QueriesZeroUnexpected,
	)
	return err
}

// RecordToolCalls bulk-inserts per-call events into session_tool_calls
// (schema v27, #635). Single transaction so the 10s flush is one
// fsync regardless of how many calls accumulated since the last
// flush. Caller owns the slice; this method does not retain it.
//
// Returns the first error encountered — the partial rows that landed
// before the error are committed because the transaction stays open
// across them. That's safe: the events are append-only / advisory;
// the next flush won't double-insert because the buffer's been
// drained server-side before this call runs.
//
// Writer-routed (mutates session_tool_calls).
func (s *Store) RecordToolCalls(events []ToolCallEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO session_tool_calls(
		session_id, tool, complexity_tier, response_bytes,
		tokens_used, tokens_saved, tokens_saved_pct, ts, request_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()
	for _, e := range events {
		var savedArg any
		if e.TokensSaved != nil {
			savedArg = *e.TokensSaved
		}
		var pctArg any
		if e.TokensSavedPct != nil {
			pctArg = *e.TokensSavedPct
		}
		if _, err := stmt.Exec(
			e.SessionID, e.Tool, e.ComplexityTier, e.ResponseBytes,
			e.TokensUsed, savedArg, pctArg, e.TS.UnixNano(), e.RequestID,
		); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
	}
	return tx.Commit()
}

// RecentToolCallsForSession returns rows from session_tool_calls
// matching the given session_id, newest first. Used by tests and
// future dashboard panels reading per-session detail. Reader-routed.
func (s *Store) RecentToolCallsForSession(sessionID string, limit int) ([]ToolCallEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.ro.Query(
		`SELECT session_id, tool, complexity_tier, response_bytes,
		        tokens_used, tokens_saved, tokens_saved_pct, ts, request_id
		   FROM session_tool_calls
		  WHERE session_id = ?
		  ORDER BY ts DESC
		  LIMIT ?`,
		sessionID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ToolCallEvent{}
	for rows.Next() {
		var e ToolCallEvent
		var saved sql.NullInt64
		var pct sql.NullFloat64
		var tsNanos int64
		if err := rows.Scan(
			&e.SessionID, &e.Tool, &e.ComplexityTier, &e.ResponseBytes,
			&e.TokensUsed, &saved, &pct, &tsNanos, &e.RequestID,
		); err != nil {
			return nil, err
		}
		if saved.Valid {
			v := saved.Int64
			e.TokensSaved = &v
		}
		if pct.Valid {
			v := pct.Float64
			e.TokensSavedPct = &v
		}
		e.TS = time.Unix(0, tsNanos)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ToolCallTierTallyRow is one row of the per-complexity-tier
// aggregation (#635 panel 2). Tiers — lite / standard / heavy — are
// stamped at request time by toolComplexityTier; this groups the
// trailing window's calls so dashboards can show "where is my budget
// being spent": agents heavy on guide / context_for_task show a
// large `heavy` row, traditional read-heavy sessions show a large
// `lite` row.
type ToolCallTierTallyRow struct {
	Tier              string  `json:"tier"`
	CallCount         int64   `json:"call_count"`
	AvgTokensUsed     float64 `json:"avg_tokens_used"`
	SumTokensSaved    int64   `json:"sum_tokens_saved"`
	AvgTokensSavedPct float64 `json:"avg_tokens_saved_pct"`
	AvgResponseBytes  float64 `json:"avg_response_bytes"`
}

// ToolCallStatsByTier mirrors ToolCallStatsByTool's shape but groups
// by complexity_tier instead of tool. Same window-cutoff semantics,
// same NULL-pct exclusion logic. Reader-routed.
//
// Empty-tier rows (where complexity_tier was '' at insert time) are
// filtered — those slipped in pre-#1191 before the tier was always
// stamped, and surfacing them as "" in a dashboard is just noise.
func (s *Store) ToolCallStatsByTier(windowSeconds int64) ([]ToolCallTierTallyRow, error) {
	if windowSeconds <= 0 {
		windowSeconds = 7 * 24 * 60 * 60
	}
	cutoff := time.Now().Add(-time.Duration(windowSeconds) * time.Second).UnixNano()
	rows, err := s.ro.Query(`
		SELECT complexity_tier,
		       COUNT(*)                                       AS call_count,
		       AVG(CAST(tokens_used AS REAL))                 AS avg_tokens_used,
		       COALESCE(SUM(tokens_saved), 0)                 AS sum_tokens_saved,
		       COALESCE(AVG(tokens_saved_pct), 0)             AS avg_tokens_saved_pct,
		       AVG(CAST(response_bytes AS REAL))              AS avg_response_bytes
		  FROM session_tool_calls
		 WHERE ts >= ?
		   AND complexity_tier != ''
		 GROUP BY complexity_tier
		 ORDER BY call_count DESC`,
		cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ToolCallTierTallyRow{}
	for rows.Next() {
		var r ToolCallTierTallyRow
		if err := rows.Scan(
			&r.Tier, &r.CallCount, &r.AvgTokensUsed,
			&r.SumTokensSaved, &r.AvgTokensSavedPct, &r.AvgResponseBytes,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ToolCallTallyRow is one row of the per-tool aggregation surfaced by
// ToolCallStatsByTool — feeds the v0.67 dashboard "tool-call breakdown"
// panel (#635 substrate from schema v27 session_tool_calls).
type ToolCallTallyRow struct {
	Tool              string  `json:"tool"`
	CallCount         int64   `json:"call_count"`
	AvgTokensUsed     float64 `json:"avg_tokens_used"`
	SumTokensSaved    int64   `json:"sum_tokens_saved"`
	AvgTokensSavedPct float64 `json:"avg_tokens_saved_pct"`
	AvgResponseBytes  float64 `json:"avg_response_bytes"`
}

// ToolCallStatsByTool aggregates session_tool_calls rows over the
// trailing windowSeconds-second window into per-tool tallies. Excludes
// rows older than the window cutoff. Sorted by call_count desc so the
// dashboard panel naturally shows the hot tools first.
//
// avg_tokens_saved_pct averages only over rows that actually carry a
// saved_pct value (admin-shape tools like architecture/list/schema
// don't have a Read/Grep baseline so they record NULL there). Without
// the NULL exclusion the average collapses toward zero on read-heavy
// sessions, making search/symbol look worse than they are.
//
// Reader-routed (pure SELECT).
func (s *Store) ToolCallStatsByTool(windowSeconds int64, limit int) ([]ToolCallTallyRow, error) {
	if windowSeconds <= 0 {
		windowSeconds = 7 * 24 * 60 * 60 // 7 days default — matches hook-stats window
	}
	if limit <= 0 {
		limit = 20
	}
	cutoff := time.Now().Add(-time.Duration(windowSeconds) * time.Second).UnixNano()
	rows, err := s.ro.Query(`
		SELECT tool,
		       COUNT(*)                                       AS call_count,
		       AVG(CAST(tokens_used AS REAL))                 AS avg_tokens_used,
		       COALESCE(SUM(tokens_saved), 0)                 AS sum_tokens_saved,
		       COALESCE(AVG(tokens_saved_pct), 0)             AS avg_tokens_saved_pct,
		       AVG(CAST(response_bytes AS REAL))              AS avg_response_bytes
		  FROM session_tool_calls
		 WHERE ts >= ?
		 GROUP BY tool
		 ORDER BY call_count DESC
		 LIMIT ?`,
		cutoff, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ToolCallTallyRow{}
	for rows.Next() {
		var r ToolCallTallyRow
		if err := rows.Scan(
			&r.Tool, &r.CallCount, &r.AvgTokensUsed,
			&r.SumTokensSaved, &r.AvgTokensSavedPct, &r.AvgResponseBytes,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ToolCallPayloadRow is one row of the per-tool payload-size distribution
// surfaced by ToolCallPayloadSizeByTool — feeds the v0.67 dashboard
// "payload size by tool" panel (#635 panel 3). The point of this panel
// is finding outliers: tools where max_bytes is many multiples of
// avg_bytes are the calls that occasionally blow up token bills. Sorting
// by max_bytes desc puts those at the top.
type ToolCallPayloadRow struct {
	Tool      string  `json:"tool"`
	CallCount int64   `json:"call_count"`
	MinBytes  int64   `json:"min_bytes"`
	AvgBytes  float64 `json:"avg_bytes"`
	MaxBytes  int64   `json:"max_bytes"`
	SumBytes  int64   `json:"sum_bytes"`
}

// ToolCallPayloadSizeByTool aggregates response_bytes per tool over the
// trailing windowSeconds-second window. Sorted by max_bytes DESC so the
// loudest tools show first — the dashboard "outlier finder" view.
// Reader-routed (pure SELECT).
func (s *Store) ToolCallPayloadSizeByTool(windowSeconds int64, limit int) ([]ToolCallPayloadRow, error) {
	if windowSeconds <= 0 {
		windowSeconds = 7 * 24 * 60 * 60
	}
	if limit <= 0 {
		limit = 20
	}
	cutoff := time.Now().Add(-time.Duration(windowSeconds) * time.Second).UnixNano()
	rows, err := s.ro.Query(`
		SELECT tool,
		       COUNT(*)                          AS call_count,
		       COALESCE(MIN(response_bytes), 0)  AS min_bytes,
		       COALESCE(AVG(CAST(response_bytes AS REAL)), 0) AS avg_bytes,
		       COALESCE(MAX(response_bytes), 0)  AS max_bytes,
		       COALESCE(SUM(response_bytes), 0)  AS sum_bytes
		  FROM session_tool_calls
		 WHERE ts >= ?
		 GROUP BY tool
		 ORDER BY max_bytes DESC
		 LIMIT ?`,
		cutoff, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ToolCallPayloadRow{}
	for rows.Next() {
		var r ToolCallPayloadRow
		if err := rows.Scan(
			&r.Tool, &r.CallCount, &r.MinBytes,
			&r.AvgBytes, &r.MaxBytes, &r.SumBytes,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueryMetrics carries the v17 query-failure / retry-rate counters
// flushed onto every sessions row (#241). Pre-v17 rows hold zero on
// every counter — agents that haven't been instrumented yet stay
// invisible but don't break the aggregate.
type QueryMetrics struct {
	// QueriesTotal counts every query-shaped tool call (search, query,
	// trace, neighborhood). Other tools (architecture, list, schema)
	// are admin/orientation calls and don't return a count, so they
	// don't influence retry-rate signals.
	QueriesTotal int64

	// QueriesZeroResult counts the subset of QueriesTotal where the
	// response contained 0 results. The diagnosis surface (#165)
	// already tells the agent how to retry; this counter aggregates
	// the cumulative cost of those retries so users can decide if
	// their default threshold is mistuned for their workflow.
	QueriesZeroResult int64

	// QueriesRetriedSucceeded counts the subset of zero-result calls
	// that were immediately followed by an equivalent retry (same
	// tool, same query string, lower min_confidence or otherwise
	// loosened) returning ≥1 result. Lets users distinguish
	// "agent learned and recovered" from "agent gave up" friction.
	QueriesRetriedSucceeded int64

	// TokensBurnedOnFailures sums tokens_used across the zero-result
	// calls. This is pure overhead — tokens the agent paid for a
	// response that ultimately required a retry to be useful.
	TokensBurnedOnFailures int64

	// QueriesZeroExpected and QueriesZeroUnexpected (v34, #1632) split
	// QueriesZeroResult into "audit-shape" vs "caller-surprised"
	// populations so dashboards can surface the actionable rate.
	// The two MUST satisfy:
	//     QueriesZeroExpected + QueriesZeroUnexpected == QueriesZeroResult
	// on every flushed row. Pre-v34 rows hold zero on both new columns
	// (their zero-results are accounted for in QueriesZeroResult only),
	// so aggregates over the historical dataset are conservative —
	// "of every NEW zero result since v34, this many were
	// audit-shape and this many were caller-surprised."
	//
	// Classification rule: tool=="query" AND pinchql/cypher contains
	// "{" (property predicate) → expected; everything else →
	// unexpected. The conservative default favors the friction signal.
	QueriesZeroExpected   int64
	QueriesZeroUnexpected int64
}

// ToolCallEvent is one row in session_tool_calls — emitted by the
// server's jsonResultWithMeta hot path and bulk-persisted via
// RecordToolCalls. The fields are exactly the substrate the v0.64
// dashboard triangulating panels (#635) need:
//
//   - tool + complexity_tier   → per-tier saved-pct medians, entropy
//   - response_bytes           → median payload size
//   - tokens_used / saved / pct → existing _meta accounting frozen at the call
//   - ts                       → 7-day window filtering
//   - request_id               → correlate with hook_invocations + logs (#657)
//
// Zero-value friendliness: TokensSaved + TokensSavedPct are *float64
// in pointer form for "none" baseline tools (architecture / list /
// schema etc. — see baselineMethodNone in server). Stored as SQL NULL
// when nil; dashboard SQL filters with `WHERE tokens_saved IS NOT NULL`
// to avoid distorting medians with non-applicable rows.
type ToolCallEvent struct {
	SessionID       string
	Tool            string
	ComplexityTier  string
	ResponseBytes   int64
	TokensUsed      int64
	TokensSaved     *int64
	TokensSavedPct  *float64
	TS              time.Time
	RequestID       string
}

// SessionRow holds per-session stats for historical display.
type SessionRow struct {
	SessionID   string
	StartedAt   time.Time
	LastSeen    time.Time
	Calls       int64
	TokensUsed  int64
	TokensSaved int64
	CostAvoided float64
	HTTPURL     string
	HTTPPID     int
	// CallsByLanguage is the JSON-encoded language→count map persisted
	// per session (#240). Empty string when the row pre-dates v15 or
	// no per-language data was recorded. Callers parse via
	// json.Unmarshal; the raw string is exposed for forward-compat
	// (additional fields beyond a flat int map can land without a
	// SessionRow struct change).
	CallsByLanguage string
	// QueryMetrics carries the v17 query-failure counters (#241).
	// Pre-v17 rows hold zero on every field.
	QueryMetrics QueryMetrics
}

// GetSessionByID returns the sessions row for a specific session_id, or
// (nil, nil) when no row exists yet. #420: used by the server on startup
// to seed in-memory counters from prior flushes when a stable session
// ID is supplied (e.g. via PINCHER_SESSION_ID under supervised mode).
// Reader-pool routed — pure SELECT, never blocks writers.
func (s *Store) GetSessionByID(sessionID string) (*SessionRow, error) {
	if sessionID == "" {
		return nil, nil
	}
	q := `SELECT session_id, started_at, last_seen, calls, tokens_used, tokens_saved,
	             cost_avoided, http_url, http_pid, calls_by_language,
	             queries_total, queries_zero_result, queries_retried_succeeded, tokens_burned_on_failures,
	             queries_zero_expected, queries_zero_unexpected
	      FROM sessions WHERE session_id=? LIMIT 1`
	var r SessionRow
	var startedUnix, lastSeenUnix int64
	var clbl sql.NullString
	err := s.ro.QueryRow(q, sessionID).Scan(
		&r.SessionID, &startedUnix, &lastSeenUnix, &r.Calls, &r.TokensUsed, &r.TokensSaved,
		&r.CostAvoided, &r.HTTPURL, &r.HTTPPID, &clbl,
		&r.QueryMetrics.QueriesTotal, &r.QueryMetrics.QueriesZeroResult,
		&r.QueryMetrics.QueriesRetriedSucceeded, &r.QueryMetrics.TokensBurnedOnFailures,
		&r.QueryMetrics.QueriesZeroExpected, &r.QueryMetrics.QueriesZeroUnexpected,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.StartedAt = time.Unix(startedUnix, 0)
	r.LastSeen = time.Unix(lastSeenUnix, 0)
	if clbl.Valid {
		r.CallsByLanguage = clbl.String
	}
	return &r, nil
}

// GetSessions returns all recorded sessions ordered by start time descending.
// Limit ≤ 0 returns all rows.
func (s *Store) GetSessions(limit int) ([]SessionRow, error) {
	q := `SELECT session_id, started_at, last_seen, calls, tokens_used, tokens_saved,
	             cost_avoided, http_url, http_pid, calls_by_language,
	             queries_total, queries_zero_result, queries_retried_succeeded, tokens_burned_on_failures,
	             queries_zero_expected, queries_zero_unexpected
	      FROM sessions ORDER BY started_at DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		var startedUnix, lastSeenUnix int64
		var clbl sql.NullString
		if err := rows.Scan(&r.SessionID, &startedUnix, &lastSeenUnix, &r.Calls, &r.TokensUsed, &r.TokensSaved, &r.CostAvoided, &r.HTTPURL, &r.HTTPPID, &clbl,
			&r.QueryMetrics.QueriesTotal, &r.QueryMetrics.QueriesZeroResult, &r.QueryMetrics.QueriesRetriedSucceeded, &r.QueryMetrics.TokensBurnedOnFailures,
			&r.QueryMetrics.QueriesZeroExpected, &r.QueryMetrics.QueriesZeroUnexpected); err != nil {
			return nil, err
		}
		r.StartedAt = time.Unix(startedUnix, 0)
		r.LastSeen = time.Unix(lastSeenUnix, 0)
		if clbl.Valid {
			r.CallsByLanguage = clbl.String
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetAllTimeQueryMetrics returns the cumulative query-failure /
// retry-rate counters across every session (#241). Pre-v17 rows
// contribute zero to every field; the aggregate is conservative:
// "of every query that ever happened on a v17+ binary, this many
// returned zero results, this many were retried successfully, and
// this many tokens were burned on the zero-result attempts."
func (s *Store) GetAllTimeQueryMetrics() (QueryMetrics, error) {
	var qm QueryMetrics
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(queries_total),0),
		        COALESCE(SUM(queries_zero_result),0),
		        COALESCE(SUM(queries_retried_succeeded),0),
		        COALESCE(SUM(tokens_burned_on_failures),0),
		        COALESCE(SUM(queries_zero_expected),0),
		        COALESCE(SUM(queries_zero_unexpected),0)
		 FROM sessions`,
	).Scan(&qm.QueriesTotal, &qm.QueriesZeroResult, &qm.QueriesRetriedSucceeded, &qm.TokensBurnedOnFailures,
		&qm.QueriesZeroExpected, &qm.QueriesZeroUnexpected)
	return qm, err
}

// GetLatestHTTPSession returns the most recently-flushed session row that
// advertised an HTTP listener. Caller is expected to PID-liveness-check the
// returned PID before trusting the URL — this function does not filter on
// last_seen, so very stale rows can come back.
//
// Returns (zero-value, sql.ErrNoRows) when no session has ever advertised
// an HTTP URL.
func (s *Store) GetLatestHTTPSession() (SessionRow, error) {
	var r SessionRow
	var startedUnix, lastSeenUnix int64
	err := s.ro.QueryRow(
		`SELECT session_id, started_at, last_seen, calls, tokens_used, tokens_saved, cost_avoided, http_url, http_pid
		 FROM sessions
		 WHERE http_url != '' AND http_pid > 0
		 ORDER BY last_seen DESC
		 LIMIT 1`,
	).Scan(&r.SessionID, &startedUnix, &lastSeenUnix, &r.Calls, &r.TokensUsed, &r.TokensSaved, &r.CostAvoided, &r.HTTPURL, &r.HTTPPID)
	if err != nil {
		return SessionRow{}, err
	}
	r.StartedAt = time.Unix(startedUnix, 0)
	r.LastSeen = time.Unix(lastSeenUnix, 0)
	return r, nil
}

// GetAllTimeSavings returns the cumulative savings across all recorded sessions.
func (s *Store) GetAllTimeSavings() (calls, tokensUsed, tokensSaved int64, costAvoided float64, err error) {
	err = s.db.QueryRow(
		`SELECT COALESCE(SUM(calls),0), COALESCE(SUM(tokens_used),0),
		        COALESCE(SUM(tokens_saved),0), COALESCE(SUM(cost_avoided),0.0)
		 FROM sessions`,
	).Scan(&calls, &tokensUsed, &tokensSaved, &costAvoided)
	return
}

// CelebrationThresholds is the ordered list of cumulative-tokens-saved
// milestones (#494). 5× spacing between tiers means crossings are
// inherently rare — natural pacing without an artificial daily timer.
// A given threshold fires exactly once per installation, ever
// (enforced by celebrations.threshold_tokens PRIMARY KEY).
var CelebrationThresholds = []int64{
	100_000,
	500_000,
	1_000_000,
	5_000_000,
	10_000_000,
	50_000_000,
	100_000_000,
	500_000_000,
	1_000_000_000,
}

// MaybeFireCelebration finds the highest CelebrationThresholds entry
// at or below cumulativeTokensSaved that has NOT yet been celebrated,
// records it as fired, and returns it. Returns (0, false, nil) when
// nothing new to celebrate. INSERT OR IGNORE makes this safe under
// concurrent tool-call races: only one caller wins the row, the rest
// observe `affected==0` and report "no celebration".
//
// Caller is expected to format the human-readable string from the
// returned threshold. Keeping formatting out of the store keeps the
// DB layer free of UI strings.
func (s *Store) MaybeFireCelebration(cumulativeTokensSaved int64) (threshold int64, fired bool, err error) {
	// Find highest threshold ≤ cumulative that is NOT in celebrations.
	var candidate int64
	for i := len(CelebrationThresholds) - 1; i >= 0; i-- {
		if CelebrationThresholds[i] <= cumulativeTokensSaved {
			candidate = CelebrationThresholds[i]
			break
		}
	}
	if candidate == 0 {
		return 0, false, nil
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO celebrations(threshold_tokens, fired_at) VALUES(?, ?)`,
		candidate, time.Now().Unix(),
	)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Already fired — race or repeat call.
		return 0, false, nil
	}
	return candidate, true, nil
}

// GetAllTimeCallsByLanguage returns the cumulative per-language call
// counts across every session that recorded the v16 calls_by_language
// column (#240). Sessions with NULL or unparseable JSON are skipped
// silently — a single corrupt row should not blank out the diagnostic
// for every other session. Returns an empty (non-nil) map when no
// session has ever recorded language data.
func (s *Store) GetAllTimeCallsByLanguage() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT calls_by_language FROM sessions WHERE calls_by_language IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	totals := make(map[string]int64)
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if !raw.Valid || raw.String == "" {
			continue
		}
		var per map[string]int64
		if err := json.Unmarshal([]byte(raw.String), &per); err != nil {
			continue
		}
		for lang, n := range per {
			totals[lang] += n
		}
	}
	return totals, rows.Err()
}

// ResetSessions wipes every row from the sessions table and returns the
// number of rows deleted. Used by `pincher stats --reset` to clear the
// adoption-priming counters (cost avoided, tokens saved, call counts)
// without touching symbol / edge / project data.
//
// The running pincher process retains its in-memory atomic counters and
// will re-flush them on the next 10s tick — so subsequent stats won't
// stay at zero, but the historical sessions are gone permanently. There
// is no automatic backup; callers who want to preserve totals should
// snapshot via `pincher stats --json > backup.json` before calling.
func (s *Store) ResetSessions() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions`)
	if err != nil {
		return 0, fmt.Errorf("delete sessions: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return rows, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Scan helpers
// ─────────────────────────────────────────────────────────────────────────────

const symSelectFrom = `
	SELECT id, project_id, file_path, name, qualified_name, kind, language,
	       start_byte, end_byte, start_line, end_line,
	       signature, return_type, docstring, parent,
	       complexity, is_exported, is_test, is_entry_point, file_hash,
	       extraction_confidence, branch
	FROM symbols`

// querySymbols runs q with args and returns all symbols scanned from the result set.
// querySymbols routes through the READER pool (#51). Used by every
// Get*Symbol* method in this file — migrating once here covers them all.
func (s *Store) querySymbols(q string, args ...any) ([]Symbol, error) {
	rows, err := s.ro.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanManySymbols(rows)
}

func scanOneSymbol(row *sql.Row) (*Symbol, error) {
	var sym Symbol
	var sig, ret, doc, par, fh sql.NullString
	var isExp, isTest, isEntry int64
	err := row.Scan(
		&sym.ID, &sym.ProjectID, &sym.FilePath, &sym.Name, &sym.QualifiedName, &sym.Kind, &sym.Language,
		&sym.StartByte, &sym.EndByte, &sym.StartLine, &sym.EndLine,
		&sig, &ret, &doc, &par,
		&sym.Complexity, &isExp, &isTest, &isEntry, &fh,
		&sym.ExtractionConfidence, &sym.Branch,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fillSymbol(&sym, sig, ret, doc, par, fh, isExp, isTest, isEntry)
	return &sym, nil
}

func scanManySymbols(rows *sql.Rows) ([]Symbol, error) {
	var out []Symbol
	for rows.Next() {
		var sym Symbol
		if err := scanSymbolRowsRow(rows, &sym); err != nil {
			return nil, err
		}
		out = append(out, sym)
	}
	return out, rows.Err()
}

func scanSymbolRowsRow(rows *sql.Rows, sym *Symbol) error {
	var sig, ret, doc, par, fh sql.NullString
	var isExp, isTest, isEntry int64
	if err := rows.Scan(
		&sym.ID, &sym.ProjectID, &sym.FilePath, &sym.Name, &sym.QualifiedName, &sym.Kind, &sym.Language,
		&sym.StartByte, &sym.EndByte, &sym.StartLine, &sym.EndLine,
		&sig, &ret, &doc, &par,
		&sym.Complexity, &isExp, &isTest, &isEntry, &fh,
		&sym.ExtractionConfidence, &sym.Branch,
	); err != nil {
		return err
	}
	fillSymbol(sym, sig, ret, doc, par, fh, isExp, isTest, isEntry)
	return nil
}

// scanSymbolRow scans a symbol row that also includes a score column (FTS5 queries).
func scanSymbolRow(rows *sql.Rows, sym *Symbol, score *float64) error {
	var sig, ret, doc, par, fh sql.NullString
	var isExp, isTest, isEntry int64
	if err := rows.Scan(
		&sym.ID, &sym.ProjectID, &sym.FilePath, &sym.Name, &sym.QualifiedName, &sym.Kind, &sym.Language,
		&sym.StartByte, &sym.EndByte, &sym.StartLine, &sym.EndLine,
		&sig, &ret, &doc, &par,
		&sym.Complexity, &isExp, &isTest, &isEntry, &fh,
		&sym.ExtractionConfidence, &sym.Branch, score,
	); err != nil {
		return err
	}
	fillSymbol(sym, sig, ret, doc, par, fh, isExp, isTest, isEntry)
	return nil
}

// fillSymbol sets the NullString and bool fields on sym after a Scan call.
func fillSymbol(sym *Symbol, sig, ret, doc, par, fh sql.NullString, isExp, isTest, isEntry int64) {
	sym.Signature = sig.String
	sym.ReturnType = ret.String
	sym.Docstring = doc.String
	sym.Parent = par.String
	sym.FileHash = fh.String
	sym.IsExported = isExp != 0
	sym.IsTest = isTest != 0
	sym.IsEntryPoint = isEntry != 0
}

// withTx runs fn inside a transaction, committing on success and rolling back on error.
func (s *Store) withTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ns converts empty string to nil for SQL NULL columns.
func ns(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// bi converts bool to 0/1 for SQLite INTEGER columns.
func bi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ProjectNameFromPath derives a project name from a directory path.
// Handles both Unix and Windows-style paths on all platforms.
func ProjectNameFromPath(path string) string {
	// Normalize Windows backslashes so filepath.Base works cross-platform.
	path = strings.ReplaceAll(path, "\\", "/")
	return filepath.Base(filepath.Clean(path))
}

// ProjectIDFromPath derives a stable project ID from a directory path.
//
// Returns the canonical form of `path`: symlinks resolved, casing
// normalised on case-insensitive filesystems (macOS APFS default,
// Windows NTFS default). Two invocations against the same physical
// directory MUST return the same project_id, regardless of the caller's
// path-string casing or symlink usage. See CanonicalProjectPath for
// the full canonicalisation rules; closes #84.
func ProjectIDFromPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		// filepath.Abs only fails if os.Getwd does — extremely rare, but
		// fall back to the input path so we don't silently invent IDs.
		return path
	}
	return CanonicalProjectPath(abs)
}

// ApproxTokens returns an approximate BPE token count for s.
//
// Default (#1320): a char/4 heuristic. cl100k_base BPE averages ~4
// chars/token for English text and pincher response bodies (JSON +
// symbol IDs + snippets); the error is bounded and the cost is one
// integer divide vs ~1000 allocs/call for the BPE encoder. Profile
// of BenchmarkAuth_TimingProfile/correct: the BPE encoder consumed
// 60% of per-call allocations to populate _meta.tokens_used. The
// fast path makes the envelope effectively free.
//
// Opt back in to exact BPE counts with PINCHER_TOKEN_ACCOUNTING=exact
// — useful for operators benchmarking real token consumption or
// validating savings reporting. The session-flush aggregator writes
// the same value either way; per-call envelopes shift by ~5-15%.
//
// Aligned with the long-standing user-facing feedback that the
// per-call savings panel is noisy: making it cheap-by-default is
// what enables it to stay quiet without losing the signal.
func ApproxTokens(s string) int {
	if tokenAccountingExact() {
		if enc := getTokenizer(); enc != nil {
			ids, _, _ := enc.Encode(s)
			return len(ids)
		}
	}
	return (len(s) + 3) / 4
}

// tokenAccountingExact reports whether PINCHER_TOKEN_ACCOUNTING=exact
// was set in the process environment, opting into BPE for every
// ApproxTokens call. Cached on first read — env doesn't change inside
// a process lifetime and the read is on the hot path.
var (
	tokenAccountingExactOnce sync.Once
	tokenAccountingExactFlag bool
)

func tokenAccountingExact() bool {
	tokenAccountingExactOnce.Do(func() {
		v := strings.ToLower(strings.TrimSpace(os.Getenv("PINCHER_TOKEN_ACCOUNTING")))
		tokenAccountingExactFlag = v == "exact" || v == "bpe" || v == "1" || v == "true"
	})
	return tokenAccountingExactFlag
}

// HookInvocation captures one row of the v24 hook_invocations table.
// Written by `pincher hook-check` on every PreToolUse callout; the
// `took_recommendation` column is filled in by a post-hoc joiner that
// walks the session's subsequent tool calls. See #626.
type HookInvocation struct {
	ID                 int64
	TS                 int64  // unix epoch nanos
	SessionID          string // optional; empty for non-MCP invocations
	ToolName           string // "Read" or "Grep"
	FilePath           string
	FileBytes          int64
	Decision           string // "pass_through" | "redirect"
	SuggestedTool      string // null/empty on pass_through
	SuggestedArgs      string // JSON blob; null/empty on pass_through
	NextToolWithin3    string
	TookRecommendation *bool // nullable; nil until joiner runs
}

// LogHookInvocation writes one hook decision into the telemetry table.
// Best-effort: callers shouldn't fail their primary work because this
// failed. Writer-routed.
func (s *Store) LogHookInvocation(inv HookInvocation) error {
	_, err := s.db.Exec(
		`INSERT INTO hook_invocations(
			ts, session_id, tool_name, file_path, file_bytes,
			decision, suggested_tool, suggested_args
		) VALUES (?,?,?,?,?,?,?,?)`,
		inv.TS, inv.SessionID, inv.ToolName, inv.FilePath, inv.FileBytes,
		inv.Decision, inv.SuggestedTool, inv.SuggestedArgs,
	)
	return err
}

// ResolveHookInvocationsForSession walks redirect rows for the given
// session whose took_recommendation is still NULL and marks them as
// 1 / 0 based on whether suggested_tool appears in the session's next
// 3 tool calls after the invocation timestamp. Returns the count of
// rows updated. Idempotent: rows already resolved are skipped by the
// WHERE clause. Reader+writer routed (read pending, write resolution).
func (s *Store) ResolveHookInvocationsForSession(sessionID string, recentCalls []HookSessionCall) (int, error) {
	rows, err := s.ro.Query(
		`SELECT id, ts, suggested_tool
		   FROM hook_invocations
		  WHERE session_id = ? AND decision IN ('redirect','redirect_advisory') AND took_recommendation IS NULL
		  ORDER BY ts`,
		sessionID,
	)
	if err != nil {
		return 0, err
	}
	type pending struct {
		ID            int64
		TS            int64
		SuggestedTool string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.ID, &p.TS, &p.SuggestedTool); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if len(todo) == 0 {
		return 0, nil
	}
	var updated int
	for _, p := range todo {
		took := 0
		var nextTool string
		// Find the up-to-3 next calls strictly after the invocation.
		seen := 0
		for _, c := range recentCalls {
			if c.TS <= p.TS {
				continue
			}
			seen++
			if c.ToolName == p.SuggestedTool {
				took = 1
				nextTool = c.ToolName
				break
			}
			if nextTool == "" {
				nextTool = c.ToolName
			}
			if seen >= 3 {
				break
			}
		}
		// Only resolve when at least one subsequent call exists. Without
		// any next-call evidence we can't say whether the agent took it.
		if seen == 0 {
			continue
		}
		if _, err := s.db.Exec(
			`UPDATE hook_invocations
			    SET took_recommendation = ?, next_tool_within_3 = ?
			  WHERE id = ?`,
			took, nextTool, p.ID,
		); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// HookSessionCall is a minimal tool-call record used to resolve hook
// invocations. Passed in by the caller so the resolver doesn't need to
// know about the session-call source.
type HookSessionCall struct {
	TS       int64
	ToolName string
}

// IsFileIndexed reports whether (projectID, filePath) appears in
// the files (file-hash) table — i.e. the indexer has processed this
// file at least once. Cheap point lookup; the hook decision path
// calls this on every Read interception so latency budget is tight.
// Reader-routed.
func (s *Store) IsFileIndexed(projectID, filePath string) bool {
	var n int
	if err := s.ro.QueryRow(
		`SELECT COUNT(1) FROM files WHERE project_id = ? AND path = ?`,
		projectID, filePath,
	).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// CountSymbolsInFile returns the number of indexed symbols for the
// given (projectID, filePath). Used by the PreToolUse hook to gate
// redirect on \"file has meaningful symbolic content\" — < 5 symbols
// usually means a config blob where context wouldn't help.
// Reader-routed.
func (s *Store) CountSymbolsInFile(projectID, filePath string) (int, error) {
	var n int
	err := s.ro.QueryRow(
		`SELECT COUNT(1) FROM symbols WHERE project_id = ? AND file_path = ?`,
		projectID, filePath,
	).Scan(&n)
	return n, err
}

// LargestSymbolInFile returns the ID of the symbol with the widest
// byte span in (projectID, filePath). Used by the PreToolUse hook
// to pick a sensible default for the `context id=...` redirect —
// the file's main entry point is usually the largest symbol.
// Returns empty string when the file has no indexed symbols.
// Reader-routed.
func (s *Store) LargestSymbolInFile(projectID, filePath string) (string, error) {
	var id string
	err := s.ro.QueryRow(
		`SELECT id FROM symbols
		  WHERE project_id = ? AND file_path = ?
		  ORDER BY (end_byte - start_byte) DESC
		  LIMIT 1`,
		projectID, filePath,
	).Scan(&id)
	if err != nil {
		// Treat \"no rows\" as empty string + no error (caller wants
		// best-effort, not a hard fail).
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return id, nil
}

// HookConversionRate7d returns the conversion rate over the trailing
// 7 days plus raw counts. Returns (pct, redirects, taken, err) where
// pct is in [0, 100]. Backs the v0.37 headline dashboard panel.
// Reader-routed.
func (s *Store) HookConversionRate7d() (float64, int, int, error) {
	row := s.ro.QueryRow(
		`SELECT
		    COALESCE(SUM(CASE WHEN decision IN ('redirect','redirect_advisory') THEN 1 ELSE 0 END), 0) AS redirects,
		    COALESCE(SUM(CASE WHEN took_recommendation=1 THEN 1 ELSE 0 END), 0) AS taken
		   FROM hook_invocations
		  WHERE ts > ?`,
		time.Now().Add(-7*24*time.Hour).UnixNano(),
	)
	var redirects, taken int
	if err := row.Scan(&redirects, &taken); err != nil {
		return 0, 0, 0, err
	}
	if redirects == 0 {
		return 0, 0, 0, nil
	}
	return float64(taken) / float64(redirects) * 100, redirects, taken, nil
}

// HookOverrideRate7d returns the trailing-7d percentage of redirects
// the agent saw and explicitly rejected (took_recommendation=0). When
// this stays > 30% over multiple weeks, the redirect message itself
// is the problem — agents are seeing the suggestion and choosing not
// to take it. Returns (pct, overrides, resolved, err) where resolved
// is the number of redirects with a non-NULL took_recommendation
// (the denominator) and overrides is the count where it equals 0.
//
// Distinct from "100% - conversion_pct" because it excludes redirects
// that haven't been resolved yet (no subsequent calls observed).
// Reader-routed.
func (s *Store) HookOverrideRate7d() (float64, int, int, error) {
	row := s.ro.QueryRow(
		`SELECT
		    COALESCE(SUM(CASE WHEN took_recommendation IS NOT NULL THEN 1 ELSE 0 END), 0) AS resolved,
		    COALESCE(SUM(CASE WHEN took_recommendation=0 THEN 1 ELSE 0 END), 0) AS overrides
		   FROM hook_invocations
		  WHERE decision IN ('redirect','redirect_advisory') AND ts > ?`,
		time.Now().Add(-7*24*time.Hour).UnixNano(),
	)
	var resolved, overrides int
	if err := row.Scan(&resolved, &overrides); err != nil {
		return 0, 0, 0, err
	}
	if resolved == 0 {
		return 0, 0, 0, nil
	}
	return float64(overrides) / float64(resolved) * 100, overrides, resolved, nil
}

// HookCountsByTool7d breaks down the trailing-7d intercept counts by
// tool name. Surfaces the Read-vs-Grep balance — when one is much
// higher than the other, the per-pattern decision logic may need
// rebalancing. Returns a map of tool_name → {redirects, taken} pairs.
// Reader-routed.
func (s *Store) HookCountsByTool7d() (map[string]map[string]int, error) {
	rows, err := s.ro.Query(
		`SELECT
		    tool_name,
		    SUM(CASE WHEN decision IN ('redirect','redirect_advisory') THEN 1 ELSE 0 END) AS redirects,
		    SUM(CASE WHEN took_recommendation=1 THEN 1 ELSE 0 END) AS taken
		   FROM hook_invocations
		  WHERE ts > ?
		  GROUP BY tool_name`,
		time.Now().Add(-7*24*time.Hour).UnixNano(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]map[string]int)
	for rows.Next() {
		var tool string
		var redirects, taken int
		if err := rows.Scan(&tool, &redirects, &taken); err != nil {
			return nil, err
		}
		out[tool] = map[string]int{"redirects": redirects, "taken": taken}
	}
	return out, rows.Err()
}

// FormatSize formats a byte count as a human-readable string.
func FormatSize(bytes int) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// BenchRun is one persisted `pincher bench --persist` invocation.
// Schema v29 (#1263 follow-up). One row per CLI invocation; per-tool
// aggregates live in bench_results joined by run_id.
type BenchRun struct {
	RunID         string    `json:"run_id"`
	ProjectID     string    `json:"project_id"`
	StartedAt     time.Time `json:"started_at"`
	NSamples      int       `json:"n_samples"`
	TraceDepth    int       `json:"trace_depth"`
	BinaryVersion string    `json:"binary_version,omitempty"`
}

// BenchResult is one per-tool aggregate row attached to a BenchRun.
type BenchResult struct {
	RunID              string  `json:"run_id"`
	ToolName           string  `json:"tool_name"`
	Calls              int     `json:"calls"`
	P50LatencyMs       float64 `json:"p50_latency_ms"`
	P95LatencyMs       float64 `json:"p95_latency_ms"`
	MeanLatencyMs      float64 `json:"mean_latency_ms"`
	MeanTokensActual   int64   `json:"mean_tokens_actual"`
	MeanTokensBaseline int64   `json:"mean_tokens_baseline"`
	SavingsPct         float64 `json:"savings_pct"`
}

// RecordBenchRun writes one summary row plus the per-tool aggregates
// in a single transaction. Best-effort write semantics: failures
// return an error but the bench's text/JSON output is still useful.
// Writer-routed.
func (s *Store) RecordBenchRun(run BenchRun, results []BenchResult) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO bench_runs(run_id, project_id, started_at, n_samples, trace_depth, binary_version)
			VALUES (?,?,?,?,?,?)`,
			run.RunID, run.ProjectID, run.StartedAt, run.NSamples, run.TraceDepth, run.BinaryVersion,
		); err != nil {
			return err
		}
		stmt, err := tx.Prepare(`
			INSERT INTO bench_results(
				run_id, tool_name, calls,
				p50_latency_ms, p95_latency_ms, mean_latency_ms,
				mean_tokens_actual, mean_tokens_baseline, savings_pct
			) VALUES (?,?,?, ?,?,?, ?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range results {
			if _, err := stmt.Exec(
				run.RunID, r.ToolName, r.Calls,
				r.P50LatencyMs, r.P95LatencyMs, r.MeanLatencyMs,
				r.MeanTokensActual, r.MeanTokensBaseline, r.SavingsPct,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListBenchRuns returns the most recent `limit` runs for projectID,
// newest first. Empty projectID lists across all projects.
// Reader-routed.
func (s *Store) ListBenchRuns(projectID string, limit int) ([]BenchRun, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if projectID == "" {
		rows, err = s.ro.Query(`
			SELECT run_id, project_id, started_at, n_samples, trace_depth, binary_version
			  FROM bench_runs
			 ORDER BY started_at DESC
			 LIMIT ?`, limit)
	} else {
		rows, err = s.ro.Query(`
			SELECT run_id, project_id, started_at, n_samples, trace_depth, binary_version
			  FROM bench_runs
			 WHERE project_id = ?
			 ORDER BY started_at DESC
			 LIMIT ?`, projectID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchRun{}
	for rows.Next() {
		var r BenchRun
		if err := rows.Scan(&r.RunID, &r.ProjectID, &r.StartedAt, &r.NSamples, &r.TraceDepth, &r.BinaryVersion); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetBenchResults returns the per-tool rows attached to runID.
// Reader-routed.
func (s *Store) GetBenchResults(runID string) ([]BenchResult, error) {
	rows, err := s.ro.Query(`
		SELECT run_id, tool_name, calls,
		       p50_latency_ms, p95_latency_ms, mean_latency_ms,
		       mean_tokens_actual, mean_tokens_baseline, savings_pct
		  FROM bench_results
		 WHERE run_id = ?
		 ORDER BY tool_name`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BenchResult{}
	for rows.Next() {
		var r BenchResult
		if err := rows.Scan(
			&r.RunID, &r.ToolName, &r.Calls,
			&r.P50LatencyMs, &r.P95LatencyMs, &r.MeanLatencyMs,
			&r.MeanTokensActual, &r.MeanTokensBaseline, &r.SavingsPct,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
