package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1184-adjacent (v0.64): pin the dead_code tool description to its
// actual default behavior. Pre-fix, the description claimed
// `language=Go` was a default — but the server passes empty language
// straight through and relies on min_confidence=0.95 to floor results
// to AST-tier extractors (Go + Python since #862, plus
// JSON/YAML/HCL/TOML parser-backed). Stale text drifts the agent's
// expectations vs reality; the contract test fails loudly the next
// time someone touches the defaults without updating the prose.
//
// Table-from-the-start (#1152): positive (text describes reality),
// negative (text doesn't claim a stale default), control (matching
// default behavior surfaces both Go AND Python dead code), and a
// cross-check that the regex-tier opt-out claim ("drop to 0.0")
// actually surfaces a regex-tier result.

// findDeadCodeTool extracts the dead_code tool's MCP registration so
// the description text can be asserted directly rather than parsed
// out of a regenerated contract snapshot — fewer indirections, fails
// at the spot the drift was introduced.
func findDeadCodeToolDescription(t *testing.T) string {
	t.Helper()
	srv, _, _ := newTestServer(t)
	tool := srv.tools["dead_code"]
	if tool == nil {
		t.Fatal("dead_code tool not registered")
	}
	return tool.Description
}

// findDeadCodeToolSchema returns the dead_code InputSchema as a
// decoded map so per-field "description" text can be asserted.
func findDeadCodeToolSchema(t *testing.T) map[string]any {
	t.Helper()
	srv, _, _ := newTestServer(t)
	tool := srv.tools["dead_code"]
	if tool == nil {
		t.Fatal("dead_code tool not registered")
	}
	raw, ok := tool.InputSchema.(json.RawMessage)
	if !ok {
		t.Fatalf("InputSchema not json.RawMessage: %T", tool.InputSchema)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("InputSchema not valid JSON: %v", err)
	}
	return schema
}

// Positive: description names what defaults actually are.
func TestDeadCodeDescription_NamesActualDefaults(t *testing.T) {
	desc := findDeadCodeToolDescription(t)
	mustContain := []string{
		"min_confidence=0.95", // the actual precision lever
		"Python",              // AST-tier since #862; must be acknowledged
		"Function,Method",     // store-side default when kinds is empty
	}
	for _, want := range mustContain {
		if !strings.Contains(desc, want) {
			t.Errorf("dead_code description missing %q\nGOT:\n%s", want, desc)
		}
	}
}

// Negative: description must NOT make the historically-wrong claim
// that `language=Go` is the default. The server-side default for
// language is empty (no filter); min_confidence is what filters.
func TestDeadCodeDescription_DoesNotClaimStaleLanguageGoDefault(t *testing.T) {
	desc := findDeadCodeToolDescription(t)
	// The exact stale phrasing was "Defaults bias toward precision:
	// `language=Go`". Pin against that fragment plus the broader
	// pattern of asserting Go-only defaults.
	stale := []string{
		"`language=Go`",
		"Defaults bias toward precision: `language=Go`",
	}
	for _, bad := range stale {
		if strings.Contains(desc, bad) {
			t.Errorf("dead_code description still contains stale claim %q\nGOT:\n%s", bad, desc)
		}
	}
	// The per-field schema description for `language` previously
	// said "Recommended: 'Go' until non-Go AST extractors land".
	// Python AST shipped in #862, so this stale recommendation must
	// not appear.
	schema := findDeadCodeToolSchema(t)
	props, _ := schema["properties"].(map[string]any)
	lang, _ := props["language"].(map[string]any)
	langDesc, _ := lang["description"].(string)
	if strings.Contains(langDesc, "until non-Go AST extractors land") {
		t.Errorf("dead_code.language description still claims non-Go AST is pending; #862 (Python AST) shipped\nGOT:\n%s", langDesc)
	}
}

// Control: passing the description's documented default behavior
// (empty language, default kinds, default min_confidence) returns
// dead code in BOTH Go AND Python — proves the "AST-tier extractors
// at 1.0 include Python" claim isn't just prose.
func TestDeadCodeDescription_DefaultsSurfaceBothGoAndPython(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-dead-default"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
	})
	srv.sessionID = pid

	mustUpsertSymbols(t, store, []db.Symbol{
		// Go dead function — AST-tier (1.0), un-exported, no callers.
		{ID: pid + "::pkg.goDead#Function", ProjectID: pid, FilePath: "a.go",
			Name: "goDead", QualifiedName: "pkg.goDead", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		// Python dead function — AST-tier (1.0), un-exported.
		// Python doesn't have Go's exported convention; pincher's
		// extractor marks _underscore-prefix as unexported.
		{ID: pid + "::mod._py_dead#Function", ProjectID: pid, FilePath: "a.py",
			Name: "_py_dead", QualifiedName: "mod._py_dead", Kind: "Function", Language: "Python",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleDeadCode(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleDeadCode: %v", err)
	}
	body := decode(t, result)
	syms, _ := body["dead_symbols"].([]any)
	seenGo, seenPython := false, false
	for _, s := range syms {
		m, _ := s.(map[string]any)
		switch m["language"] {
		case "Go":
			if m["name"] == "goDead" {
				seenGo = true
			}
		case "Python":
			if m["name"] == "_py_dead" {
				seenPython = true
			}
		}
	}
	if !seenGo {
		t.Errorf("Go dead function missing from default dead_code result (description claims AST-tier defaults include Go)")
	}
	if !seenPython {
		t.Errorf("Python dead function missing from default dead_code result (description claims AST-tier defaults include Python since #862)")
	}
}

// Cross-check: the description's opt-out claim ("Drop to 0.0 to
// include regex-tier languages") must actually surface a regex-tier
// result when min_confidence=0.0 is passed. Pins the documented
// escape hatch so future floor-changes don't silently break it.
func TestDeadCodeDescription_MinConfidenceZeroSurfacesRegexTier(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-dead-regex"
	store.UpsertProject(db.Project{
		ID: pid, Path: t.TempDir(), Name: pid, IndexedAt: time.Now(),
	})
	srv.sessionID = pid

	mustUpsertSymbols(t, store, []db.Symbol{
		// Regex-tier confidence (0.70), un-exported, no callers.
		{ID: pid + "::pkg.tsDead#Function", ProjectID: pid, FilePath: "a.ts",
			Name: "tsDead", QualifiedName: "pkg.tsDead", Kind: "Function", Language: "TypeScript",
			ExtractionConfidence: 0.70},
	})

	// Default min_confidence=0.95 — regex-tier symbol should be
	// floored out.
	result, err := srv.handleDeadCode(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleDeadCode (default): %v", err)
	}
	body := decode(t, result)
	syms, _ := body["dead_symbols"].([]any)
	for _, s := range syms {
		m, _ := s.(map[string]any)
		if m["name"] == "tsDead" {
			t.Fatal("regex-tier symbol surfaced at default min_confidence=0.95 — floor not enforced")
		}
	}

	// min_confidence=0.0 — description claims this opt-out
	// surfaces regex-tier results.
	result, err = srv.handleDeadCode(context.Background(), makeReq(map[string]any{
		"min_confidence": float64(0.0),
	}))
	if err != nil {
		t.Fatalf("handleDeadCode (min_confidence=0.0): %v", err)
	}
	body = decode(t, result)
	syms, _ = body["dead_symbols"].([]any)
	seenTs := false
	for _, s := range syms {
		m, _ := s.(map[string]any)
		if m["name"] == "tsDead" {
			seenTs = true
		}
	}
	if !seenTs {
		t.Errorf("description claims min_confidence=0.0 surfaces regex-tier — but tsDead not found")
	}
}
