package server

import (
	"context"
	"strings"
	"testing"
)

// #1075: pre-fix `target=codex` (and any other AlwaysGlobal target)
// returned `results: []` with no signal — the user got nothing back,
// not a refusal, not an explanation. Same silent-confidently-wrong
// family as #1063/#1064/#1065. Now: a skipped_always_global entry
// per dropped target with a CLI-fallback affordance.

func TestHandleInit_TargetCodex_SkippedAlwaysGlobalEntry(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tmp := t.TempDir()
	setSessionRoot(t, srv, tmp)

	res, err := srv.handleInit(context.Background(), makeReq(map[string]any{
		"target": "codex",
	}))
	if err != nil {
		t.Fatalf("handleInit: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleInit unexpected error: %s", textOf(t, res))
	}
	body := decode(t, res)
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("expected a results entry for codex (skipped); got empty %v", body)
	}

	var codexEntry map[string]any
	for _, r := range results {
		e, _ := r.(map[string]any)
		if n, _ := e["target"].(string); n == "codex" {
			codexEntry = e
			break
		}
	}
	if codexEntry == nil {
		t.Fatalf("no codex entry in results; got %v", results)
	}
	action, _ := codexEntry["action"].(string)
	if action != "skipped_always_global" {
		t.Errorf("expected codex action=skipped_always_global; got %q (entry=%v)", action, codexEntry)
	}
	reason, _ := codexEntry["reason"].(string)
	if !strings.Contains(reason, "pincher init --target=codex") {
		t.Errorf("reason must point at the CLI fallback; got %q", reason)
	}
}
