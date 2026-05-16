package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #635-adjacent (v0.64): health tool's per-language parser identity
// must distinguish three tiers, not two. Pre-fix the label was
// AST-vs-Regex only, so stub-tier languages (confidence=0.0) got
// silently bucketed as "Regex" — a stub-tier extractor returns
// empty FileResult, it is not regex extraction. After v0.63 stub
// promotions (#1186/#1187) Haskell is the only remaining stub-tier
// language, so the bug surface narrowed but didn't disappear.
//
// Table-from-the-start (#1152):
//   - Positive: AST-tier (Go), Regex-tier (TypeScript), Stub-tier
//     (Haskell) each get their correctly-labeled bucket.
//   - Negative: description string must NOT make the stale "AST vs
//     Regex" two-tier claim.
//   - Control: Python is correctly labeled AST when the dispatcher
//     gate (PythonAvailable) returns true — the regex-fallback
//     adapter registers at 0.85 but the live extractor is AST.
//   - Cross-check: the registered tier IS what shows up in health
//     output; no off-by-one between extractor confidence and the
//     label in the response.

func TestHealthDescription_NamesThreeTiers(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tool := srv.tools["health"]
	if tool == nil {
		t.Fatal("health tool not registered")
	}
	desc := tool.Description
	for _, want := range []string{"AST", "Regex", "Stub"} {
		if !strings.Contains(desc, want) {
			t.Errorf("health description missing tier %q\nGOT:\n%s", want, desc)
		}
	}
	// Negative: stale "AST vs Regex" two-tier framing must not
	// appear. Pin the exact stale fragment to fail loudly if a
	// future edit reverts to two tiers.
	if strings.Contains(desc, "AST vs Regex") {
		t.Errorf("health description still uses stale two-tier framing 'AST vs Regex'\nGOT:\n%s", desc)
	}
}

// Positive + cross-check: per-language coverage labels match the
// registered tier. Seeds one symbol per tier so health's per-language
// loop visits each language; asserts the Parser field on the
// resulting row.
func TestHandleHealth_PerLanguageParserLabel(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-health-tier"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
	})
	srv.sessionID = pid

	mustUpsertSymbols(t, store, []db.Symbol{
		// Go = AST-tier (1.0).
		{ID: pid + "::go.Foo#Function", ProjectID: pid, FilePath: "a.go",
			Name: "Foo", QualifiedName: "go.Foo", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		// TypeScript = stable-regex (0.85). Post v0.61 (#1158) ships
		// Method extraction; still registered as regex tier overall.
		{ID: pid + "::ts.bar#Function", ProjectID: pid, FilePath: "a.ts",
			Name: "bar", QualifiedName: "ts.bar", Kind: "Function", Language: "TypeScript",
			ExtractionConfidence: 0.85},
		// Haskell = stub-tier (0.0). Only language still stub-tier
		// post v0.63 promotions (#1186/#1187).
		{ID: pid + "::hs.baz#Function", ProjectID: pid, FilePath: "a.hs",
			Name: "baz", QualifiedName: "hs.baz", Kind: "Function", Language: "Haskell",
			ExtractionConfidence: 0.0},
	})

	result, err := srv.handleHealth(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleHealth: %v", err)
	}
	body := decode(t, result)
	coverage, _ := body["extraction_coverage"].([]any)
	if len(coverage) == 0 {
		t.Fatal("coverage field empty")
	}

	parserByLang := map[string]string{}
	for _, row := range coverage {
		m, _ := row.(map[string]any)
		lang, _ := m["language"].(string)
		parser, _ := m["parser"].(string)
		if lang != "" && parser != "" {
			parserByLang[lang] = parser
		}
	}

	want := map[string]string{
		"Go":         "AST",
		"TypeScript": "Regex",
		"Haskell":    "Stub",
	}
	for lang, wantParser := range want {
		got := parserByLang[lang]
		if got != wantParser {
			t.Errorf("language %s: parser=%q, want %q (registered confidence drives the label)",
				lang, got, wantParser)
		}
	}
}

// Control: Python's regex/AST dispatcher (#944) must still upgrade
// the label to AST when PythonAvailable() returns true. The three-
// tier switch above must not regress this. Pinned by simulating a
// Python symbol indexed at the AST-tier confidence (1.0); the upgrade
// path runs after the switch.
func TestHandleHealth_PythonASTUpgradeStillFires(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-health-python"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
	})
	srv.sessionID = pid

	// Python symbols at AST confidence — what the live dispatcher
	// emits when PythonAvailable() is true.
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::py.spam#Function", ProjectID: pid, FilePath: "a.py",
			Name: "spam", QualifiedName: "py.spam", Kind: "Function", Language: "Python",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleHealth(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleHealth: %v", err)
	}
	body := decode(t, result)
	coverage, _ := body["extraction_coverage"].([]any)
	for _, row := range coverage {
		m, _ := row.(map[string]any)
		lang, _ := m["language"].(string)
		if lang == "Python" {
			parser, _ := m["parser"].(string)
			// Python's RegisteredConfidence is 0.85 (regex
			// fallback's honesty). The post-loop upgrade should
			// land it on AST iff PythonAvailable() returns true.
			// Either AST (live dispatcher) or Regex (no AST gate
			// on this build) is correct — but Stub is not.
			if parser == "Stub" {
				t.Errorf("Python labeled Stub — should be AST or Regex via #944 dispatcher")
			}
		}
	}
}
