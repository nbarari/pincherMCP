package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// #1631 v0.85: adherence telemetry contract tests.

func TestNextStepsAdherence_RecordThenConsume_Credits_1631(t *testing.T) {
	t.Parallel()
	tr := &nextStepsAdherenceTracker{}
	tr.RecordEmitted("sess-A", []map[string]string{
		{"tool": "trace", "args": `{"id":"f.go::pkg.X#Function","direction":"outbound"}`},
	})
	emitted, followed := tr.Stats()
	if emitted != 1 || followed != 0 {
		t.Errorf("after RecordEmitted: emitted=%d followed=%d, want (1,0)", emitted, followed)
	}

	matched := tr.CheckAndConsume("sess-A", "trace", map[string]any{
		"id":        "f.go::pkg.X#Function",
		"direction": "outbound",
	})
	if !matched {
		t.Fatal("CheckAndConsume should match the stashed recommendation")
	}
	emitted, followed = tr.Stats()
	if emitted != 1 || followed != 1 {
		t.Errorf("after CheckAndConsume: emitted=%d followed=%d, want (1,1)", emitted, followed)
	}
}

func TestNextStepsAdherence_NonMatch_DoesNotCredit_1631(t *testing.T) {
	t.Parallel()
	tr := &nextStepsAdherenceTracker{}
	tr.RecordEmitted("sess-B", []map[string]string{
		{"tool": "trace", "args": `{"id":"f.go::pkg.X#Function","direction":"outbound"}`},
	})
	// Different direction — not the same recommendation.
	matched := tr.CheckAndConsume("sess-B", "trace", map[string]any{
		"id":        "f.go::pkg.X#Function",
		"direction": "inbound",
	})
	if matched {
		t.Error("non-matching args should not credit followed counter")
	}
	_, followed := tr.Stats()
	if followed != 0 {
		t.Errorf("followed=%d after non-match, want 0", followed)
	}
}

func TestNextStepsAdherence_MatchAndConsume_FiresOnce_1631(t *testing.T) {
	t.Parallel()
	tr := &nextStepsAdherenceTracker{}
	tr.RecordEmitted("sess-C", []map[string]string{
		{"tool": "search", "args": `{"query":"X","min_confidence":0.0}`},
	})
	args := map[string]any{"query": "X", "min_confidence": 0.0}
	if !tr.CheckAndConsume("sess-C", "search", args) {
		t.Fatal("first call should match")
	}
	if tr.CheckAndConsume("sess-C", "search", args) {
		t.Error("second call should NOT match — the recommendation was consumed")
	}
	_, followed := tr.Stats()
	if followed != 1 {
		t.Errorf("followed=%d, want 1 (match-and-consume invariant)", followed)
	}
}

func TestNextStepsAdherence_KeyOrderingInvariant_1631(t *testing.T) {
	t.Parallel()
	tr := &nextStepsAdherenceTracker{}
	// Emit with one key order, consume with another.
	tr.RecordEmitted("sess-D", []map[string]string{
		{"tool": "search", "args": `{"min_confidence":0.0,"query":"X"}`},
	})
	matched := tr.CheckAndConsume("sess-D", "search", map[string]any{
		"query":          "X",
		"min_confidence": 0.0,
	})
	if !matched {
		t.Error("differing key order in args should still match — canonicalization sorts keys")
	}
}

func TestNextStepsAdherence_CrossSessionIsolation_1631(t *testing.T) {
	t.Parallel()
	tr := &nextStepsAdherenceTracker{}
	tr.RecordEmitted("sess-E", []map[string]string{
		{"tool": "trace", "args": `{"id":"x"}`},
	})
	// Different session should NOT see sess-E's recommendation.
	if tr.CheckAndConsume("sess-F", "trace", map[string]any{"id": "x"}) {
		t.Error("recommendations should be per-session — sess-F cannot consume sess-E's stash")
	}
	if !tr.CheckAndConsume("sess-E", "trace", map[string]any{"id": "x"}) {
		t.Error("same session should still find the entry")
	}
}

func TestNextStepsAdherence_RingCap_EvictsOldest_1631(t *testing.T) {
	t.Parallel()
	tr := &nextStepsAdherenceTracker{}
	// Push adherenceRingCap+5 entries; the first 5 should be evicted.
	overflow := 5
	for i := 0; i < adherenceRingCap+overflow; i++ {
		tr.RecordEmitted("sess-G", []map[string]string{
			{"tool": "search", "args": `{"query":"q` + string(rune('A'+i)) + `"}`},
		})
	}
	// The first entry ("qA") should be evicted.
	if tr.CheckAndConsume("sess-G", "search", map[string]any{"query": "qA"}) {
		t.Error("oldest entry should have been FIFO-evicted past cap")
	}
	// The cap+1th-from-end entry should still be present.
	lastKept := string(rune('A' + overflow))
	if !tr.CheckAndConsume("sess-G", "search", map[string]any{"query": "q" + lastKept}) {
		t.Errorf("entry %q should still be present at cap boundary", "q"+lastKept)
	}
}

func TestNextStepsAdherence_EmptySession_NoCrash_1631(t *testing.T) {
	t.Parallel()
	tr := &nextStepsAdherenceTracker{}
	// CheckAndConsume on never-recorded session must not panic / credit.
	if tr.CheckAndConsume("sess-empty", "trace", map[string]any{}) {
		t.Error("never-recorded session should not match")
	}
	// Empty steps list is also safe.
	tr.RecordEmitted("sess-empty", nil)
	tr.RecordEmitted("sess-empty", []map[string]string{})
	emitted, followed := tr.Stats()
	if emitted != 0 || followed != 0 {
		t.Errorf("empty record should not increment counters: emitted=%d followed=%d", emitted, followed)
	}
}

func TestCanonicalArgsJSON_HandlesMalformed_1631(t *testing.T) {
	t.Parallel()
	// Malformed JSON falls back to the raw string, doesn't panic.
	got := canonicalArgsJSON(`{"unclosed":`)
	if got == "" {
		t.Error("malformed JSON should fall back to raw, got empty string")
	}
	if !strings.Contains(got, "unclosed") {
		t.Errorf("got %q, want fallback containing 'unclosed'", got)
	}
}

// Integration: pre-stash a recommendation, then make a matching
// query-shaped tool call, then assert (a) the followed counter
// incremented via recordQueryMetrics's CheckAndConsume hook and
// (b) the response carries `_meta.next_steps_adherence` populated
// by jsonResultWithMeta. Decoupled from any specific handler's
// emit logic so the test is independent of #1634 / future next_step
// additions — what's being tested is the adherence pipeline, not
// any single handler's recommendation text.
func TestNextStepsAdherence_EndToEnd_StashThenMatch_1631(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "adherence-e2e-1631"
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: pid + "/a.go::pkg.Target#Function", ProjectID: pid,
			FilePath: "a.go", Name: "Target", QualifiedName: "pkg.Target",
			Kind: "Function", Language: "Go"},
		{ID: pid + "/b.go::pkg.Caller#Function", ProjectID: pid,
			FilePath: "b.go", Name: "Caller", QualifiedName: "pkg.Caller",
			Kind: "Function", Language: "Go"},
	})
	store.BulkUpsertEdges([]db.Edge{
		{FromID: pid + "/b.go::pkg.Caller#Function", ToID: pid + "/a.go::pkg.Target#Function",
			Kind: "CALLS", ProjectID: pid, Confidence: 1},
	})

	// Pre-seed an adherence recommendation as if a prior call had
	// emitted it. The args shape mirrors the trace tool's contract.
	srv.nextStepsAdherence.RecordEmitted(pid, []map[string]string{
		{"tool": "trace", "args": `{"name":"Target","direction":"inbound"}`},
	})
	if emitted, followed := srv.nextStepsAdherence.Stats(); emitted != 1 || followed != 0 {
		t.Fatalf("after RecordEmitted: emitted=%d followed=%d, want (1,0)", emitted, followed)
	}

	// Agent re-issues the recommended call VERBATIM. Adding extra
	// args (even hermetic ones like project=) would break the
	// canonical-args match — the agent is expected to copy-paste the
	// suggested args. Session project is set above so the call still
	// scopes correctly without an explicit project field.
	//
	// Note: makeReq leaves req.Params.Name unset; CheckAndConsume runs
	// from recordQueryMetrics which gates on queryShapedTools[tool] —
	// tool comes from req.Params.Name via beginCall, so we need to
	// build the request manually with Name="trace" for the gate to fire.
	callArgs, _ := json.Marshal(map[string]any{
		"name":      "Target",
		"direction": "inbound",
	})
	resp, err := srv.handleTrace(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "trace", Arguments: callArgs},
	})
	if err != nil || resp.IsError {
		t.Fatalf("handleTrace: err=%v isErr=%v body=%v", err, resp.IsError, decode(t, resp))
	}

	emitted, followed := srv.nextStepsAdherence.Stats()
	if followed != 1 {
		t.Errorf("followed=%d after matching call, want 1", followed)
	}
	if emitted < 1 {
		t.Errorf("emitted=%d, want >= 1", emitted)
	}

	// The response _meta should now carry the adherence snapshot.
	m := decode(t, resp)
	meta, _ := m["_meta"].(map[string]any)
	adherence, _ := meta["next_steps_adherence"].(map[string]any)
	if adherence == nil {
		// Re-marshal the meta to make the failure log readable.
		j, _ := json.Marshal(meta)
		t.Fatalf("_meta.next_steps_adherence missing on response after match; meta=%s", j)
	}
	if got := adherence["followed"]; got == nil {
		t.Errorf("adherence.followed missing: %v", adherence)
	}
	// pct should be 100.0 when followed == emitted == 1.
	if got, _ := adherence["pct"].(float64); got != 100.0 {
		t.Errorf("adherence.pct=%v, want 100.0 (1/1 = 100%%)", adherence["pct"])
	}
}

func TestCanonicalArgsJSON_EmptyShapes_1631(t *testing.T) {
	t.Parallel()
	if got := canonicalArgsJSON(""); got != "{}" {
		t.Errorf("empty string → %q, want {}", got)
	}
	if got := canonicalArgsJSON("{}"); got != "{}" {
		t.Errorf("{} → %q, want {}", got)
	}
	if got := canonicalArgsMap(nil); got != "{}" {
		t.Errorf("nil map → %q, want {}", got)
	}
	if got := canonicalArgsMap(map[string]any{}); got != "{}" {
		t.Errorf("empty map → %q, want {}", got)
	}
}
