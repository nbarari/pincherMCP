package server

import (
	"context"
	"strings"
	"testing"
)

// mustProject's error path used to return a bare errResult — every
// tool that scopes to a project (architecture / schema / query / trace
// / dead_code / neighborhood / changes / search / etc.) shared the same
// silent "project not found" envelope. Now mustProject emits an
// errResultRich with list + index next_steps so the recovery path is
// inline. Tested through handleArchitecture as a representative
// caller; the same shape lands on every other shared-mustProject
// surface.

func TestMustProject_UnknownProject_ReturnsRichEnvelopeWithListAndIndex(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = ""
	srv.sessionRoot = ""

	res, err := srv.handleArchitecture(context.Background(), makeReq(map[string]any{
		"project": "ghost-project-xyz",
	}))
	if err != nil {
		t.Fatalf("handleArchitecture: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError; got %s", textOf(t, res))
	}

	body := decode(t, res)
	errStr, _ := body["error"].(string)
	if !strings.Contains(errStr, "ghost-project-xyz") || !strings.Contains(errStr, "not found") {
		t.Errorf("expected 'project ghost-project-xyz not found'; got %q", errStr)
	}

	meta, _ := body["_meta"].(map[string]any)
	steps, _ := meta["next_steps"].([]any)
	if len(steps) < 2 {
		t.Fatalf("expected at least 2 next_steps (list + index); got %d", len(steps))
	}

	wantTools := map[string]bool{"list": false, "index": false}
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
}
