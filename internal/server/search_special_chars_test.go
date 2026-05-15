package server

import (
	"context"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #289: FTS5 raises "syntax error near '.'" when the query is a bare
// dotted identifier. The sanitizer auto-quotes the dotted token so
// `search query="os.Stat"` just works without the caller learning
// FTS5 phrase syntax.
func TestSanitizeFTS5Query_DottedIdentifier(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		// Bare dotted identifiers get wrapped.
		{"os.Stat", `"os.Stat"`},
		{"fmt.Errorf", `"fmt.Errorf"`},
		{"pkg.sub.Func", `"pkg.sub.Func"`},

		// Bare hyphenated identifiers get wrapped (FTS5 treats `-` as NOT).
		{"my-component", `"my-component"`},
		{"a-b", `"a-b"`},

		// Prefix wildcard preserved across the quoting.
		{"os.Stat*", `"os.Stat"*`},
		{"my-comp*", `"my-comp"*`},

		// Multiple tokens — each evaluated independently.
		{"os.Stat foo.Bar", `"os.Stat" "foo.Bar"`},
		{"plain os.Stat", `plain "os.Stat"`},

		// Already-quoted: pass through unchanged. The user knows what they're doing.
		{`"os.Stat"`, `"os.Stat"`},
		{`"login flow"`, `"login flow"`},

		// #887: CamelCase identifiers phrase-wrap so multi-token OR
		// queries actually return rows. Semantically identical to the
		// unwrapped form for single-word queries (one-token phrase),
		// but works around an FTS5 quirk where `handleSearch OR
		// handleQuery` returns 0 vs `"handleSearch" OR "handleQuery"`
		// returning both.
		{"flushBuffers", `"flushBuffers"`},
		{"auth*", "auth*"},

		// #424: bare uppercase boolean operators in a multi-token query
		// trigger the FTS5 operator parser and crash on malformed
		// expressions. The sanitizer phrase-wraps the prose case
		// (lowercase identifier-words around the operator) so it
		// searches as literal text.
		{"foo OR bar", `"foo OR bar"`},
		{"NOT foo", `"NOT foo"`},
		{"handle AND NOT context", `"handle AND NOT context"`},
		{"NOT", "NOT"},
		// Lowercase booleans aren't FTS5 operators — pass through.
		{"foo or bar", "foo or bar"},
		{"foo and bar", "foo and bar"},

		// #452: deliberate FTS5 expressions — short query with at least
		// one CamelCase / punctuation / wildcard non-operator token —
		// pass through with operator semantics preserved. CamelCase
		// non-operator tokens phrase-wrap per #887 so FTS5's OR
		// actually finds them; pure-lowercase tokens stay bare.
		{"Watch OR poll", `"Watch" OR poll`},
		{"Foo AND Bar", `"Foo" AND "Bar"`},
		{"Foo AND NOT Bar", `"Foo" AND NOT "Bar"`},
		{"auth* OR oauth*", "auth* OR oauth*"},
		// Code-shape tokens with punctuation pass through too.
		{"os.Stat OR fmt.Errorf", `"os.Stat" OR "fmt.Errorf"`},
		{"my-mod OR your-mod", `"my-mod" OR "your-mod"`},

		// #424: parens, slash, at-sign, brackets, braces, comma, !, ?
		// — these all crash bare in FTS5; phrase-wrap them.
		{"parse(query)", `"parse(query)"`},
		{"http.Get(", `"http.Get("`},
		{"json.Marshal(rows)", `"json.Marshal(rows)"`},
		{"notifications/tools/list_changed", `"notifications/tools/list_changed"`},
		{"pkg/sub", `"pkg/sub"`},
		{"@deprecated", `"@deprecated"`},
		{"@Component", `"@Component"`},
		{"foo,bar", `"foo,bar"`},
		{"arr[0]", `"arr[0]"`},
		{"map{k}", `"map{k}"`},
		{"foo!", `"foo!"`},
		{"foo?", `"foo?"`},

		// #424: apostrophe inside a token used to crash with
		// "unterminated string". Now it's stripped from the wrapped span.
		{"don't", `"dont"`},
		{"user's input", `"users" input`},

		// #356: colon-separated identifiers get wrapped — FTS5 treats
		// `colname:term` as a column-prefix lookup, but the FTS5 vtab
		// only indexes name/qualified_name/signature/docstring; user
		// queries with `:` are almost always paths (`localhost:8080`,
		// `mod:fn`) or YAML key chains, not column lookups. The
		// `kind=`/`language=` PARAMETERS are the right way to filter
		// — auto-quoting frees `:` for literal use.
		{"kind:Function", `"kind:Function"`},
		{"localhost:8080", `"localhost:8080"`},
		{"a:b", `"a:b"`},
		{"a:b:c", `"a:b:c"`},

		// Edge cases: leading/trailing dot or hyphen don't trigger wrap
		// (a token like `.foo` or `-foo` isn't a normal identifier; if it's
		// a real query it almost certainly came from FTS5 syntax).
		{".foo", ".foo"},
		{"-foo", "-foo"},
		{"foo.", "foo."},
		{"foo-", "foo-"},

		// Empty query passes through.
		{"", ""},

		// Whitespace-only stays effectively empty.
		{"   ", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := sanitizeFTS5Query(c.in)
			if got != c.want {
				t.Errorf("sanitizeFTS5Query(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// End-to-end: an unsanitized FTS5 search on `os.Stat` would error
// with "fts5: syntax error near '.'". After the fix, the call reaches
// the index without error and finds the seeded match.
func TestHandleSearch_DottedIdentifier_DoesNotError(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "proj1"
	store.UpsertProject(db.Project{ID: "proj1", Path: "/tmp/proj1", Name: "proj1", IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		// Seed a symbol whose qualified name contains the dotted token —
		// FTS5 should match it once the query is properly quoted.
		{ID: "s1", ProjectID: "proj1", FilePath: "a.go", Name: "Stat",
			QualifiedName: "os.Stat", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 5,
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   "os.Stat",
		"project": "proj1",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if result.IsError {
		t.Fatalf("search returned error for dotted identifier: %v", decode(t, result))
	}
}

// `my-component`-style hyphenated tokens used to error with
// "no such column: component" because FTS5 reads `-` as NOT.
func TestHandleSearch_HyphenatedIdentifier_DoesNotError(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "proj1"
	store.UpsertProject(db.Project{ID: "proj1", Path: "/tmp/proj1", Name: "proj1", IndexedAt: time.Now()})

	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":   "my-component",
		"project": "proj1",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if result.IsError {
		t.Fatalf("search returned error for hyphenated identifier: %v", decode(t, result))
	}
}
