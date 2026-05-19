package server

import (
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1612 v0.87: FTS5 fragmentation advisory. The advisory's input is a
// per-corpus fragmentation table (data/idx ratio) computed by
// Store.FTS5Fragmentation. These tests pin the threshold logic + the
// message shape so a regression in either side surfaces immediately.

func TestFTS5FragmentationAdvisory_AllHealthy_NoAdvisory_1612(t *testing.T) {
	t.Parallel()
	// Healthy ratios sit at 1–3x for code/docs and up to 10–11x for
	// config corpus on a multi-project install post-rebuild (#1663).
	// All in the healthy band must produce no advisory.
	rows := []db.FTS5CorpusFragmentation{
		{Corpus: "code", IdxRows: 4460, DataRows: 9039, Ratio: 2.03, NeedsRebuild: false},
		{Corpus: "config", IdxRows: 1466, DataRows: 15293, Ratio: 10.43, NeedsRebuild: false}, // observed post-rebuild floor
		{Corpus: "docs", IdxRows: 1192, DataRows: 1356, Ratio: 1.14, NeedsRebuild: false},
	}
	if got := fts5FragmentationAdvisory(rows); got != "" {
		t.Errorf("expected empty advisory on healthy corpora; got: %s", got)
	}
}

func TestFTS5FragmentationAdvisory_SingleFraggedCorpus_1612(t *testing.T) {
	t.Parallel()
	// The config corpus on a long-running install: 62.8x ratio. Real
	// observed value from pincher dogfood machine 2026-05-19.
	rows := []db.FTS5CorpusFragmentation{
		{Corpus: "code", IdxRows: 4460, DataRows: 9039, Ratio: 2.03, NeedsRebuild: false},
		{Corpus: "config", IdxRows: 7215, DataRows: 453285, Ratio: 62.8, NeedsRebuild: true},
		{Corpus: "docs", IdxRows: 1192, DataRows: 1356, Ratio: 1.14, NeedsRebuild: false},
	}
	got := fts5FragmentationAdvisory(rows)
	if got == "" {
		t.Fatal("expected advisory when one corpus exceeds threshold; got empty")
	}
	// Must name the specific bad corpus and its ratio.
	if !strings.Contains(got, "config corpus is 62.8x fragmented") {
		t.Errorf("advisory missing concrete corpus name + ratio: %s", got)
	}
	if !strings.Contains(got, "453285 data rows / 7215 index rows") {
		t.Errorf("advisory missing concrete row counts: %s", got)
	}
	// Must NOT mention healthy corpora — keeps the advisory actionable.
	if strings.Contains(got, "code corpus") || strings.Contains(got, "docs corpus") {
		t.Errorf("advisory leaked healthy-corpus references: %s", got)
	}
	// Must carry the remediation.
	if !strings.Contains(got, "rebuild_fts") {
		t.Errorf("advisory missing remediation: %s", got)
	}
	// Must reference the issue for traceability.
	if !strings.Contains(got, "#1612") {
		t.Errorf("advisory missing issue ref: %s", got)
	}
}

func TestFTS5FragmentationAdvisory_AllFragged_ListsAll_1612(t *testing.T) {
	t.Parallel()
	rows := []db.FTS5CorpusFragmentation{
		{Corpus: "code", IdxRows: 100, DataRows: 1500, Ratio: 15.0, NeedsRebuild: true},
		{Corpus: "config", IdxRows: 200, DataRows: 6000, Ratio: 30.0, NeedsRebuild: true},
		{Corpus: "docs", IdxRows: 50, DataRows: 600, Ratio: 12.0, NeedsRebuild: true},
	}
	got := fts5FragmentationAdvisory(rows)
	for _, want := range []string{"code corpus is 15.0x", "config corpus is 30.0x", "docs corpus is 12.0x"} {
		if !strings.Contains(got, want) {
			t.Errorf("advisory missing %q for all-fragged case: %s", want, got)
		}
	}
	// Items should be joined with semicolons so the reader can scan
	// them as a list.
	if !strings.Contains(got, "; ") {
		t.Errorf("advisory should join multi-corpus phrases with '; ': %s", got)
	}
}

func TestFTS5FragmentationAdvisory_EmptyInput_NoAdvisory_1612(t *testing.T) {
	t.Parallel()
	// Empty input = no FTS5 vtabs (pre-v9 schema or no rows yet).
	if got := fts5FragmentationAdvisory(nil); got != "" {
		t.Errorf("expected empty advisory on nil input; got: %s", got)
	}
	if got := fts5FragmentationAdvisory([]db.FTS5CorpusFragmentation{}); got != "" {
		t.Errorf("expected empty advisory on empty slice; got: %s", got)
	}
}
