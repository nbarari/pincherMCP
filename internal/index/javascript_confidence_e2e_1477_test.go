package index

import (
	"context"
	"testing"
)

// #1477: JavaScript AST symbols must arrive at confidence >= 0.99
// after the FULL indexer pipeline — not just in the isolated AST
// unit test (internal/ast/javascript_ast_confidence_test.go).
//
// The bug as filed: AST-extracted JS symbols stamped at the
// regex-tier floor (0.95 / 0.975) in production even though the
// unit test passed. The unit test exercises extractJavaScriptAST's
// FileResult.ConfidenceOverride in isolation; it does NOT prove the
// indexer pipeline (langAdapter → Symbol struct → BulkUpsertSymbols
// → DB) preserves the override end-to-end. This test closes that
// gap: index a real .js file through Index() and assert the
// persisted confidence.
//
// The bug was fixed somewhere between v0.73 (filed) and v0.87
// (verified fixed via a cross-project query: all normal JS symbols
// at 1.0). This test is the regression guard so it can't silently
// come back.

func TestIndex_JavaScriptAST_ConfidenceEndToEnd_1477(t *testing.T) {
	t.Parallel()
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	// A plain ES6+ module — class, method, function, arrow const.
	// No JSX, no decorators: the AST extractor must parse this
	// trivially and stamp every symbol at 1.0.
	writeFile(t, dir, "mod.js", `
class Greeter {
  constructor(name) { this.name = name; }
  greet() { return "hi " + this.name; }
}

function makeGreeter(name) {
  return new Greeter(name);
}

const shout = (s) => s.toUpperCase();

export { Greeter, makeGreeter, shout };
`)

	summary, err := idx.Index(context.Background(), dir, true)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	projectID := summary.ProjectID

	syms, err := store.GetSymbolsForFile(projectID, "mod.js")
	if err != nil {
		t.Fatalf("GetSymbolsForFile: %v", err)
	}
	if len(syms) == 0 {
		t.Fatal("no symbols extracted from mod.js — AST extractor may have silently failed")
	}

	jsCount := 0
	for _, s := range syms {
		if s.Language != "JavaScript" {
			continue
		}
		jsCount++
		// >= 0.99 is the AST-tier contract. Regex-tier floors sit at
		// 0.95 / 0.975 — anything below 0.99 means the indexer
		// pipeline dropped the ConfidenceOverride (regression of
		// #1477) or the AST path silently fell back to regex.
		if s.ExtractionConfidence < 0.99 {
			t.Errorf("symbol %q (%s): confidence = %v, want >= 0.99 — "+
				"AST override not preserved through the indexer pipeline (#1477)",
				s.Name, s.Kind, s.ExtractionConfidence)
		}
	}
	if jsCount == 0 {
		t.Fatal("no symbols tagged Language=JavaScript — language detection misrouted mod.js")
	}
}

// Sibling: the .mjs extension must route to the JS AST extractor
// too. The #1477 report specifically flagged .mjs files
// (WebDriverAgent/Scripts/fetch-prebuilt-wda.mjs) as a candidate
// misroute — language detection that recognised .js but not .mjs
// would silently regex-extract every ES-module script.
func TestIndex_JavaScriptAST_MjsExtension_ConfidenceEndToEnd_1477(t *testing.T) {
	t.Parallel()
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	writeFile(t, dir, "script.mjs", `
import { readFile } from "node:fs/promises";

async function loadConfig(path) {
  const raw = await readFile(path, "utf8");
  return JSON.parse(raw);
}

const DEFAULT_PATH = "./config.json";

export { loadConfig, DEFAULT_PATH };
`)

	summary, err := idx.Index(context.Background(), dir, true)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	syms, err := store.GetSymbolsForFile(summary.ProjectID, "script.mjs")
	if err != nil {
		t.Fatalf("GetSymbolsForFile: %v", err)
	}
	jsCount := 0
	for _, s := range syms {
		if s.Language != "JavaScript" {
			continue
		}
		jsCount++
		if s.ExtractionConfidence < 0.99 {
			t.Errorf("symbol %q (%s) in .mjs: confidence = %v, want >= 0.99 (#1477)",
				s.Name, s.Kind, s.ExtractionConfidence)
		}
	}
	if jsCount == 0 {
		t.Fatal("no JavaScript symbols from script.mjs — .mjs not routed to the JS AST extractor")
	}
}
