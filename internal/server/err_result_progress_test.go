package server

import (
	"strings"
	"testing"
)

// #974: errResultRich must surface `index_in_progress` (and a
// retry-after-pass nudge in next_steps) when the session indexer is
// mid-pass. Pre-fix, success responses carried this signal but error
// responses didn't — agents seeing "symbol not found" during a
// binary-drift re-extract had no hint that the result was transient
// and concluded the symbol genuinely didn't exist.

func TestErrResultRich_IdleIndexer_NoInProgressFlag(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "p974-idle"

	res := srv.errResultRich("symbol not found", []map[string]string{{
		"tool": "search", "args": `{"query":"X"}`, "why": "look by name",
	}})
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	if _, present := meta["index_in_progress"]; present {
		t.Errorf("idle indexer must not emit index_in_progress on error path; got meta=%v", meta)
	}
	if _, present := meta["warnings"]; present {
		t.Errorf("idle indexer must not emit mid-pass warnings on error path; got meta=%v", meta)
	}
	steps, _ := meta["next_steps"].([]any)
	if len(steps) != 1 {
		t.Errorf("idle indexer should preserve caller's next_steps unchanged; got %d entries", len(steps))
	}
}

// #993: when files_done == files_total the per-file walk has finished
// but cross-file resolvers are still running. The warning must say
// "finalizing", not the misleading "mid-pass (55/55 files)".
func TestErrResultRich_FilesAllDone_SaysFinalizingNotMidPass(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "p993-final"
	srv.indexer.MarkActiveForTest("p993-final", 55, 55)
	t.Cleanup(func() { srv.indexer.UnmarkActiveForTest("p993-final") })

	res := srv.errResultRich("symbol not found", []map[string]string{{
		"tool": "search", "args": `{"query":"X"}`, "why": "look by name",
	}})
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatal("expected a warning when indexer is active")
	}
	warn, _ := warnings[0].(string)
	if strings.Contains(warn, "mid-pass") {
		t.Errorf("warning at files_done==files_total must NOT say 'mid-pass'; got %q", warn)
	}
	if !strings.Contains(warn, "finalizing") {
		t.Errorf("warning at files_done==files_total should say 'finalizing'; got %q", warn)
	}
}

func TestErrResultRich_ActiveIndexer_EmitsInProgressFlag(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "p974-active"
	srv.indexer.MarkActiveForTest("p974-active", 73, 478)
	t.Cleanup(func() { srv.indexer.UnmarkActiveForTest("p974-active") })

	res := srv.errResultRich("symbol not found", []map[string]string{{
		"tool": "search", "args": `{"query":"X"}`, "why": "look by name",
	}})
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}

	prog, ok := meta["index_in_progress"].(map[string]any)
	if !ok {
		t.Fatalf("expected index_in_progress on error path during mid-pass; meta=%v", meta)
	}
	if d, _ := prog["files_done"].(float64); d != 73 {
		t.Errorf("files_done = %v, want 73", prog["files_done"])
	}
	if total, _ := prog["files_total"].(float64); total != 478 {
		t.Errorf("files_total = %v, want 478", prog["files_total"])
	}

	warnings, _ := meta["warnings"].([]any)
	foundMidPass := false
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "mid-pass") {
			foundMidPass = true
			break
		}
	}
	if !foundMidPass {
		t.Errorf("expected mid-pass warning in _meta.warnings; got %v", warnings)
	}

	steps, _ := meta["next_steps"].([]any)
	if len(steps) < 2 {
		t.Fatalf("expected at least 2 next_steps (retry-after-pass prepended + caller's); got %d", len(steps))
	}
	// The retry-after-pass hint must be FIRST so callers see it before
	// the original "search by short name" advice.
	first, _ := steps[0].(map[string]any)
	if tool, _ := first["tool"].(string); tool != "index" {
		t.Errorf("first next_step tool = %q, want index (retry-after-pass hint should lead); steps=%v", tool, steps)
	}
}

