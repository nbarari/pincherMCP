package ast

import (
	"testing"
)

// #1343 v0.71: Markdown extractor now emits REFERENCES edges for
// inter-doc and intra-doc links — `[text](other.md#section)` and
// `[text](#anchor)`. External URLs (http/https/mailto) and non-docs
// extensions (.png, .pdf) are deliberately skipped — never resolvable.

func TestExtractMarkdown_InterDocLink_EmitsREFERENCES_1343(t *testing.T) {
	src := []byte(`# Overview

Read the [reference](REFERENCE.md#schema-version) for details.
See [the tutorial](docs/tutorials/quickstart.md).
`)
	result := Extract(src, "Markdown", "README.md")
	if result == nil {
		t.Fatal("nil result")
	}
	want := []struct{ fromQN, toName string }{
		{"overview", "REFERENCE.schema_version"},
		{"overview", "quickstart"},
	}
	for _, w := range want {
		var found bool
		for _, e := range result.Edges {
			if e.Kind == "REFERENCES" && e.FromQN == w.fromQN && e.ToName == w.toName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected REFERENCES edge %s → %s; got edges: %+v", w.fromQN, w.toName, result.Edges)
		}
	}
}

func TestExtractMarkdown_IntraDocLink_EmitsREFERENCES_1343(t *testing.T) {
	src := []byte(`# Top

[Back to setup](#setup)

## Setup

Setup instructions.
`)
	result := Extract(src, "Markdown", "guide.md")
	if result == nil {
		t.Fatal("nil result")
	}
	var found bool
	for _, e := range result.Edges {
		if e.Kind == "REFERENCES" && e.FromQN == "top" && e.ToName == "guide.setup" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected intra-doc REFERENCES edge top → guide.setup; got %+v", result.Edges)
	}
}

// External URLs (http/https/mailto/tel/data/javascript) must NOT emit
// REFERENCES — there's no symbol target. Distinct from #1340's deferred
// IMPORTS — Markdown external links are never resolvable.
func TestExtractMarkdown_ExternalLinks_NoEdge_1343(t *testing.T) {
	src := []byte(`# Links

External: [GitHub](https://github.com/kwad77/pincher).
Insecure: [Other](http://example.com).
Mail: [Contact](mailto:foo@example.com).
Tel: [Call](tel:+15551234567).
Protocol-relative: [CDN](//cdn.example/foo.css).
`)
	result := Extract(src, "Markdown", "links.md")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, e := range result.Edges {
		if e.Kind == "REFERENCES" {
			t.Errorf("external/non-symbol link emitted REFERENCES: %+v", e)
		}
	}
}

// Non-docs extensions: links to .png / .pdf / etc. must NOT emit —
// they don't resolve to docs symbols.
func TestExtractMarkdown_NonDocsExtension_NoEdge_1343(t *testing.T) {
	src := []byte(`# Assets

Image: [Diagram](docs/diagram.png).
PDF: [Spec](docs/spec.pdf).
`)
	result := Extract(src, "Markdown", "assets.md")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, e := range result.Edges {
		if e.Kind == "REFERENCES" {
			t.Errorf("non-docs extension emitted REFERENCES: %+v", e)
		}
	}
}

// Cross-check: duplicate link to the same target in the same section
// dedupes to one edge.
func TestExtractMarkdown_DuplicateLink_DedupedToOneEdge_1343(t *testing.T) {
	src := []byte(`# Overview

[See here](REFERENCE.md) and [also here](REFERENCE.md).
`)
	result := Extract(src, "Markdown", "README.md")
	if result == nil {
		t.Fatal("nil result")
	}
	var count int
	for _, e := range result.Edges {
		if e.Kind == "REFERENCES" && e.FromQN == "overview" && e.ToName == "REFERENCE" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 deduped REFERENCES edge; got %d (edges: %+v)", count, result.Edges)
	}
}

// Cross-check: a link before any heading attaches to FromQN="" — the
// indexer treats that as file-scope (same convention as jinja/yaml
// IMPORTS / hcl module IMPORTS).
func TestExtractMarkdown_PreambleLink_FromQNEmpty_1343(t *testing.T) {
	src := []byte(`See [reference](REFERENCE.md) before starting.

# Overview
`)
	result := Extract(src, "Markdown", "README.md")
	if result == nil {
		t.Fatal("nil result")
	}
	var found bool
	for _, e := range result.Edges {
		if e.Kind == "REFERENCES" && e.FromQN == "" && e.ToName == "REFERENCE" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected preamble link with FromQN=\"\"; got %+v", result.Edges)
	}
}

// Self-edges: a link from a section to itself (e.g. `[See](#overview)`
// inside the `## Overview` section) is noise — guard against it.
func TestExtractMarkdown_SelfLink_NoEdge_1343(t *testing.T) {
	src := []byte(`# Overview

[Back to top](#overview)
`)
	result := Extract(src, "Markdown", "self.md")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, e := range result.Edges {
		if e.Kind == "REFERENCES" && e.FromQN == e.ToName {
			t.Errorf("self-edge emitted: %+v", e)
		}
	}
}
