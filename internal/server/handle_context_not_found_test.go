package server

import (
	"context"
	"strings"
	"testing"
)

// #1008: handleContext shared handleSymbol's silent "symbol not found"
// shape pre-#704 — handleSymbol got upgraded to errResultRich with
// search+list next_steps, handleContext kept its bare errResult. Same
// failure-as-pedagogy gap. Agents reading via context on a stale ID
// had no inline recovery path; this test pins the rich envelope shape.

func TestHandleContext_UnknownSymbol_ReturnsRichEnvelopeWithSearchAndList(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	res, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id": "internal/ghost/missing.go::ghost.DoesNotExist#Function",
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError; got %s", textOf(t, res))
	}

	body := decode(t, res)
	errStr, _ := body["error"].(string)
	if !strings.Contains(errStr, "DoesNotExist") || !strings.Contains(errStr, "not found") {
		t.Errorf("expected 'symbol ... not found' message naming the id; got %q", errStr)
	}

	meta, _ := body["_meta"].(map[string]any)
	steps, _ := meta["next_steps"].([]any)
	if len(steps) < 2 {
		t.Fatalf("expected at least 2 next_steps (search + list); got %d", len(steps))
	}

	wantTools := map[string]bool{"search": false, "list": false}
	for _, s := range steps {
		step, _ := s.(map[string]any)
		if tool, _ := step["tool"].(string); wantTools[tool] == false {
			if _, present := wantTools[tool]; present {
				wantTools[tool] = true
			}
		}
	}
	for tool, found := range wantTools {
		if !found {
			t.Errorf("expected next_step for %q; got steps=%v", tool, steps)
		}
	}

	// The search step should carry the short name extracted from the id
	// (DoesNotExist), so the agent can paste-and-go.
	for _, s := range steps {
		step, _ := s.(map[string]any)
		if tool, _ := step["tool"].(string); tool == "search" {
			args, _ := step["args"].(string)
			if !strings.Contains(args, "DoesNotExist") {
				t.Errorf("search step args should embed the short name 'DoesNotExist'; got %q", args)
			}
		}
	}
}
