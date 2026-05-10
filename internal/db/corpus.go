package db

import "fmt"

// Corpus is the FTS5-routing label assigned to every symbol. The per-corpus
// FTS5 split (#32) keeps the same search UX but routes BM25 ranking through
// three independent indexes so identifier-shaped queries don't get diluted
// by configuration / documentation rows.
//
// One corpus per symbol — symbols never appear in two FTS5 indexes.
const (
	CorpusCode   = "code"   // source code identifiers (Function, Method, Class, etc.)
	CorpusConfig = "config" // YAML/JSON/HCL Settings, Resources, Variables, Outputs
	CorpusDocs   = "docs"   // Markdown sections, fetched URL Documents
)

// ClassifyCorpus returns the corpus name for a (language, kind) tuple. The
// rules:
//
//  1. Document kind always routes to docs (`fetch` tool stores remote URL
//     content with a `Document` kind regardless of detected language).
//  2. Markdown language → docs.
//  3. YAML / JSON / HCL / TOML / XML → config.
//  4. Everything else (Go, Python, JS, TS, Rust, Java, Ruby, PHP, C, C++,
//     C#, Kotlin, Swift, Bash, JSX, TSX) → code.
//
// **PARITY WITH SQL:** The v9 schema migration encodes the same routing
// in three FTS5 sync triggers (sym_fts_corpus_insert/delete/update). The
// language lists in this function and in the SQL must match. The
// TestClassifyCorpus_MatchesSQLTriggerRouting parity test inserts one
// symbol per registered language and asserts the routing observed in
// SQL matches what this function says — adding a new language without
// updating both sides fails CI loudly.
//
// Returns CorpusCode for unknown languages so a missing classifier rule
// can never silently drop a symbol from search; the parity test catches
// the omission separately.
func ClassifyCorpus(language, kind string) string {
	if kind == "Document" {
		return CorpusDocs
	}
	switch language {
	case "Markdown", "HTML":
		return CorpusDocs
	case "YAML", "JSON", "HCL", "TOML", "XML":
		return CorpusConfig
	default:
		return CorpusCode
	}
}

// corpusVtab maps a corpus parameter (the user-facing label) to the SQL
// vtab name used in queries.
//
// **Default flipped in #32 part 3**: empty string routes to
// `symbols_code_fts` (the code-corpus index). Pincher is a
// code-intelligence tool; the most common `search` call is for an
// identifier and code is the right default. Pass `corpus=config` or
// `corpus=docs` to query those slices.
//
// **Legacy `corpus=all` removed in #106 (v12 schema migration)**. The
// MCP search handler still soft-redirects `corpus=all` to `corpus=code`
// (with a deprecation warning) so older callers keep working; this
// function never sees the literal "all" via that path. A direct caller
// passing `corpus="all"` here gets the same unknown-corpus error any
// other typo would produce.
//
// Returns an error on unknown corpus names — a typo from a caller
// should fail loudly, not silently fall through.
func corpusVtab(corpus string) (string, error) {
	switch corpus {
	case "", CorpusCode:
		return "symbols_code_fts", nil
	case CorpusConfig:
		return "symbols_config_fts", nil
	case CorpusDocs:
		return "symbols_docs_fts", nil
	default:
		return "", fmt.Errorf("unknown corpus %q (valid: %q, %q, %q, %q)",
			corpus, "", CorpusCode, CorpusConfig, CorpusDocs)
	}
}
