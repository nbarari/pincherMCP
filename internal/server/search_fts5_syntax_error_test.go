package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1022: search used to leak raw "fts5: syntax error near" SQLite
// messages on malformed boolean queries (`foo AND` / `OR bar`) — the
// sanitizer catches dotted identifiers (#289) and unmatched quotes
// (#489) but can't repair an incomplete boolean expression. The bare
// error had no recovery affordance; agents hit it and stopped. Now:
// rich envelope with three next_steps:
//   - retry with the trailing boolean stripped (programmatic recover)
//   - wrap the whole query in quotes (treat as literal phrase)
//   - guide for shape help when the user genuinely meant something else.

func TestHandleSearch_FTS5SyntaxError_ReturnsRichEnvelope(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "fts-err-p"
	store.UpsertProject(db.Project{ID: "fts-err-p", Path: "/tmp/fts-err-p", Name: "fts-err-p"})

	res, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query": "handleSearch OR errResult AND",
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError; got %s", textOf(t, res))
	}

	body := decode(t, res)
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "fts5") || !strings.Contains(msg, "rejected") {
		t.Errorf("expected rich error message naming FTS5; got %q", msg)
	}

	meta, _ := body["_meta"].(map[string]any)
	steps, _ := meta["next_steps"].([]any)
	if len(steps) < 2 {
		t.Fatalf("expected at least 2 next_steps; got %d", len(steps))
	}

	// One step should propose the trailing-boolean-stripped retry.
	foundStrip := false
	for _, s := range steps {
		step, _ := s.(map[string]any)
		args, _ := step["args"].(string)
		// stripTrailingBoolean removes the trailing " AND" leaving the rest:
		// expected query body is "handleSearch OR errResult"
		if strings.Contains(args, "handleSearch OR errResult") && !strings.HasSuffix(strings.TrimSuffix(args, `"}`), "AND") {
			foundStrip = true
			break
		}
	}
	if !foundStrip {
		t.Errorf("expected a next_step that strips the trailing boolean; got steps=%v", steps)
	}
}

// Unit tests for the helper.
func TestStripTrailingBoolean(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"foo AND", "foo"},
		{"foo OR ", "foo"},
		{"foo NOT", "foo"},
		{"foo AND ", "foo"},
		{"foo", "foo"}, // no-op
		{"foo OR bar", "foo OR bar"},
		{"", ""},
		{"AND", "AND"}, // standalone — not stripped (would empty the query)
		{"foo AND OR", "foo AND"},
		{"foo AND OR NOT", "foo AND OR"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := stripTrailingBoolean(c.in)
			if got != c.want {
				t.Errorf("stripTrailingBoolean(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
