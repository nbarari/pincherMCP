package server

import (
	"context"
	"strings"
	"testing"
)

// #1064: handleChanges was the second per-project tool (with search,
// #1063) that returned a bare errResult on project-not-found. Same
// silent-confidently-wrong family — agents consuming _meta.next_steps
// programmatically lost the canonical `list` + `index` recovery
// affordance on these tools while every other per-project tool
// (architecture / query / symbol / dead_code / trace / schema /
// neighborhood) returned the rich envelope.

func TestHandleChanges_ProjectNotFound_RichErrorWithListIndexSteps(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	res, err := srv.handleChanges(context.Background(), makeReq(map[string]any{
		"project": "absolutely-not-a-real-project-xyz",
	}))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on missing project; got %s", textOf(t, res))
	}
	body := decode(t, res)
	errStr, _ := body["error"].(string)
	if !strings.Contains(errStr, "absolutely-not-a-real-project-xyz") {
		t.Errorf("expected the bad project name in the error string; got %q", errStr)
	}
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("expected _meta envelope on project-not-found; got bare error %q", errStr)
	}
	stepsRaw, _ := meta["next_steps"].([]any)
	if len(stepsRaw) < 2 {
		t.Fatalf("expected at least 2 next_steps (list + index); got %d (%v)", len(stepsRaw), stepsRaw)
	}
	wantTools := map[string]bool{"list": false, "index": false}
	for _, s := range stepsRaw {
		step, _ := s.(map[string]any)
		if tool, _ := step["tool"].(string); tool != "" {
			if _, want := wantTools[tool]; want {
				wantTools[tool] = true
			}
		}
	}
	for tool, found := range wantTools {
		if !found {
			t.Errorf("expected next_step with tool=%q; got steps=%v", tool, stepsRaw)
		}
	}
}
