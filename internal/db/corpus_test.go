package db

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestClassifyCorpus_FixedRoutingTable pins every documented routing rule
// from the function godoc. Adding a new rule = add a row here. Changing
// an existing rule should be obvious in the diff.
func TestClassifyCorpus_FixedRoutingTable(t *testing.T) {
	cases := []struct {
		name     string
		language string
		kind     string
		want     string
	}{
		// Code-corpus default — known parser-backed languages.
		{"go function", "Go", "Function", CorpusCode},
		{"go method", "Go", "Method", CorpusCode},
		{"go module", "Go", "Module", CorpusCode},
		{"python class", "Python", "Class", CorpusCode},
		{"typescript interface", "TypeScript", "Interface", CorpusCode},
		{"rust enum", "Rust", "Enum", CorpusCode},
		{"bash function", "Bash", "Function", CorpusCode},

		// Config corpus — YAML, JSON, HCL.
		{"yaml setting", "YAML", "Setting", CorpusConfig},
		{"json setting", "JSON", "Setting", CorpusConfig},
		{"hcl resource", "HCL", "Resource", CorpusConfig},
		{"hcl module", "HCL", "Module", CorpusConfig},
		{"hcl variable", "HCL", "Variable", CorpusConfig},

		// Docs corpus — Markdown OR Document kind.
		{"markdown section", "Markdown", "Section", CorpusDocs},
		{"document any-language", "URL", "Document", CorpusDocs},
		{"document empty-language", "", "Document", CorpusDocs},

		// Document kind wins over language even if language says config.
		{"document overrides yaml", "YAML", "Document", CorpusDocs},

		// Unknown language → code (default; the parity test catches missing
		// languages separately).
		{"unknown language", "Klingon", "Function", CorpusCode},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyCorpus(tc.language, tc.kind)
			if got != tc.want {
				t.Errorf("ClassifyCorpus(%q, %q) = %q, want %q", tc.language, tc.kind, got, tc.want)
			}
		})
	}
}

// TestClassifyCorpus_MatchesSQLTriggerRouting is the cross-language parity
// gate. It inserts one symbol per corpus into the symbols table and asserts
// the corpus the SQL trigger routed it to matches what ClassifyCorpus says.
//
// **WHY THIS GATE MATTERS:** The Go classifier and the SQL trigger WHERE
// clauses encode the same routing rules. If they drift (e.g. someone adds
// a language to the Go classifier but not the SQL trigger, or vice versa),
// search results split between the legacy and per-corpus indexes
// inconsistently — a partial outage that's hard to diagnose. This test
// catches drift loudly.
//
// **IMPORTANT — querying external-content FTS5:** `COUNT(*)` on an
// external-content FTS5 vtab returns the row count of the CONTENT TABLE
// (`symbols`), not the FTS5 index — so it gives 1 for every inserted
// symbol regardless of whether the trigger routed it to that vtab. The
// only reliable way to check "is symbol X in vtab Y" is to MATCH a token
// that's unique to X. We use the symbol's name (which is unique per case
// in this test) as the match token.
func TestClassifyCorpus_MatchesSQLTriggerRouting(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	// One representative per corpus. We'd want one per known language but
	// that's a maintenance burden — the routing logic only branches on
	// language IN ('YAML','JSON','HCL') and language='Markdown'. Anything
	// else falls through to code. Three reps + Document covers the rules.
	// Names are unique per case so we can MATCH on them.
	cases := []struct {
		id, name, language, kind string
	}{
		{"go-fn", "ZZGoFn", "Go", "Function"},
		{"yaml-set", "ZZYamlSet", "YAML", "Setting"},
		{"json-set", "ZZJsonSet", "JSON", "Setting"},
		{"hcl-res", "ZZHclRes", "HCL", "Resource"},
		{"md-sec", "ZZMdSec", "Markdown", "Section"},
		{"doc-any", "ZZDocAny", "URL", "Document"},
		{"doc-yaml", "ZZDocYaml", "YAML", "Document"}, // Document kind overrides language
	}

	for _, c := range cases {
		s.BulkUpsertSymbols([]Symbol{{
			ID: c.id, ProjectID: "p1",
			FilePath: "f.x", Name: c.name,
			QualifiedName: "qn." + c.id,
			Kind:          c.kind, Language: c.language,
		}})
	}

	corpusVtab := map[string]string{
		CorpusCode:   "symbols_code_fts",
		CorpusConfig: "symbols_config_fts",
		CorpusDocs:   "symbols_docs_fts",
	}
	for _, c := range cases {
		want := ClassifyCorpus(c.language, c.kind)
		// Assert the symbol is in the wanted vtab and ABSENT from the others.
		for corpus, vtab := range corpusVtab {
			var present int
			err := s.DB().QueryRow(
				`SELECT COUNT(*) FROM `+vtab+` WHERE `+vtab+` MATCH ?`,
				c.name,
			).Scan(&present)
			if err != nil {
				t.Fatalf("query %s: %v", vtab, err)
			}
			if corpus == want && present != 1 {
				t.Errorf("symbol %s (lang=%s, kind=%s): expected in %s but MATCH found %d",
					c.id, c.language, c.kind, vtab, present)
			}
			if corpus != want && present != 0 {
				t.Errorf("symbol %s (lang=%s, kind=%s): unexpectedly in %s (corpus=%s, want=%s)",
					c.id, c.language, c.kind, vtab, corpus, want)
			}
		}
	}
}

// TestFTSCorpusSplit_LegacyDropped asserts the v12 migration (#106)
// removed `symbols_fts`. Pins the post-removal contract — querying the
// legacy vtab errors with "no such table" on every fresh install.
//
// Replaces the prior TestFTSCorpusSplit_LegacyStillPopulated which
// pinned the v9 zero-observable-change invariant, now obsolete.
func TestFTSCorpusSplit_LegacyDropped(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "x.go", Name: "ZZLegFoo",
			QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go"},
	})

	var present int
	err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM symbols_fts WHERE symbols_fts MATCH ?`, "ZZLegFoo",
	).Scan(&present)
	if err == nil {
		t.Fatalf("legacy symbols_fts should not exist after v12 migration; got count=%d", present)
	}
	if !strings.Contains(err.Error(), "no such table") {
		t.Errorf("expected 'no such table' error, got: %v", err)
	}
}

// TestFTSCorpusSplit_BackfillOnUpgrade simulates an upgrade path: open a
// v8-shaped DB (no per-corpus indexes), insert symbols, then run the v9
// migration manually and assert the per-corpus indexes were backfilled.
//
// We can't easily construct a "real" v8 DB without re-implementing the
// migration system — instead, we open at v9 (which is fine since
// migrations run forward), then assert that symbols inserted BEFORE
// the test's writes are present in the per-corpus indexes. The
// indistinguishable upgrade-vs-fresh case is fine because BulkUpsertSymbols
// fires the same triggers either way.
//
// More direct: we DROP the per-corpus indexes and re-run the migration
// to prove backfill works on a populated symbols table.
func TestFTSCorpusSplit_BackfillOnUpgrade(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "x.go", Name: "ZZBfFoo",
			QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go"},
		{ID: "s2", ProjectID: "p1", FilePath: "y.yaml", Name: "ZZBfImage",
			QualifiedName: "services.web.image", Kind: "Setting", Language: "YAML"},
	})

	// Drop everything the v9 migration created — simulating a pre-v9 DB.
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS sym_fts_corpus_insert`,
		`DROP TRIGGER IF EXISTS sym_fts_corpus_delete`,
		`DROP TRIGGER IF EXISTS sym_fts_corpus_update`,
		`DROP TABLE IF EXISTS symbols_code_fts`,
		`DROP TABLE IF EXISTS symbols_config_fts`,
		`DROP TABLE IF EXISTS symbols_docs_fts`,
	} {
		if _, err := s.DB().Exec(stmt); err != nil {
			t.Fatalf("drop pre-v9: %v", err)
		}
	}

	// Re-run the migration as if upgrading from v8 to v9. The backfill
	// at the bottom should populate the new indexes from the existing
	// symbols rows.
	if _, err := s.DB().Exec(ftsCorpusSplitDDL); err != nil {
		t.Fatalf("re-run v9 migration: %v", err)
	}

	// Backfill check via MATCH (COUNT(*) on external-content FTS5 returns
	// the content table count, not the index — see test header note).
	for _, c := range []struct{ name, vtab string }{
		{"ZZBfFoo", "symbols_code_fts"},
		{"ZZBfImage", "symbols_config_fts"},
	} {
		var present int
		if err := s.DB().QueryRow(
			`SELECT COUNT(*) FROM `+c.vtab+` WHERE `+c.vtab+` MATCH ?`,
			c.name,
		).Scan(&present); err != nil {
			t.Fatalf("MATCH %s in %s: %v", c.name, c.vtab, err)
		}
		if present != 1 {
			t.Errorf("after backfill: %q missing from %s (MATCH count=%d)", c.name, c.vtab, present)
		}
	}
}

// TestFTSCorpusSplit_DeleteTriggerCleansAllVtabs asserts that deleting a
// symbol removes it from BOTH the legacy and the appropriate per-corpus
// vtab. A regression here would leave ghost entries in search results
// after re-indexing — exactly the kind of bug rebuild-fts exists to fix.
func TestFTSCorpusSplit_DeleteTriggerCleansAllVtabs(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "x.go", Name: "ZZDelFoo",
			QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go"},
	})

	// Delete by file path — the indexer's normal cleanup pattern.
	if err := s.DeleteSymbolsForFile("p1", "x.go"); err != nil {
		t.Fatalf("DeleteSymbolsForFile: %v", err)
	}

	// The relevant per-corpus index should no longer match the symbol's
	// name token. (Legacy `symbols_fts` was removed in #106's v12 migration.)
	for _, vtab := range []string{"symbols_code_fts"} {
		var present int
		if err := s.DB().QueryRow(
			`SELECT COUNT(*) FROM `+vtab+` WHERE `+vtab+` MATCH ?`,
			"ZZDelFoo",
		).Scan(&present); err != nil {
			t.Fatalf("query %s: %v", vtab, err)
		}
		if present != 0 {
			t.Errorf("%s still has %d MATCH hits for deleted symbol", vtab, present)
		}
	}
}

// TestSearchSymbolsByCorpus_RoutingTable pins corpus → vtab routing.
// Empty defaults to code. Each named corpus goes to its index. Unknown
// corpus names error loudly. (`all` was removed in #106 and now errors
// the same as any other unknown name; the MCP search handler still
// soft-redirects `all` → `code` for backwards compat at the API layer.)
func TestSearchSymbolsByCorpus_RoutingTable(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		{ID: "s-go", ProjectID: "p1", FilePath: "x.go", Name: "ZZRtFoo",
			QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go"},
		{ID: "s-yaml", ProjectID: "p1", FilePath: "y.yaml", Name: "ZZRtImage",
			QualifiedName: "services.web.image", Kind: "Setting", Language: "YAML"},
		{ID: "s-md", ProjectID: "p1", FilePath: "z.md", Name: "ZZRtIntro",
			QualifiedName: "doc.intro", Kind: "Section", Language: "Markdown"},
	})

	// Each corpus query should return ONLY its own slice.
	// Default flipped in #32 part 3: empty corpus now routes to code, NOT
	// to the legacy mixed index. `all` is the explicit opt-in for legacy.
	for _, c := range []struct {
		name      string
		corpus    string
		matchTok  string
		wantHitID string
	}{
		{"code finds Go", CorpusCode, "ZZRtFoo", "s-go"},
		{"config finds YAML", CorpusConfig, "ZZRtImage", "s-yaml"},
		{"docs finds Markdown", CorpusDocs, "ZZRtIntro", "s-md"},
		// empty defaults to code — finds the Go function.
		{"empty = code finds Go", "", "ZZRtFoo", "s-go"},
	} {
		t.Run(c.name, func(t *testing.T) {
			results, err := s.SearchSymbolsByCorpus("p1", c.matchTok, "", "", c.corpus, 10)
			if err != nil {
				t.Fatalf("SearchSymbolsByCorpus: %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("got %d results, want 1", len(results))
			}
			if results[0].Symbol.ID != c.wantHitID {
				t.Errorf("got %s, want %s", results[0].Symbol.ID, c.wantHitID)
			}
		})
	}

	// Regression gate for the part-3 flip: empty corpus MUST NOT find a
	// YAML symbol now (pre-flip it would have, via the legacy index).
	// If corpusVtab gets reverted to "" → symbols_fts, this case
	// surfaces the regression immediately.
	t.Run("empty does NOT find YAML (flip regression gate)", func(t *testing.T) {
		results, err := s.SearchSymbolsByCorpus("p1", "ZZRtImage", "", "", "", 10)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("empty corpus returned %d hits for YAML — default flipped back to legacy?", len(results))
		}
	})

	// Cross-corpus isolation: code corpus must NOT find YAML or Markdown tokens.
	for _, c := range []struct{ corpus, matchTok string }{
		{CorpusCode, "ZZRtImage"},   // YAML hidden from code
		{CorpusCode, "ZZRtIntro"},   // Markdown hidden from code
		{CorpusConfig, "ZZRtFoo"},   // Go hidden from config
		{CorpusConfig, "ZZRtIntro"}, // Markdown hidden from config
		{CorpusDocs, "ZZRtFoo"},     // Go hidden from docs
		{CorpusDocs, "ZZRtImage"},   // YAML hidden from docs
	} {
		t.Run("isolation_"+c.corpus+"_"+c.matchTok, func(t *testing.T) {
			results, err := s.SearchSymbolsByCorpus("p1", c.matchTok, "", "", c.corpus, 10)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if len(results) != 0 {
				t.Errorf("corpus=%s leaked %q (got %d results)", c.corpus, c.matchTok, len(results))
			}
		})
	}
}

// TestSearchSymbolsByCorpus_UnknownCorpusErrors asserts a typo in the
// corpus parameter doesn't silently fall through to legacy. The error
// is the only signal a caller has that they got the wrong shape.
func TestSearchSymbolsByCorpus_UnknownCorpusErrors(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	for _, bad := range []string{"COde", "src", "yaml", "Code", "doc"} {
		_, err := s.SearchSymbolsByCorpus("p1", "anything", "", "", bad, 10)
		if err == nil {
			t.Errorf("corpus=%q: expected error, got nil", bad)
			continue
		}
		// Error must mention the bad corpus name so the caller can debug.
		if !contains(err.Error(), bad) {
			t.Errorf("corpus=%q: error %q doesn't mention the bad value", bad, err.Error())
		}
	}
}

// TestSearchSymbols_BackwardCompat asserts the SearchSymbols shim
// produces identical results to SearchSymbolsByCorpus("") — the
// "no behavior change for existing callers" invariant for #32 part 2.
func TestSearchSymbols_BackwardCompat(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "a.go", Name: "ZZBcFoo",
			QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go"},
		{ID: "s2", ProjectID: "p1", FilePath: "b.go", Name: "ZZBcBar",
			QualifiedName: "pkg.Bar", Kind: "Function", Language: "Go"},
	})
	legacy, err := s.SearchSymbols("p1", "ZZBc*", "", "", 10)
	if err != nil {
		t.Fatalf("legacy: %v", err)
	}
	corpus, err := s.SearchSymbolsByCorpus("p1", "ZZBc*", "", "", "", 10)
	if err != nil {
		t.Fatalf("corpus(empty): %v", err)
	}
	if len(legacy) != len(corpus) {
		t.Fatalf("len mismatch: legacy=%d corpus(empty)=%d", len(legacy), len(corpus))
	}
	for i := range legacy {
		if legacy[i].Symbol.ID != corpus[i].Symbol.ID {
			t.Errorf("row %d: legacy=%s corpus(empty)=%s", i, legacy[i].Symbol.ID, corpus[i].Symbol.ID)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestEnsureSymbolIDColumn_PresentOnFreshV9 confirms the baseline:
// a fresh v9 DB has the symbol_id generated column. (Without this
// guard, a future schema refactor could regress the v6 column-add and
// we'd never know.)
func TestEnsureSymbolIDColumn_PresentOnFreshV9(t *testing.T) {
	s := newTestStore(t)
	var present int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_xinfo('symbols') WHERE name = 'symbol_id'`,
	).Scan(&present); err != nil {
		t.Fatalf("xinfo: %v", err)
	}
	if present != 1 {
		t.Errorf("fresh v9 DB missing symbol_id column (xinfo count=%d)", present)
	}
}

// TestEnsureSymbolIDColumn_OptionAReplica is the closer reproducer:
// build a database that LOOKS like an Option-A-lineage DB by creating
// the symbols table without the generated column, then verify Open()'s
// repair pass adds it.
func TestEnsureSymbolIDColumn_OptionAReplica(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + dir + "/pincher.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"

	// Open raw, before pincher's migrate() runs.
	rawDB, err := openRaw(dsn)
	if err != nil {
		t.Fatalf("openRaw: %v", err)
	}
	// Build the schema EXCLUDING the symbol_id column on `symbols`.
	for _, stmt := range []string{
		`CREATE TABLE projects (id TEXT PRIMARY KEY, path TEXT, name TEXT, indexed_at INTEGER, file_count INTEGER, sym_count INTEGER, edge_count INTEGER, schema_version_at_index INTEGER)`,
		// symbols table WITHOUT the symbol_id column — this is the
		// Option-A-lineage shape.
		`CREATE TABLE symbols (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id),
			file_path TEXT NOT NULL,
			name TEXT NOT NULL,
			qualified_name TEXT NOT NULL,
			kind TEXT NOT NULL,
			language TEXT NOT NULL,
			start_byte INTEGER NOT NULL,
			end_byte INTEGER NOT NULL,
			start_line INTEGER NOT NULL,
			end_line INTEGER NOT NULL,
			signature TEXT,
			return_type TEXT,
			docstring TEXT,
			parent TEXT,
			complexity INTEGER,
			is_exported INTEGER,
			is_test INTEGER,
			is_entry_point INTEGER,
			file_hash TEXT,
			extraction_confidence REAL DEFAULT 1.0
		)`,
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		// Mark the DB as already at the current schema_version so
		// pincher's migrate() doesn't try to apply numbered migrations
		// (they assume the column already exists from v6). We want to
		// exercise JUST the repair pass that runs after migrations.
		// The version must be exactly len(schemaMigrations)+1 (the
		// current version) — anything higher trips the "newer than
		// binary" guard before the repair runs. Using a string-formatted
		// value keeps the test in lock-step with the migration list as
		// new migrations land (e.g. the v10→v11 sessions http_url cols).
		fmt.Sprintf(`INSERT INTO schema_version (version) VALUES (%d)`, len(schemaMigrations)+1),
	} {
		if _, err := rawDB.Exec(stmt); err != nil {
			t.Fatalf("setup stmt %q: %v", stmt, err)
		}
	}
	rawDB.Close()

	// Now Open() through pincher — migrate() should detect the missing
	// column via pragma_table_xinfo and repair it.
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Schema version 999 (newer than this binary's len(schemaMigrations)+1)
	// would normally trigger the "newer than this binary" guard, but that
	// guard fires before migrate() runs. So this test path actually does
	// trigger the guard — let's verify by reading the version. If the
	// guard fired, Open() would have returned an error already. So either
	// the guard is permissive enough, or we need to set version to current.
	// Either way, the repair pass runs after migrations, so even if the
	// version guard doesn't fire, the repair runs and adds the column.

	var present int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_xinfo('symbols') WHERE name = 'symbol_id'`,
	).Scan(&present); err != nil {
		t.Fatalf("post-repair xinfo: %v", err)
	}
	if present != 1 {
		t.Errorf("repair pass did NOT add symbol_id column (present=%d)", present)
	}

	// User-visible failure mode from #83: JOIN against the per-corpus
	// FTS5 vtab using `f.symbol_id = s.id`. Pre-repair this errors with
	// `no such column: T.symbol_id`. Post-repair it succeeds.
	//
	// Need to seed a symbol so the FTS5 trigger has something to fire
	// against, and the per-corpus vtabs need to exist. The latter is
	// created by the v9 migration body (we marked schema_version=9 in
	// setup, so v9 DDL never ran in this test). Create them manually
	// as a one-off so the JOIN has something to query against.
	if _, err := s.DB().Exec(ftsCorpusSplitDDL); err != nil {
		t.Fatalf("create per-corpus vtabs: %v", err)
	}
	if err := s.UpsertProject(testProject("p1")); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := s.BulkUpsertSymbols([]Symbol{{
		ID: "s1", ProjectID: "p1", FilePath: "x.go", Name: "ZZRepairFoo",
		QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go",
	}}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	// The actual #83 reproducer query.
	rows, err := s.DB().Query(
		`SELECT s.kind FROM symbols s JOIN symbols_code_fts f ON f.symbol_id=s.id ` +
			`WHERE symbols_code_fts MATCH 'ZZRepairFoo'`)
	if err != nil {
		t.Fatalf("symbol_id JOIN query: %v (this is the #83 user-visible failure)", err)
	}
	defer rows.Close()
	gotRows := 0
	for rows.Next() {
		gotRows++
	}
	if gotRows == 0 {
		t.Errorf("symbol_id JOIN returned 0 rows — repair landed but query path still broken")
	}
}

// openRaw opens the same SQLite file pincher uses, without running
// pincher's migrate() — needed by the Option-A-replica test to set up
// a non-canonical starting state.
func openRaw(dsn string) (*sql.DB, error) {
	return sql.Open("sqlite", dsn)
}
// coverage: after a rebuild, every per-corpus vtab must contain the
// right slice of `symbols` again. The legacy `symbols_fts` vtab was
// removed in #106 (v12 migration) — the rebuild contract now covers
// only the three corpus-specific indexes.
func TestRebuildFTS_RebuildsAllCorpusVtabs(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "x.go", Name: "ZZRebuildFoo",
			QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go"},
		{ID: "s2", ProjectID: "p1", FilePath: "y.yaml", Name: "ZZRebuildImage",
			QualifiedName: "services.web.image", Kind: "Setting", Language: "YAML"},
		{ID: "s3", ProjectID: "p1", FilePath: "z.md", Name: "ZZRebuildIntro",
			QualifiedName: "doc.intro", Kind: "Section", Language: "Markdown"},
	})

	// Simulate corruption: drop the corpus indexes' delete trigger and
	// remove all symbols. Per-corpus indexes are now ghosts.
	for _, stmt := range []string{
		`DROP TRIGGER sym_fts_corpus_delete`,
		`DELETE FROM symbols`,
	} {
		if _, err := s.DB().Exec(stmt); err != nil {
			t.Fatalf("setup corruption: %v", err)
		}
	}

	// Re-add symbols (triggers are partially gone but inserts still fire
	// the per-corpus insert trigger).
	s.BulkUpsertSymbols([]Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "x.go", Name: "ZZRebuildFoo",
			QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go"},
		{ID: "s2", ProjectID: "p1", FilePath: "y.yaml", Name: "ZZRebuildImage",
			QualifiedName: "services.web.image", Kind: "Setting", Language: "YAML"},
		{ID: "s3", ProjectID: "p1", FilePath: "z.md", Name: "ZZRebuildIntro",
			QualifiedName: "doc.intro", Kind: "Section", Language: "Markdown"},
	})

	if _, err := s.RebuildFTS(); err != nil {
		t.Fatalf("RebuildFTS: %v", err)
	}

	// Each per-corpus vtab should match its slice of the corpus.
	expectations := []struct {
		vtab string
		name string
	}{
		{"symbols_code_fts", "ZZRebuildFoo"},
		{"symbols_config_fts", "ZZRebuildImage"},
		{"symbols_docs_fts", "ZZRebuildIntro"},
	}
	for _, e := range expectations {
		var present int
		if err := s.DB().QueryRow(
			`SELECT COUNT(*) FROM `+e.vtab+` WHERE `+e.vtab+` MATCH ?`,
			e.name,
		).Scan(&present); err != nil {
			t.Fatalf("query %s: %v", e.vtab, err)
		}
		if present != 1 {
			t.Errorf("%s should match %q after rebuild (got %d hits)", e.vtab, e.name, present)
		}
	}

	// Cross-corpus negatives: each name must only match in its own corpus.
	for _, e := range []struct{ vtab, name string }{
		{"symbols_config_fts", "ZZRebuildFoo"},   // Go ! in config
		{"symbols_docs_fts", "ZZRebuildFoo"},     // Go ! in docs
		{"symbols_code_fts", "ZZRebuildImage"},   // YAML ! in code
		{"symbols_docs_fts", "ZZRebuildImage"},   // YAML ! in docs
		{"symbols_code_fts", "ZZRebuildIntro"},   // MD ! in code
		{"symbols_config_fts", "ZZRebuildIntro"}, // MD ! in config
	} {
		var present int
		if err := s.DB().QueryRow(
			`SELECT COUNT(*) FROM `+e.vtab+` WHERE `+e.vtab+` MATCH ?`,
			e.name,
		).Scan(&present); err != nil {
			t.Fatalf("cross-query %s: %v", e.vtab, err)
		}
		if present != 0 {
			t.Errorf("%q leaked into %s (cross-corpus contamination, got %d hits)", e.name, e.vtab, present)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ExtractionFailureCountsByReason — powers the QN-collision snapshot gate
// ─────────────────────────────────────────────────────────────────────────────

// TestExtractionFailureCountsByReason_Empty pins the no-failures path:
// a fresh project MUST return an empty map. Under the JSON encoder
// this becomes `{}`, which is the "healthy" snapshot value committed
// for every corpus.
func TestExtractionFailureCountsByReason_Empty(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	got, err := s.ExtractionFailureCountsByReason("p1")
	if err != nil {
		t.Fatalf("ExtractionFailureCountsByReason: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map for fresh project, got %v", got)
	}
}

// TestExtractionFailureCountsByReason_GroupsByReason exercises the
// GROUP BY aggregation: multiple failures of the same reason collapse
// to one count; failures with different reasons appear as separate
// keys. This is the shape that drives the snapshot diff at PR time.
func TestExtractionFailureCountsByReason_GroupsByReason(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	// Three QN collisions across three files; one byte_range failure.
	for _, f := range []string{"a.c", "b.c", "c.c"} {
		if err := s.RecordExtractionFailure("p1", f, "C", "qualified_name_collision", "details"); err != nil {
			t.Fatalf("record %s: %v", f, err)
		}
	}
	if err := s.RecordExtractionFailure("p1", "d.go", "Go", "byte_range_negative", "end<=start"); err != nil {
		t.Fatalf("record byte_range: %v", err)
	}

	got, err := s.ExtractionFailureCountsByReason("p1")
	if err != nil {
		t.Fatalf("ExtractionFailureCountsByReason: %v", err)
	}
	if got["qualified_name_collision"] != 3 {
		t.Errorf("qualified_name_collision count = %d, want 3", got["qualified_name_collision"])
	}
	if got["byte_range_negative"] != 1 {
		t.Errorf("byte_range_negative count = %d, want 1", got["byte_range_negative"])
	}
}

// TestExtractionFailureCountsByReason_PerProject — counts MUST scope
// to the requested project. Gates against a future change that
// accidentally aggregates across projects.
func TestExtractionFailureCountsByReason_PerProject(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.UpsertProject(testProject("p2"))

	s.RecordExtractionFailure("p1", "a.c", "C", "qualified_name_collision", "")
	s.RecordExtractionFailure("p2", "b.c", "C", "qualified_name_collision", "")
	s.RecordExtractionFailure("p2", "c.c", "C", "qualified_name_collision", "")

	got1, _ := s.ExtractionFailureCountsByReason("p1")
	got2, _ := s.ExtractionFailureCountsByReason("p2")

	if got1["qualified_name_collision"] != 1 {
		t.Errorf("p1 count = %d, want 1", got1["qualified_name_collision"])
	}
	if got2["qualified_name_collision"] != 2 {
		t.Errorf("p2 count = %d, want 2", got2["qualified_name_collision"])
	}
}
