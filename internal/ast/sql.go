package ast

import (
	"bytes"
	"regexp"
	"strings"
)

// extractSQL is a regex-tier extractor (confidence 0.85) for the
// symbol-extraction-relevant subset of SQL DDL. Captures CREATE
// statements that introduce a named, callable / queryable entity:
//
//   - CREATE TABLE [IF NOT EXISTS] [schema.]name      → Class
//   - CREATE [OR REPLACE] VIEW [schema.]name          → Class
//     (views are query-shaped tables; same kind for graph parity)
//   - CREATE [OR REPLACE] FUNCTION [schema.]name      → Function
//   - CREATE [OR REPLACE] PROCEDURE [schema.]name     → Function
//   - CREATE [OR REPLACE] TRIGGER [schema.]name       → Function
//
// Skipped (deliberately out of scope):
//
//   - DML (INSERT / UPDATE / DELETE / SELECT) — not symbol-shaped
//   - ALTER TABLE / DROP — modifies existing symbols, would need
//     reference-edge tracking the regex tier can't deliver
//   - CREATE INDEX — structural plumbing, low signal as a symbol
//   - View columns — needs column-level resolution, parser-tier work
//
// Why regex over AST: no pure-Go multi-dialect SQL parser exists.
// MySQL-only and Postgres-only AST libraries either use CGO or
// miss the other dialect's syntax. Regex catches the high-frequency
// definitions across all dialects (MySQL / Postgres / SQLite / MSSQL
// / Oracle) at the cost of missing complex / nested cases. For
// symbol extraction at the (table-name, function-name) granularity,
// that's the right trade. Closes #102.
//
// Case-insensitivity: SQL keywords are conventionally uppercase but
// not required — `create table` and `CREATE TABLE` both parse in
// every dialect. The patterns use `(?i)` for case-insensitive
// matching across the keyword run while keeping the captured name
// case-sensitive (database identifiers are case-sensitive in most
// dialects when quoted, and even unquoted MySQL identifiers preserve
// case for storage).
//
// Schema prefix: `[schema.]name` is captured as a single
// `schema.name` qualified name. The `name` field strips the schema
// prefix; the `qualified_name` carries the full dotted form. This
// matches how pincher emits Setting symbols for YAML — dotted-path
// QN with bare `name` for leaf-name search.

// sqlTableRE matches CREATE TABLE definitions. Supports:
//   - Optional `OR REPLACE` (some dialects)
//   - Optional `IF NOT EXISTS`
//   - Optional `TEMPORARY` / `TEMP` / `GLOBAL TEMPORARY` / `UNLOGGED`
//   - Optional schema prefix
//   - Backtick / double-quote / square-bracket quoting around the name
var sqlTableRE = regexp.MustCompile(
	`(?i)(?m)^[ \t]*CREATE(?:\s+(?:OR\s+REPLACE|TEMPORARY|TEMP|GLOBAL\s+TEMPORARY|UNLOGGED))*\s+TABLE` +
		`(?:\s+IF\s+NOT\s+EXISTS)?\s+` +
		`(?P<name>[` + "`" + `"\[]?[\w.]+[` + "`" + `"\]]?)`)

// sqlViewRE matches CREATE VIEW / CREATE OR REPLACE VIEW. Same
// dialect-agnostic shape as TABLE.
var sqlViewRE = regexp.MustCompile(
	`(?i)(?m)^[ \t]*CREATE(?:\s+(?:OR\s+REPLACE|TEMPORARY|TEMP|MATERIALIZED|RECURSIVE))*\s+VIEW` +
		`(?:\s+IF\s+NOT\s+EXISTS)?\s+` +
		`(?P<name>[` + "`" + `"\[]?[\w.]+[` + "`" + `"\]]?)`)

// sqlFunctionRE matches CREATE FUNCTION. Postgres allows arg lists
// on the same line; the captured `name` stops at whitespace or the
// opening paren. `IF NOT EXISTS` is supported by MariaDB.
var sqlFunctionRE = regexp.MustCompile(
	`(?i)(?m)^[ \t]*CREATE(?:\s+OR\s+REPLACE)?\s+FUNCTION` +
		`(?:\s+IF\s+NOT\s+EXISTS)?\s+` +
		`(?P<name>[` + "`" + `"\[]?[\w.]+[` + "`" + `"\]]?)`)

// sqlProcedureRE matches CREATE PROCEDURE. `IF NOT EXISTS` works in
// MariaDB.
var sqlProcedureRE = regexp.MustCompile(
	`(?i)(?m)^[ \t]*CREATE(?:\s+OR\s+REPLACE)?\s+PROCEDURE` +
		`(?:\s+IF\s+NOT\s+EXISTS)?\s+` +
		`(?P<name>[` + "`" + `"\[]?[\w.]+[` + "`" + `"\]]?)`)

// sqlTriggerRE matches CREATE TRIGGER. The trigger body is multi-line
// but only the trigger NAME is captured here. `IF NOT EXISTS` is
// supported by SQLite and MariaDB.
var sqlTriggerRE = regexp.MustCompile(
	`(?i)(?m)^[ \t]*CREATE(?:\s+OR\s+REPLACE)?\s+TRIGGER` +
		`(?:\s+IF\s+NOT\s+EXISTS)?\s+` +
		`(?P<name>[` + "`" + `"\[]?[\w.]+[` + "`" + `"\]]?)`)

// sqlExtractorPattern bundles a regex with the symbol kind to emit.
// Each pattern is tried independently; the same byte position can
// only produce one symbol because the patterns require distinct
// keyword sequences (TABLE vs VIEW vs FUNCTION vs PROCEDURE vs
// TRIGGER) — they can't double-fire on the same line.
type sqlExtractorPattern struct {
	re   *regexp.Regexp
	kind string
}

func extractSQL(source []byte, relPath string) *FileResult {
	out := &FileResult{}

	// Strip block comments and line comments so that
	//   -- CREATE TABLE foo
	//   /* CREATE FUNCTION bar */
	// don't produce phantom symbols. Indices into the resulting
	// stripped buffer would no longer line up with the original
	// source's byte offsets, so we operate on the original source
	// and skip matches whose start byte falls inside a comment span.
	commentSpans := sqlCommentSpans(source)

	patterns := []sqlExtractorPattern{
		{sqlTableRE, "Class"},
		{sqlViewRE, "Class"},
		{sqlFunctionRE, "Function"},
		{sqlProcedureRE, "Function"},
		{sqlTriggerRE, "Function"},
	}

	for _, p := range patterns {
		for _, m := range p.re.FindAllSubmatchIndex(source, -1) {
			nameStart, nameEnd := m[2], m[3]
			rawName := string(source[nameStart:nameEnd])
			name := stripSQLQuotes(rawName)
			if name == "" {
				continue
			}
			// Skip matches inside comments.
			if inAnySpan(nameStart, commentSpans) {
				continue
			}
			// Qualified name preserves the schema prefix; the bare
			// `name` is the trailing identifier (after the last dot).
			qn := name
			if dot := strings.LastIndex(name, "."); dot >= 0 {
				name = name[dot+1:]
			}
			lineStart := bytes.LastIndexByte(source[:nameStart], '\n') + 1
			lineEnd := bytes.IndexByte(source[nameStart:], '\n')
			if lineEnd < 0 {
				lineEnd = len(source) - nameStart
			}
			startByte := lineStart
			endByte := nameStart + lineEnd
			startLine := bytes.Count(source[:startByte], []byte("\n")) + 1
			endLine := bytes.Count(source[:endByte], []byte("\n")) + 1
			out.Symbols = append(out.Symbols, ExtractedSymbol{
				Name:          name,
				QualifiedName: qn,
				Kind:          p.kind,
				StartByte:     startByte,
				EndByte:       endByte,
				StartLine:     startLine,
				EndLine:       endLine,
				IsExported:    true, // SQL CREATE statements are always public-by-definition
			})
		}
	}

	return out
}

// sqlCommentSpans returns the byte ranges occupied by comments in src.
// Both `--` line comments and `/* ... */` block comments are tracked
// so the extractor can skip CREATE statements that appear inside
// commented-out SQL (common in migration files where prior versions
// of a definition are kept as documentation).
func sqlCommentSpans(src []byte) [][2]int {
	var spans [][2]int
	i := 0
	for i < len(src) {
		// Skip strings — quoted strings can contain '--' or '/*'
		// without those being comments. Single-quoted is the SQL
		// standard; double-quoted is identifier-quoting in most
		// dialects so we don't strip those.
		if src[i] == '\'' {
			j := i + 1
			for j < len(src) && src[j] != '\'' {
				if src[j] == '\\' && j+1 < len(src) {
					j += 2
					continue
				}
				j++
			}
			if j < len(src) {
				j++ // consume closing quote
			}
			i = j
			continue
		}
		if i+1 < len(src) && src[i] == '-' && src[i+1] == '-' {
			start := i
			for i < len(src) && src[i] != '\n' {
				i++
			}
			spans = append(spans, [2]int{start, i})
			continue
		}
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
			start := i
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			if i+1 < len(src) {
				i += 2
			}
			spans = append(spans, [2]int{start, i})
			continue
		}
		i++
	}
	return spans
}

// inAnySpan reports whether pos lies inside any of the given byte
// ranges. Linear scan; spans are typically <100 per file so this is
// fine. Caller passes spans in source order but inAnySpan doesn't
// require it.
func inAnySpan(pos int, spans [][2]int) bool {
	for _, s := range spans {
		if pos >= s[0] && pos < s[1] {
			return true
		}
	}
	return false
}

// stripSQLQuotes removes the dialect-specific quoting wrappers
// around an SQL identifier:
//   - Backticks (MySQL):              `name`
//   - Double quotes (Postgres/SQLite): "name"
//   - Square brackets (MSSQL):         [name]
//
// Schema-qualified names (`schema.name`) keep the dot; only the
// outermost wrappers are stripped.
func stripSQLQuotes(s string) string {
	if len(s) < 2 {
		return s
	}
	first, last := s[0], s[len(s)-1]
	switch {
	case first == '`' && last == '`':
		return s[1 : len(s)-1]
	case first == '"' && last == '"':
		return s[1 : len(s)-1]
	case first == '[' && last == ']':
		return s[1 : len(s)-1]
	}
	return s
}
