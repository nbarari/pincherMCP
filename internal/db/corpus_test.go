package db

import (
	"testing"
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

// TestFTSCorpusSplit_LegacyStillPopulated asserts the v9 migration is a
// pure addition: every symbol is still in `symbols_fts` (the legacy single
// index). Pins the "zero observable change" invariant for callers that
// haven't switched to the per-corpus API yet.
//
// Each name is checked via MATCH because COUNT(*) on external-content
// FTS5 returns the content table's row count, not the index's.
func TestFTSCorpusSplit_LegacyStillPopulated(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "x.go", Name: "ZZLegFoo",
			QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go"},
		{ID: "s2", ProjectID: "p1", FilePath: "y.yaml", Name: "ZZLegImage",
			QualifiedName: "services.web.image", Kind: "Setting", Language: "YAML"},
		{ID: "s3", ProjectID: "p1", FilePath: "z.tf", Name: "ZZLegWeb",
			QualifiedName: "resource.aws_instance.web", Kind: "Resource", Language: "HCL"},
	})

	for _, name := range []string{"ZZLegFoo", "ZZLegImage", "ZZLegWeb"} {
		var present int
		if err := s.DB().QueryRow(
			`SELECT COUNT(*) FROM symbols_fts WHERE symbols_fts MATCH ?`, name,
		).Scan(&present); err != nil {
			t.Fatalf("MATCH %s: %v", name, err)
		}
		if present != 1 {
			t.Errorf("legacy symbols_fts missing %q (zero-observable-change broken; MATCH count=%d)", name, present)
		}
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

	// Both indexes should no longer match the symbol's name token.
	for _, vtab := range []string{"symbols_fts", "symbols_code_fts"} {
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

// TestRebuildFTS_RebuildsAllFourVtabs extends the existing rebuild-fts
// coverage: after a rebuild, every vtab (legacy + 3 per-corpus) must
// contain the right slice of `symbols` again.
func TestRebuildFTS_RebuildsAllFourVtabs(t *testing.T) {
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

	// Simulate corruption: drop the corpus indexes' delete triggers and
	// remove all symbols. Per-corpus indexes are now ghosts.
	for _, stmt := range []string{
		`DROP TRIGGER sym_fts_corpus_delete`,
		`DROP TRIGGER sym_fts_delete`,
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

	// Each vtab should match its slice of the corpus.
	expectations := []struct {
		vtab string
		name string
	}{
		{"symbols_fts", "ZZRebuildFoo"},
		{"symbols_fts", "ZZRebuildImage"},
		{"symbols_fts", "ZZRebuildIntro"},
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
