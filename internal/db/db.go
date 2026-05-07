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
type Store struct {
	db   *sql.DB
	Path string
}

// DataDir returns the platform data directory for pincherMCP.
func DataDir() (string, error) {
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
	return s, nil
}

// DB returns the raw *sql.DB for advanced callers (e.g. the Cypher executor).
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

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
	if _, err := s.db.Exec(
		`INSERT INTO schema_version(version) SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM schema_version)`,
	); err != nil {
		return fmt.Errorf("init schema_version: %w", err)
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
	return nil
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
//	  symbols_fts virtual table with built-in BM25 ranking
//	  Auto-synced via AFTER INSERT/UPDATE/DELETE triggers
const schema = `
CREATE TABLE IF NOT EXISTS projects (
    id          TEXT    PRIMARY KEY,
    path        TEXT    NOT NULL,
    name        TEXT    NOT NULL,
    indexed_at  INTEGER,
    file_count  INTEGER DEFAULT 0,
    sym_count   INTEGER DEFAULT 0,
    edge_count  INTEGER DEFAULT 0
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

-- Layer 3: FTS5 full-text search with BM25 ranking.
-- content= avoids storing duplicate text; triggers keep the index in sync.
CREATE VIRTUAL TABLE IF NOT EXISTS symbols_fts USING fts5(
    symbol_id UNINDEXED,
    name,
    qualified_name,
    signature,
    docstring,
    content='symbols',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 1'
);

CREATE TRIGGER IF NOT EXISTS sym_fts_insert AFTER INSERT ON symbols BEGIN
    INSERT INTO symbols_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    VALUES (new.rowid, new.id, new.name, new.qualified_name,
            COALESCE(new.signature,''), COALESCE(new.docstring,''));
END;
CREATE TRIGGER IF NOT EXISTS sym_fts_delete AFTER DELETE ON symbols BEGIN
    INSERT INTO symbols_fts(symbols_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    VALUES ('delete', old.rowid, old.id, old.name, old.qualified_name,
            COALESCE(old.signature,''), COALESCE(old.docstring,''));
END;
CREATE TRIGGER IF NOT EXISTS sym_fts_update AFTER UPDATE ON symbols BEGIN
    INSERT INTO symbols_fts(symbols_fts, rowid, symbol_id, name, qualified_name, signature, docstring)
    VALUES ('delete', old.rowid, old.id, old.name, old.qualified_name,
            COALESCE(old.signature,''), COALESCE(old.docstring,''));
    INSERT INTO symbols_fts(rowid, symbol_id, name, qualified_name, signature, docstring)
    VALUES (new.rowid, new.id, new.name, new.qualified_name,
            COALESCE(new.signature,''), COALESCE(new.docstring,''));
END;

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
	ID        string
	Path      string
	Name      string
	IndexedAt time.Time
	FileCount int
	SymCount  int
	EdgeCount int
}

// SearchResult is a FTS5 match returned by SearchSymbols.
type SearchResult struct {
	Symbol Symbol
	Score  float64
}

// ─────────────────────────────────────────────────────────────────────────────
// Project operations
// ─────────────────────────────────────────────────────────────────────────────

// UpsertProject creates or updates a project record.
func (s *Store) UpsertProject(p Project) error {
	_, err := s.db.Exec(`
		INSERT INTO projects(id, path, name, indexed_at, file_count, sym_count, edge_count)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			path=excluded.path, name=excluded.name, indexed_at=excluded.indexed_at,
			file_count=excluded.file_count, sym_count=excluded.sym_count, edge_count=excluded.edge_count`,
		p.ID, p.Path, p.Name, p.IndexedAt.Unix(),
		p.FileCount, p.SymCount, p.EdgeCount,
	)
	return err
}

// ListProjects returns all indexed projects.
func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(
		`SELECT id, path, name, indexed_at, file_count, sym_count, edge_count FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var ts int64
		if err := rows.Scan(&p.ID, &p.Path, &p.Name, &ts, &p.FileCount, &p.SymCount, &p.EdgeCount); err != nil {
			return nil, err
		}
		p.IndexedAt = time.Unix(ts, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProject returns a single project by ID, or nil if not found.
func (s *Store) GetProject(id string) (*Project, error) {
	row := s.db.QueryRow(
		`SELECT id, path, name, indexed_at, file_count, sym_count, edge_count FROM projects WHERE id=?`, id)
	var p Project
	var ts int64
	if err := row.Scan(&p.ID, &p.Path, &p.Name, &ts, &p.FileCount, &p.SymCount, &p.EdgeCount); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	p.IndexedAt = time.Unix(ts, 0)
	return &p, nil
}

// LanguageCoverage describes extraction quality for one language.
type LanguageCoverage struct {
	Language   string  `json:"language"`
	Parser     string  `json:"parser"`   // "AST" or "Regex"
	Confidence float64 `json:"confidence"` // avg extraction_confidence for this language
	Symbols    int     `json:"symbols"`
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
	report := &HealthReport{DBPath: s.Path}

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
	return report, rows.Err()
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
func (s *Store) GetSymbol(id string) (*Symbol, error) {
	row := s.db.QueryRow(symSelectFrom+` WHERE id=?`, id)
	return scanOneSymbol(row)
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
func (s *Store) SearchSymbols(projectID, query, kind, language string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `
		SELECT s.id, s.project_id, s.file_path, s.name, s.qualified_name, s.kind, s.language,
		       s.start_byte, s.end_byte, s.start_line, s.end_line,
		       s.signature, s.return_type, s.docstring, s.parent,
		       s.complexity, s.is_exported, s.is_test, s.is_entry_point, s.file_hash,
		       s.extraction_confidence,
		       bm25(symbols_fts) AS score
		FROM symbols_fts
		JOIN symbols s ON s.rowid = symbols_fts.rowid
		WHERE symbols_fts MATCH ?`
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
	q += " ORDER BY score LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
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
	rows, err := s.db.Query(q, args...)
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
func (s *Store) TraceViaCTE(startID, direction string, edgeKinds []string, maxDepth int) ([]TraceResult, error) {
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

		q := `WITH RECURSIVE reach(id, depth, via) AS (
			SELECT ?, 0, ''
			UNION ALL
			SELECT ` + selectNeighbor + `, r.depth + 1, e.kind
			FROM reach r
			JOIN edges e ON ` + joinCond + ` AND e.kind IN (` + in + `)
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
func (s *Store) GraphStats(projectID string) (symCount, edgeCount int, kindCounts, edgeKindCounts map[string]int, err error) {
	kindCounts = make(map[string]int)
	edgeKindCounts = make(map[string]int)

	if err = s.db.QueryRow(`SELECT COUNT(*) FROM symbols WHERE project_id=?`, projectID).Scan(&symCount); err != nil {
		return
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE project_id=?`, projectID).Scan(&edgeCount); err != nil {
		return
	}

	rows, err2 := s.db.Query(`SELECT kind, COUNT(*) FROM symbols WHERE project_id=? GROUP BY kind`, projectID)
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

	erows, err3 := s.db.Query(`SELECT kind, COUNT(*) FROM edges WHERE project_id=? GROUP BY kind`, projectID)
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

// ─────────────────────────────────────────────────────────────────────────────
// File hash operations (incremental reindex)
// ─────────────────────────────────────────────────────────────────────────────

// GetFileHash returns the stored content hash for a file, or "" if not indexed.
func (s *Store) GetFileHash(projectID, path string) string {
	var hash string
	_ = s.db.QueryRow(`SELECT hash FROM files WHERE project_id=? AND path=?`, projectID, path).Scan(&hash)
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
	err := s.db.QueryRow(`SELECT value FROM adrs WHERE project_id=? AND key=?`, projectID, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return value, err == nil, err
}

// ListADRs returns all ADR entries for a project.
func (s *Store) ListADRs(projectID string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM adrs WHERE project_id=? ORDER BY key`, projectID)
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
func (s *Store) RecordSession(sessionID string, startedAt time.Time, calls, tokensUsed, tokensSaved int64, costAvoided float64) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO sessions(session_id, started_at, last_seen, calls, tokens_used, tokens_saved, cost_avoided)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sessionID, startedAt.Unix(), time.Now().Unix(), calls, tokensUsed, tokensSaved, costAvoided,
	)
	return err
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
}

// GetSessions returns all recorded sessions ordered by start time descending.
// Limit ≤ 0 returns all rows.
func (s *Store) GetSessions(limit int) ([]SessionRow, error) {
	q := `SELECT session_id, started_at, last_seen, calls, tokens_used, tokens_saved, cost_avoided
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
		if err := rows.Scan(&r.SessionID, &startedUnix, &lastSeenUnix, &r.Calls, &r.TokensUsed, &r.TokensSaved, &r.CostAvoided); err != nil {
			return nil, err
		}
		r.StartedAt = time.Unix(startedUnix, 0)
		r.LastSeen = time.Unix(lastSeenUnix, 0)
		out = append(out, r)
	}
	return out, rows.Err()
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
func (s *Store) querySymbols(q string, args ...any) ([]Symbol, error) {
	rows, err := s.db.Query(q, args...)
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
func ProjectIDFromPath(path string) string {
	// Use the absolute path itself as the ID — simple and stable.
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
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
