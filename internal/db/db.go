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
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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

// schemaMigrations is an ordered list of incremental SQL migrations applied
// after the baseline schema. migrations[i] upgrades version (i+1) → (i+2).
// To add a schema change: append a SQL string here and bump nothing else —
// the version number is derived from the slice length automatically.
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
	// calls. Surfaced in `pincher stats` so users can see "Retry rate:
	// 18%" and act (lower default min_confidence in CLAUDE.md, or file
	// an extractor issue), instead of paying retry tokens forever
	// without aggregate visibility.
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
}

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
	for i := version - 1; i < len(schemaMigrations); i++ {
		if _, err := s.db.Exec(schemaMigrations[i]); err != nil {
			return fmt.Errorf("schema migration v%d→v%d: %w", i+1, i+2, err)
		}
		next := i + 2
		if _, err := s.db.Exec(`UPDATE schema_version SET version = ?`, next); err != nil {
			return fmt.Errorf("bump schema version to %d: %w", next, err)
		}
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
CREATE TABLE IF NOT EXISTS symbols (
    id             TEXT    PRIMARY KEY,
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

    file_hash      TEXT
);
CREATE INDEX IF NOT EXISTS idx_sym_project ON symbols(project_id);
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
	_, err := s.db.Exec(`
		INSERT INTO projects(id, path, name, indexed_at, file_count, sym_count, edge_count, schema_version_at_index, binary_version)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			path=excluded.path, name=excluded.name, indexed_at=excluded.indexed_at,
			file_count=excluded.file_count, sym_count=excluded.sym_count, edge_count=excluded.edge_count,
			schema_version_at_index=excluded.schema_version_at_index,
			binary_version=excluded.binary_version`,
		p.ID, p.Path, p.Name, p.IndexedAt.Unix(),
		p.FileCount, p.SymCount, p.EdgeCount, currentSchema, p.BinaryVersion,
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

// ListProjects returns all indexed projects.
func (s *Store) ListProjects() ([]Project, error) {
	// Reader pool (#51).
	rows, err := s.ro.Query(
		`SELECT id, path, name, indexed_at, file_count, sym_count, edge_count, schema_version_at_index, binary_version FROM projects ORDER BY name`)
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
		if err := rows.Scan(&p.ID, &p.Path, &p.Name, &ts, &p.FileCount, &p.SymCount, &p.EdgeCount, &schemaVer, &binVer); err != nil {
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
		`SELECT id, path, name, indexed_at, file_count, sym_count, edge_count, schema_version_at_index, binary_version FROM projects WHERE id=?`, id)
	var p Project
	var ts int64
	var schemaVer sql.NullInt64
	var binVer sql.NullString
	if err := row.Scan(&p.ID, &p.Path, &p.Name, &ts, &p.FileCount, &p.SymCount, &p.EdgeCount, &schemaVer, &binVer); err == sql.ErrNoRows {
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

	// Schema version
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&report.SchemaVersion); err != nil {
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

	// Per-language extraction coverage
	rows, err := s.db.Query(`
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
	tupleRows, err := s.db.Query(`
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

// DeleteProject removes a project and all its data.
func (s *Store) DeleteProject(id string) error {
	return s.withTx(func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM edges   WHERE project_id=?`,
			`DELETE FROM symbols WHERE project_id=?`,
			`DELETE FROM files   WHERE project_id=?`,
			`DELETE FROM adrs    WHERE project_id=?`,
			`DELETE FROM projects WHERE id=?`,
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
			 extraction_confidence)
		VALUES (?,?,?,?,?,?,?, ?,?,?,?, ?,?,?,?, ?,?,?,?,?, ?)`)
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
				ns(sym.FileHash), conf,
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteSymbolsForFile removes all symbols (and edges) from one file.
func (s *Store) DeleteSymbolsForFile(projectID, filePath string) error {
	return s.withTx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT id FROM symbols WHERE project_id=? AND file_path=?`, projectID, filePath)
		if err != nil {
			return err
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`DELETE FROM edges WHERE from_id=? OR to_id=?`, id, id); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`DELETE FROM symbols WHERE project_id=? AND file_path=?`, projectID, filePath)
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

// GetSymbolsForFile returns all symbols in a file ordered by byte offset.
func (s *Store) GetSymbolsForFile(projectID, filePath string) ([]Symbol, error) {
	return s.querySymbols(symSelectFrom+` WHERE project_id=? AND file_path=? ORDER BY start_byte`, projectID, filePath)
}

// GetHotspots returns the most-called symbols (highest in-degree) for a project.
func (s *Store) GetHotspots(projectID string, limit int) ([]Symbol, error) {
	return s.querySymbols(`
		SELECT s.id, s.project_id, s.file_path, s.name, s.qualified_name, s.kind, s.language,
		       s.start_byte, s.end_byte, s.start_line, s.end_line,
		       s.signature, s.return_type, s.docstring, s.parent,
		       s.complexity, s.is_exported, s.is_test, s.is_entry_point, s.file_hash,
		       s.extraction_confidence
		FROM symbols s
		JOIN (SELECT to_id, COUNT(*) AS cnt FROM edges WHERE project_id=? GROUP BY to_id) e ON s.id=e.to_id
		ORDER BY cnt DESC LIMIT ?`, projectID, limit)
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
		       s.extraction_confidence,
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

// BulkUpsertEdges inserts edges, ignoring duplicates.
func (s *Store) BulkUpsertEdges(edges []Edge) error {
	if len(edges) == 0 {
		return nil
	}
	return s.withTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO edges(project_id, from_id, to_id, kind, confidence, properties)
		VALUES (?,?,?,?,?,?)`)
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
			if _, err := stmt.Exec(e.ProjectID, e.FromID, e.ToID, e.Kind, e.Confidence, ns(propsJSON)); err != nil {
				return err
			}
		}
		return nil
	})
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
	if len(details) > extractionFailureDetailsCap {
		details = details[:extractionFailureDetailsCap] + "…[truncated]"
	}
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO extraction_failures (project_id, file_path, language, reason, details, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, file_path, reason) DO UPDATE SET
			details      = excluded.details,
			last_seen_at = excluded.last_seen_at`,
		projectID, filePath, language, reason, details, now, now)
	return err
}

// ListExtractionFailures returns the most-recent extraction failures for a
// project, ordered by last_seen_at DESC. limit <= 0 returns all rows.
//
// Reads via the reader pool (#51) — pure SELECT.
func (s *Store) ListExtractionFailures(projectID string, limit int) ([]ExtractionFailure, error) {
	q := `SELECT id, project_id, file_path, language, reason, details, first_seen_at, last_seen_at
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
		var details sql.NullString
		if err := rows.Scan(&f.ID, &f.ProjectID, &f.FilePath, &f.Language, &f.Reason, &details, &first, &last); err != nil {
			return nil, err
		}
		f.Details = details.String
		f.FirstSeenAt = time.Unix(first, 0)
		f.LastSeenAt = time.Unix(last, 0)
		out = append(out, f)
	}
	return out, rows.Err()
}

// ClearExtractionFailures removes all failure rows for a project. Used by
// `pincher index --force` (when the user wants a clean slate after fixing
// the underlying issues) and by integration tests.
func (s *Store) ClearExtractionFailures(projectID string) error {
	_, err := s.db.Exec(`DELETE FROM extraction_failures WHERE project_id = ?`, projectID)
	return err
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

// DeleteADR removes an ADR entry.
func (s *Store) DeleteADR(projectID, key string) error {
	_, err := s.db.Exec(`DELETE FROM adrs WHERE project_id=? AND key=?`, projectID, key)
	return err
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
			queries_total, queries_zero_result, queries_retried_succeeded, tokens_burned_on_failures)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, startedAt.Unix(), time.Now().Unix(), calls, tokensUsed, tokensSaved, costAvoided, httpURL, httpPID, clbl,
		qm.QueriesTotal, qm.QueriesZeroResult, qm.QueriesRetriedSucceeded, qm.TokensBurnedOnFailures,
	)
	return err
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

// GetSessions returns all recorded sessions ordered by start time descending.
// Limit ≤ 0 returns all rows.
func (s *Store) GetSessions(limit int) ([]SessionRow, error) {
	q := `SELECT session_id, started_at, last_seen, calls, tokens_used, tokens_saved,
	             cost_avoided, http_url, http_pid, calls_by_language,
	             queries_total, queries_zero_result, queries_retried_succeeded, tokens_burned_on_failures
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
			&r.QueryMetrics.QueriesTotal, &r.QueryMetrics.QueriesZeroResult, &r.QueryMetrics.QueriesRetriedSucceeded, &r.QueryMetrics.TokensBurnedOnFailures); err != nil {
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
		        COALESCE(SUM(tokens_burned_on_failures),0)
		 FROM sessions`,
	).Scan(&qm.QueriesTotal, &qm.QueriesZeroResult, &qm.QueriesRetriedSucceeded, &qm.TokensBurnedOnFailures)
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
	       extraction_confidence
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
		&sym.ExtractionConfidence,
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
		&sym.ExtractionConfidence,
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
		&sym.ExtractionConfidence, score,
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

// ApproxTokens returns the BPE token count of s using the cl100k_base
// tokenizer (same BPE family as Claude). Falls back to the 4-char heuristic
// if the tokenizer is unavailable.
func ApproxTokens(s string) int {
	if enc := getTokenizer(); enc != nil {
		ids, _, _ := enc.Encode(s)
		return len(ids)
	}
	return (len(s) + 3) / 4
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
