package server

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// #344: search must reject whitespace-only queries with a clear
// input-validation error, not leak FTS5 / SQLite internals to the
// caller. Pre-fix, " " (single space) returned:
//   "search error: SQL logic error: fts5: syntax error near \"\""

func TestHandleSearch_WhitespaceOnlyQuery_RejectedWithFriendlyError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.sessionID = "ws-test"
	srv.sessionRoot = "/tmp/ws-test"

	cases := []string{" ", "  ", "\t", "\n", " \t \n "}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
				"query": q,
			}))
			if err != nil {
				t.Fatalf("handleSearch: %v", err)
			}
			if !result.IsError {
				t.Fatalf("expected IsError=true for whitespace-only query")
			}
			body := errorBody(result)
			if !strings.Contains(body, "query is required") {
				t.Errorf("expected friendly 'query is required' error; got %q", body)
			}
			// Leaky low-level error string must NOT appear.
			if strings.Contains(body, "fts5") || strings.Contains(body, "SQL logic error") {
				t.Errorf("leaky low-level error in response: %q", body)
			}
		})
	}
}

// errorBody extracts the first text block of an errResult. Errors are
// returned as a CallToolResult with IsError=true and a single text
// Content block; tests just want the message string back.
func errorBody(r *mcp.CallToolResult) string {
	if r == nil {
		return ""
	}
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// TestHandleSearch_UnbalancedQuote_RejectedWithFriendlyError pins
// #489: a single `"` (or any odd-count quote) used to leak SQLite's
// "unterminated string (1)" error to the agent. Phrase queries are a
// real feature, so the natural retry after a 0-result search is to
// add quotes — surface the matching-pair requirement instead of an
// SQL parser detail.
func TestHandleSearch_UnbalancedQuote_RejectedWithFriendlyError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.sessionID = "uq-test"
	srv.sessionRoot = "/tmp/uq-test"

	// Odd-count quote cases. Balanced-but-malformed queries like `"a "b`
	// are not in scope here — those produce different FTS5 errors and
	// will be handled in a separate phrase-syntax sanitizer.
	for _, q := range []string{`"`, `"login`, `login"`, `"""`} {
		t.Run(q, func(t *testing.T) {
			result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
				"query": q,
			}))
			if err != nil {
				t.Fatalf("handleSearch: %v", err)
			}
			if !result.IsError {
				t.Fatalf("expected IsError=true for unbalanced-quote query %q", q)
			}
			body := errorBody(result)
			if !strings.Contains(body, "unbalanced quote") {
				t.Errorf("expected friendly 'unbalanced quote' error; got %q", body)
			}
			if strings.Contains(body, "SQL logic error") || strings.Contains(body, "unterminated string") {
				t.Errorf("leaky low-level error in response: %q", body)
			}
		})
	}
}

// TestHandleSearch_BalancedQuote_RunsThrough pins the inverse: a
// well-formed phrase query (matched pair) must NOT trigger the
// pre-flight rejection.
func TestHandleSearch_BalancedQuote_RunsThrough(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.sessionID = "bq-test"
	srv.sessionRoot = "/tmp/bq-test"

	// We don't care about results — just that the call doesn't
	// IsError with the pre-flight unbalanced-quote message.
	result, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": `"login flow"`,
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if result.IsError {
		body := errorBody(result)
		if strings.Contains(body, "unbalanced quote") {
			t.Errorf("balanced phrase query was rejected by the unbalanced-quote check; body=%q", body)
		}
	}
}
