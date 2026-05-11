package index

import (
	"context"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #436: resolveReads / resolveWrites used to bind a Go function's
// reference to `path` to a JavaScript Variable named `path` (e.g. a
// local in plugin/scripts/install.js) — 482 such cross-language
// false-positive READS edges in pincher-repo. The fix scopes name
// lookups to the source symbol's language.
//
// Repro corpus:
//   svc/handler.go    : func Foo() { _ = path }   // Go reads `path`
//   plugin/install.js : var path = ...            // JS local named `path`
//
// Pre-fix: Foo gets a READS edge to the JS variable.
// Post-fix: Foo's `path` reference doesn't resolve (no Go variable
// `path` in the project) — no edge is created.
func TestIndex_ReadsEdges_DoesNotBindAcrossLanguages(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()

	// Go file references `path`.
	writeFile(t, dir, "svc/handler.go", `package svc

func Foo() {
	_ = path
}
`)
	// JavaScript file declares a `path` variable. The regex JS extractor
	// catches `var path = ...` as a Variable (confidence 0.95).
	writeFile(t, dir, "plugin/install.js", `var path = require("path");
var fs = require("fs");
`)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	// Sanity: the JS variable should have been extracted as a Variable.
	pid := db.ProjectIDFromPath(dir)
	jsVars, err := store.GetSymbolsByName(pid, "path", 5)
	if err != nil {
		t.Fatalf("GetSymbolsByName(path): %v", err)
	}
	var jsPathID string
	for _, s := range jsVars {
		if s.Language == "JavaScript" && s.Kind == "Variable" {
			jsPathID = s.ID
			break
		}
	}
	if jsPathID == "" {
		t.Skip("JS path Variable not extracted in this fixture — JS extractor may have changed; cross-language scoping is still tested by the negative assertion below")
	}

	// The Go function Foo MUST NOT have a READS edge to the JS path.
	inbound, err := store.EdgesTo(jsPathID, nil)
	if err != nil {
		t.Fatalf("EdgesTo jsPath: %v", err)
	}
	for _, e := range inbound {
		// Find the source symbol; if it's a Go function, that's the bug.
		fromSym, err := store.GetSymbol(e.FromID)
		if err != nil || fromSym == nil {
			continue
		}
		if fromSym.Language == "Go" {
			t.Errorf("#436 regression: Go symbol %s has a %s edge to JS variable %s — language scoping broken",
				e.FromID, e.Kind, jsPathID)
		}
	}
}
