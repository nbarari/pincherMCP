package index

import (
	"context"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1481 — Markdown REFERENCES edges are emitted by the extractor (per
// internal/ast/markdown.go:240-277) but reported as zero in production
// despite real corpora with hundreds of internal links. This probe
// exercises the full pipeline (extract → resolve → persist) to lock
// down whether the link-walker reaches the DB.

const markdownIntraDocLinks = `# Project

Welcome.

## Installation

See [Configuration](#configuration) for the next step.

## Configuration

After [Installation](#installation), set up env vars.

## Usage

Refer back to [Configuration](#configuration) when troubleshooting.
`

func TestIndex_MarkdownIntraDocREFERENCES_PersistedToDB_1481(t *testing.T) {
	// Positive shape. Three sections with cross-references via
	// [text](#anchor) intra-doc links. Should emit REFERENCES
	// edges (Installation → Configuration, Configuration → Installation,
	// Usage → Configuration).
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "README.md", markdownIntraDocLinks)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	projectID := db.ProjectIDFromPath(dir)

	// Find the Configuration Section. Section QNs root on the H1
	// heading slug (here: "project" from "# Project"), NOT the
	// filename — so the QN is "project.configuration".
	syms, err := store.GetSymbolsByQN(projectID, "project.configuration")
	if err != nil || len(syms) == 0 {
		t.Fatalf("expected project.configuration Section symbol; got %d syms err=%v", len(syms), err)
	}
	configID := syms[0].ID

	edges, err := store.EdgesTo(configID, []string{"REFERENCES"})
	if err != nil {
		t.Fatalf("EdgesTo: %v", err)
	}
	if len(edges) == 0 {
		t.Errorf("expected ≥1 REFERENCES edge into the Configuration section (Installation links to it via [Configuration](#configuration)); got 0. #1481 reproduces.")
	}
}
