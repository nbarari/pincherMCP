package server

import (
	"context"
	"strings"
	"testing"
)

// #509: regex meta-patterns (.*  .+ .?) in search query must hit the
// pre-flight rather than leak FTS5 syntax errors. Narrow on purpose —
// dotted identifiers (db.Open) and prefix wildcards (auth*) must
// continue to work.
func TestHandleSearch_RegexInQuery_RejectedWithFriendlyError(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "regex-test"

	cases := []string{
		"handle.*Changes", // .*
		"foo.+bar",        // .+
		"name.?",          // .?
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			req := makeReq(map[string]any{"query": q})
			req.Params.Name = "search"
			result, err := srv.handleSearch(context.Background(), req)
			if err != nil {
				t.Fatalf("handleSearch: %v", err)
			}
			if !result.IsError {
				t.Fatalf("expected IsError=true on regex meta-pattern; got success")
			}
			body := errorBody(result)
			if !strings.Contains(body, "regex sequence") {
				t.Errorf("error should mention 'regex sequence'; got %q", body)
			}
			if !strings.Contains(body, "query") || !strings.Contains(body, "=~") {
				t.Errorf("error should redirect to `query` tool with =~; got %q", body)
			}
			// Raw FTS5/SQL leaks must NOT appear.
			if strings.Contains(body, "fts5") || strings.Contains(body, "SQL logic error") {
				t.Errorf("raw SQL error must not leak; got %q", body)
			}
		})
	}
}

// Dotted identifiers (db.Open) are common search inputs and must NOT
// trigger the regex pre-flight — they're rescued by the existing
// sanitizeFTS5Query (#424).
func TestHandleSearch_DottedIdentifier_NoRegexFalsePositive(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "dotted-test"

	cases := []string{
		"db.Open",
		"os.Stat",
		"a.b.c",
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			req := makeReq(map[string]any{"query": q})
			req.Params.Name = "search"
			result, err := srv.handleSearch(context.Background(), req)
			if err != nil {
				t.Fatalf("handleSearch: %v", err)
			}
			body := errorBody(result)
			if strings.Contains(body, "regex sequence") {
				t.Errorf("dotted identifier %q must not trigger regex pre-flight; got %q", q, body)
			}
		})
	}
}

// Prefix wildcards (auth*) are valid FTS5 syntax — must NOT be flagged.
func TestHandleSearch_PrefixWildcard_NoRegexFalsePositive(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "wildcard-test"

	req := makeReq(map[string]any{"query": "auth*"})
	req.Params.Name = "search"
	result, err := srv.handleSearch(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := errorBody(result)
	if strings.Contains(body, "regex sequence") {
		t.Errorf("prefix wildcard 'auth*' must not trigger regex pre-flight; got %q", body)
	}
}

// Regex chars INSIDE a quoted phrase are literal — pre-flight skips
// them.
func TestHandleSearch_RegexInsideQuotedPhrase_AllowedThrough(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "regex-q-test"

	req := makeReq(map[string]any{"query": `"handle.*Changes"`})
	req.Params.Name = "search"
	result, err := srv.handleSearch(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := errorBody(result)
	if strings.Contains(body, "regex sequence") {
		t.Errorf("regex inside quoted phrase must not trigger pre-flight; got %q", body)
	}
}

// #736: a stem-less prefix wildcard ("*", "**") is not a valid FTS5
// query — SQLite rejects it with the raw "unknown special query"
// logic error. The pre-flight must catch it and return a friendly
// error redirecting to a real list-all, not leak the SQL noise.
func TestHandleSearch_BareWildcard_RejectedWithFriendlyError(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "bare-wildcard-test"

	for _, q := range []string{"*", "**", "***"} {
		t.Run(q, func(t *testing.T) {
			req := makeReq(map[string]any{"query": q})
			req.Params.Name = "search"
			result, err := srv.handleSearch(context.Background(), req)
			if err != nil {
				t.Fatalf("handleSearch: %v", err)
			}
			if !result.IsError {
				t.Fatalf("expected IsError=true on bare wildcard %q; got success", q)
			}
			body := errorBody(result)
			// Raw SQL leaks must NOT appear.
			if strings.Contains(body, "SQL logic error") || strings.Contains(body, "unknown special query") {
				t.Errorf("raw SQL error must not leak for %q; got %q", q, body)
			}
			// Must point at the stem requirement and the query-tool fallback.
			if !strings.Contains(body, "stem") {
				t.Errorf("error should mention the prefix-wildcard 'stem' requirement; got %q", body)
			}
			if !strings.Contains(body, "MATCH (n)") {
				t.Errorf("error should redirect to the query tool for list-all; got %q", body)
			}
		})
	}
}

// A real prefix wildcard with a stem (auth*) must still work — the
// #736 pre-flight only fires on stem-less wildcards.
func TestHandleSearch_StemmedWildcard_NotRejectedByBareWildcardPreflight(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "stemmed-wildcard-test"

	req := makeReq(map[string]any{"query": "auth*"})
	req.Params.Name = "search"
	result, err := srv.handleSearch(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	body := errorBody(result)
	if strings.Contains(body, "stem") {
		t.Errorf("stemmed wildcard 'auth*' must not trigger the bare-wildcard pre-flight; got %q", body)
	}
}

// #786: a slash-delimited regex literal (/handle[A-Z]\w+/) sailed past
// the preflight — the .*/.+/.? scan didn't match, the #424 sanitizer
// mangled [A-Z]\w into FTS5 token soup, and the agent got a misleading
// "lower min_confidence" diagnosis instead of the regex redirect.
func TestHandleSearch_SlashDelimitedRegex_RejectedWithFriendlyError(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "slash-regex-test"

	for _, q := range []string{`/handle[A-Z]\w+/`, `/^foo$/`, `/bar/`} {
		t.Run(q, func(t *testing.T) {
			req := makeReq(map[string]any{"query": q})
			req.Params.Name = "search"
			result, err := srv.handleSearch(context.Background(), req)
			if err != nil {
				t.Fatalf("handleSearch: %v", err)
			}
			if !result.IsError {
				t.Fatalf("expected IsError=true on slash-delimited regex; got success")
			}
			body := errorBody(result)
			if !strings.Contains(body, "regex sequence") {
				t.Errorf("error should mention 'regex sequence'; got %q", body)
			}
			if !strings.Contains(body, "=~") {
				t.Errorf("error should redirect to the query tool with =~; got %q", body)
			}
			// #788: pinchQL's =~ takes a BARE regex — the redirect must
			// strip the surrounding slashes, not echo them into the
			// example (`=~ '/bar/'` matches a literal slash, zero rows).
			inner := q[1 : len(q)-1]
			if !strings.Contains(body, "=~ '"+inner+"'") {
				t.Errorf("=~ example should use the slash-stripped regex %q; got %q", inner, body)
			}
			if strings.Contains(body, "=~ '/") {
				t.Errorf("=~ example must not keep the leading slash delimiter; got %q", body)
			}
		})
	}
}

// A path fragment with internal slashes (internal/server) must NOT trip
// the slash-delimited regex check — it only fires when slashes wrap the
// whole query.
func TestHandleSearch_PathFragment_NoSlashRegexFalsePositive(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "path-frag-test"

	for _, q := range []string{"internal/server", "/etc/hosts", "cmd/pinch/"} {
		t.Run(q, func(t *testing.T) {
			req := makeReq(map[string]any{"query": q})
			req.Params.Name = "search"
			result, err := srv.handleSearch(context.Background(), req)
			if err != nil {
				t.Fatalf("handleSearch: %v", err)
			}
			if strings.Contains(errorBody(result), "regex sequence") {
				t.Errorf("path fragment %q must not trigger the slash-regex preflight", q)
			}
		})
	}
}

// Pure unit test for the helper.
func TestFirstFTS5IncompatibleRegexChar_Coverage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"plain", ""},
		{"db.Open", ""},  // single dot — sanitizer handles
		{"auth*", ""},    // prefix wildcard — FTS5 supports
		{"a.b.c", ""},    // dotted ident
		{"handle.*X", ".*"},
		{"foo.+bar", ".+"},
		{"baz.?qux", ".?"},
		{`"a.*b"`, ""}, // inside quote
		// #786: slash-delimited regex literal.
		{`/handle[A-Z]\w+/`, "/.../"},
		{`/bar/`, "/.../"},
		{"internal/server", ""}, // internal slash — not wrapped
		{"/etc/hosts", ""},      // leading slash only
		{"//", ""},              // too short to be a regex literal
	}
	for _, tc := range cases {
		got := firstFTS5IncompatibleRegexChar(tc.in)
		if got != tc.want {
			t.Errorf("firstFTS5IncompatibleRegexChar(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
