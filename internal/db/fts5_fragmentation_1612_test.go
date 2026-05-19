package db

import (
	"testing"
)

// #1612 v0.87: live-DB exercise of Store.FTS5Fragmentation. The
// advisory's unit tests in internal/server cover the shape; this
// test pins the contract that:
//   - The three known corpora always appear in the canonical order.
//   - A fresh DB with no symbols has IdxRows==0 / DataRows==0 / Ratio==0
//     (no division-by-zero, no spurious advisory).
//   - Inserting symbols + rebuilding FTS5 keeps ratios in the healthy
//     band (< the 10x threshold). Provides the floor that catches any
//     future regression where post-rebuild fragmentation creeps high.

func TestFTS5Fragmentation_EmptyDB_ReturnsZeroRatio_1612(t *testing.T) {
	store := newTestStore(t)
	rows, err := store.FTS5Fragmentation()
	if err != nil {
		t.Fatalf("FTS5Fragmentation: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 corpora (code/config/docs); got %d: %+v", len(rows), rows)
	}
	wantCorpora := []string{"code", "config", "docs"}
	for i, r := range rows {
		if r.Corpus != wantCorpora[i] {
			t.Errorf("rows[%d].Corpus = %q, want %q", i, r.Corpus, wantCorpora[i])
		}
		if r.Ratio != 0 {
			t.Errorf("empty DB %s corpus ratio = %v, want 0", r.Corpus, r.Ratio)
		}
		if r.NeedsRebuild {
			t.Errorf("empty DB %s corpus must not flag NeedsRebuild", r.Corpus)
		}
	}
}

func TestFTS5Fragmentation_PostRebuild_HealthyRatio_1612(t *testing.T) {
	store := newTestStore(t)
	// Seed a project + a small symbol batch so each corpus has rows
	// to index. Per ClassifyCorpus the symbol's (language, kind)
	// pair determines its corpus assignment — Go/Function is "code",
	// YAML/Setting is "config", Markdown/Section is "docs".
	projectID := "p-frag-test"
	if err := store.UpsertProject(Project{
		ID: projectID, Path: t.TempDir(), Name: "frag-test",
	}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	syms := []Symbol{
		// code corpus
		{ID: projectID + "::a.go::pkg.A#Function", ProjectID: projectID, FilePath: "a.go", Name: "A", QualifiedName: "pkg.A", Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		{ID: projectID + "::b.go::pkg.B#Function", ProjectID: projectID, FilePath: "b.go", Name: "B", QualifiedName: "pkg.B", Kind: "Function", Language: "Go", ExtractionConfidence: 1.0},
		// config corpus
		{ID: projectID + "::c.yaml::cfg.x#Setting", ProjectID: projectID, FilePath: "c.yaml", Name: "x", QualifiedName: "cfg.x", Kind: "Setting", Language: "YAML", ExtractionConfidence: 1.0},
		// docs corpus
		{ID: projectID + "::d.md::intro#Section", ProjectID: projectID, FilePath: "d.md", Name: "intro", QualifiedName: "intro", Kind: "Section", Language: "Markdown", ExtractionConfidence: 1.0},
	}
	if err := store.BulkUpsertSymbols(syms); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	if _, err := store.RebuildFTS(); err != nil {
		t.Fatalf("RebuildFTS: %v", err)
	}
	rows, err := store.FTS5Fragmentation()
	if err != nil {
		t.Fatalf("FTS5Fragmentation: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 corpora; got %d", len(rows))
	}
	for _, r := range rows {
		// Post-rebuild ratios must NEVER cross the advisory threshold.
		// If they do on a 4-symbol seed, the threshold is wrong or
		// the rebuild is broken — both are regressions worth catching.
		if r.NeedsRebuild {
			t.Errorf("post-rebuild %s corpus flagged NeedsRebuild (ratio=%v, idx=%d, data=%d) — threshold (%v) too tight, or rebuild broken",
				r.Corpus, r.Ratio, r.IdxRows, r.DataRows, FTS5FragmentationThreshold)
		}
	}
}
