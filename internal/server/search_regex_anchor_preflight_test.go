package server

import (
	"context"
	"strings"
	"testing"
)

// #1110: regex anchors at the start/end of the query (`^handle` /
// `handle$`) leaked past the FTS5 pre-flight and got silently
// sanitized by sanitizeFTS5Query — the agent got an empty result with
// the generic "lower min_confidence" diagnosis instead of "drop the
// regex anchor". Same regex-leak family as #509 (`.*`, `.+`, `.?`) and
// #788 (slash-delimited regex literal).

func TestHandleSearch_LeadingCaretAnchor_RejectedWithFriendlyError(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "regex-anchor-test"

	cases := []string{
		"^handle",
		"^processOrder",
		"^abc",
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
				t.Fatalf("expected IsError=true on leading-caret anchor; got success")
			}
			body := errorBody(result)
			if !strings.Contains(body, "regex sequence") {
				t.Errorf("error should mention 'regex sequence'; got %q", body)
			}
			if !strings.Contains(body, "anchor") {
				t.Errorf("error should name the anchor; got %q", body)
			}
			if !strings.Contains(body, "=~") {
				t.Errorf("error should redirect to `query` tool with =~; got %q", body)
			}
		})
	}
}

func TestHandleSearch_TrailingDollarAnchor_RejectedWithFriendlyError(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "regex-anchor-test-2"

	req := makeReq(map[string]any{"query": "handle$"})
	req.Params.Name = "search"
	result, err := srv.handleSearch(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError=true on trailing-dollar anchor; got success")
	}
	body := errorBody(result)
	if !strings.Contains(body, "regex sequence") || !strings.Contains(body, "anchor") {
		t.Errorf("error should mention regex anchor; got %q", body)
	}
}

// Control: a bare `^` or `$` falls through to the FTS5 syntax-error
// path — the anchor pre-flight only fires on len>1 queries to avoid
// over-catching single-char inputs.
func TestHandleSearch_BareCaret_DoesNotTripAnchorPreflight(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "regex-anchor-bare-test"

	req := makeReq(map[string]any{"query": "^"})
	req.Params.Name = "search"
	result, err := srv.handleSearch(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	// May still be IsError (FTS5 will reject bare `^`), but the message
	// shouldn't be the anchor-preflight message — that's reserved for
	// real anchor patterns with content.
	body := errorBody(result)
	if strings.Contains(body, "anchor") {
		t.Errorf("bare `^` should not trigger anchor pre-flight; got %q", body)
	}
}

// Control: queries with no leading/trailing anchor must not trip the
// new check.
func TestHandleSearch_NormalQuery_DoesNotTripAnchorPreflight(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "regex-anchor-normal-test"

	cases := []string{"handle", "foo bar", "db.Open", "auth*"}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			req := makeReq(map[string]any{"query": q})
			req.Params.Name = "search"
			result, err := srv.handleSearch(context.Background(), req)
			if err != nil {
				t.Fatalf("handleSearch: %v", err)
			}
			body := errorBody(result)
			if strings.Contains(body, "anchor") {
				t.Errorf("query %q should not trigger anchor pre-flight; got %q", q, body)
			}
		})
	}
}

// Anchors INSIDE a quoted phrase must not trip the pre-flight — the
// quotes mean the chars are literal FTS5 tokens, not regex anchors.
// (Note: the existing pre-flight skips chars inside quotes for `.X`
// patterns; the anchor check is positional-only — at start[0] or
// end[-1] — so a quoted query like `"^foo"` matches at position 0 and
// SHOULD trip, intentionally, because FTS5 would still see the `^`.
// Skip the test for that edge case — it's a real corner.)
