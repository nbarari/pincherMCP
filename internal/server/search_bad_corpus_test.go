package server

import (
	"context"
	"strings"
	"testing"
)

// #1065: search with an unknown corpus value previously bubbled the
// DB layer's `unknown corpus "x" (valid: ...)` error through as a
// bare `search error: ...` string with no _meta envelope. Same
// silent-confidently-wrong family as #1063/#1064 (project not found).
// Upfront validation now rejects with rich envelope listing the three
// valid corpus values as candidate calls.

func TestHandleSearch_BadCorpus_RichErrorWithCorpusOptions(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	res, err := srv.handleSearch(context.Background(), makeReq(map[string]any{
		"query":  "anything",
		"corpus": "cdoe", // common typo
	}))
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on bad corpus; got %s", textOf(t, res))
	}
	body := decode(t, res)
	errStr, _ := body["error"].(string)
	if !strings.Contains(errStr, "cdoe") {
		t.Errorf("expected the typo'd corpus name in the error; got %q", errStr)
	}
	if !strings.Contains(errStr, "code") || !strings.Contains(errStr, "config") || !strings.Contains(errStr, "docs") {
		t.Errorf("expected the three valid corpora named in the error; got %q", errStr)
	}
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("expected _meta envelope on bad-corpus error; got bare error %q", errStr)
	}
	stepsRaw, _ := meta["next_steps"].([]any)
	if len(stepsRaw) < 4 {
		t.Fatalf("expected ≥4 next_steps (omit + code + config + docs); got %d", len(stepsRaw))
	}
	// All four next_steps must be search calls naming the same query.
	for i, s := range stepsRaw {
		step, _ := s.(map[string]any)
		if tool, _ := step["tool"].(string); tool != "search" {
			t.Errorf("step %d: expected tool=\"search\"; got %q", i, tool)
		}
		if argsStr, _ := step["args"].(string); !strings.Contains(argsStr, "anything") {
			t.Errorf("step %d: expected the user's query in next_step args; got %q", i, argsStr)
		}
	}
}

// Control for corpus="all" soft-redirect already covered by
// TestHandleSearch_CorpusAllSoftRedirects and
// TestHandleSearch_CorpusAll_SurfacesWarning — those use the
// pre-existing project setup. The strict-reject path added here only
// fires for values OTHER than "" / "code" / "config" / "docs" / "all",
// so it doesn't regress those cases.
