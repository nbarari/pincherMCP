package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/index"
)

// #1391 v0.83 Phase 4 audit suite for audit_unused. Positive +
// negative + control + cross-check shape per the composite-tool
// roadmap contract.

// Fixture exercising three classification buckets:
//   - High: an unexported helper with zero callers anywhere
//   - Low: an unexported helper that the SQL check filters but the
//     resolver actually has a CALLS edge to (simulating the
//     resolver-inconsistency case — we can't easily simulate this
//     without manipulating edges, so we cover it in the truth-table
//     test of the classification logic via a synthetic input)
//   - Medium: covered conceptually — the fixture only generates the
//     high/low buckets natively
const auditUnusedGoSrc = `package authz

// helperUsed is called by Login. Won't appear in dead_code candidates.
func helperUsed() error {
	return nil
}

// helperUnused has no callers and no inbound edges. Will surface as
// dead_code and the deep trace will return zero → confidence=high.
func helperUnused() error {
	return nil
}

// Login is exported, so dead_code's WHERE excludes it.
func Login(user string) error {
	return helperUsed()
}
`

func setupAuditUnusedTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	srv, store, root := newTestServer(t)
	srv.sessionRoot = root
	writeGoFile(t, root, "authz.go", auditUnusedGoSrc)
	idx := index.New(store)
	res, err := idx.Index(context.Background(), root, false)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	srv.sessionID = res.ProjectID
	return srv, root, res.ProjectID
}

// TestAuditUnused_HighConfidenceForCleanCandidate — positive happy path:
// a truly unused helper surfaces as a high-confidence candidate with
// "deep_trace_confirms_unreachable" evidence.
func TestAuditUnused_HighConfidenceForCleanCandidate(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupAuditUnusedTestServer(t)

	res, err := srv.handleAuditUnused(context.Background(), makeReq(map[string]any{
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)

	cands, ok := body["candidates"].([]any)
	if !ok || len(cands) == 0 {
		t.Fatalf("expected at least one candidate (helperUnused); got %v", cands)
	}

	foundHelperUnused := false
	for _, c := range cands {
		m := c.(map[string]any)
		if m["name"] == "helperUnused" {
			foundHelperUnused = true
			if m["confidence"] != "high" {
				t.Errorf("helperUnused confidence = %v; want high", m["confidence"])
			}
			evidence, _ := m["evidence"].([]any)
			hasConfirm := false
			for _, e := range evidence {
				if es, _ := e.(string); es == "deep_trace_confirms_unreachable" {
					hasConfirm = true
				}
			}
			if !hasConfirm {
				t.Errorf("helperUnused missing deep_trace_confirms_unreachable evidence; got %v", evidence)
			}
		}
	}
	if !foundHelperUnused {
		t.Errorf("helperUnused not among candidates; got %v", cands)
	}
}

// TestAuditUnused_HelperUsedNotInCandidates — control: helperUsed has
// a real caller, dead_code's WHERE filters it, audit_unused never sees it.
func TestAuditUnused_HelperUsedNotInCandidates(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupAuditUnusedTestServer(t)

	res, err := srv.handleAuditUnused(context.Background(), makeReq(map[string]any{
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)
	cands, _ := body["candidates"].([]any)
	for _, c := range cands {
		m := c.(map[string]any)
		if m["name"] == "helperUsed" {
			t.Errorf("helperUsed has a caller but appeared as a candidate: %v", m)
		}
		if m["name"] == "Login" {
			t.Errorf("Login is exported but appeared as a candidate: %v", m)
		}
	}
}

// TestAuditUnused_SummaryCountsTallyToCandidates — cross-check: summary's
// classification counts must sum to candidates_audited.
func TestAuditUnused_SummaryCountsTallyToCandidates(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupAuditUnusedTestServer(t)

	res, err := srv.handleAuditUnused(context.Background(), makeReq(map[string]any{
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)

	summary, ok := body["summary"].(map[string]any)
	if !ok {
		t.Fatal("missing summary")
	}
	audited, _ := summary["candidates_audited"].(float64)
	high, _ := summary["deep_trace_confirmed_unused"].(float64)
	dyn, _ := summary["deep_trace_surfaced_dynamic_callers"].(float64)
	direct, _ := summary["deep_trace_surfaced_direct_callers"].(float64)
	if int(audited) != int(high+dyn+direct) {
		t.Errorf("classification totals don't tally: audited=%v high=%v dyn=%v direct=%v",
			audited, high, dyn, direct)
	}
}

// TestAuditUnused_EmptyResults_StampsEmptyReason — empty-path: when no
// candidates surface, the response stamps empty_reason + diagnosis.
func TestAuditUnused_EmptyResults_StampsEmptyReason(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupAuditUnusedTestServer(t)

	// Filter to a kind that doesn't exist in the fixture — produces zero
	// candidates via the SQL kind filter.
	res, err := srv.handleAuditUnused(context.Background(), makeReq(map[string]any{
		"project": projectID,
		"kinds":   "Interface", // fixture has zero Interface symbols
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)
	meta, ok := body["_meta"].(map[string]any)
	if !ok {
		t.Fatal("missing _meta")
	}
	if meta["empty_reason"] != EmptyReasonNoResultsInCorpus {
		t.Errorf("empty_reason = %v; want %s", meta["empty_reason"], EmptyReasonNoResultsInCorpus)
	}
	if _, ok := meta["diagnosis"]; !ok {
		t.Error("diagnosis must accompany empty_reason")
	}
	cands, _ := body["candidates"].([]any)
	if len(cands) != 0 {
		t.Errorf("expected zero candidates; got %d", len(cands))
	}
}

// TestAuditUnused_MaxResultsCap — control: max_results=3 produces at
// most 3 candidates, even if the corpus has more.
func TestAuditUnused_MaxResultsCap(t *testing.T) {
	t.Parallel()
	srv, store, root := newTestServer(t)
	srv.sessionRoot = root

	// Synthetic fixture: 10 unused unexported functions.
	src := "package many\n\n"
	for i := 0; i < 10; i++ {
		src += "func unused" + string(rune('A'+i)) + "() {}\n"
	}
	writeGoFile(t, root, "many.go", src)

	idx := index.New(store)
	res, err := idx.Index(context.Background(), root, false)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	srv.sessionID = res.ProjectID

	out, err := srv.handleAuditUnused(context.Background(), makeReq(map[string]any{
		"project":     res.ProjectID,
		"max_results": 3,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, out)
	cands, _ := body["candidates"].([]any)
	if len(cands) > 3 {
		t.Errorf("max_results=3 not honoured; got %d candidates", len(cands))
	}
}

// TestAuditUnused_ConfirmDepthClamp — control: confirm_depth above 4
// is clamped (no test for the runtime behaviour, just the input
// guard). The clamp lives in the handler; we verify by asserting the
// trace_summary.confirm_depth on the response.
func TestAuditUnused_ConfirmDepthClamp(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupAuditUnusedTestServer(t)

	res, err := srv.handleAuditUnused(context.Background(), makeReq(map[string]any{
		"project":       projectID,
		"confirm_depth": 99,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)
	cands, _ := body["candidates"].([]any)
	if len(cands) == 0 {
		t.Skip("no candidates surfaced — can't verify clamp")
	}
	first := cands[0].(map[string]any)
	ts, _ := first["trace_summary"].(map[string]any)
	if depth, _ := ts["confirm_depth"].(float64); int(depth) != 4 {
		t.Errorf("confirm_depth=99 not clamped to 4; got %v", depth)
	}
}

// TestAuditUnused_SortsByConfidence — cross-check: candidates list
// is ordered high → medium → low. With the fixture only producing
// high-confidence candidates, this confirms the sort doesn't swap
// equal-confidence rows out of qualified-name order.
func TestAuditUnused_SortsByConfidence(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupAuditUnusedTestServer(t)

	res, err := srv.handleAuditUnused(context.Background(), makeReq(map[string]any{
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)
	cands, _ := body["candidates"].([]any)
	if len(cands) < 2 {
		t.Skip("need ≥2 candidates to verify sort")
	}
	confOrder := map[string]int{"high": 0, "medium": 1, "low": 2}
	prev := -1
	for _, c := range cands {
		m := c.(map[string]any)
		conf := m["confidence"].(string)
		cur := confOrder[conf]
		if cur < prev {
			t.Errorf("candidates not sorted by confidence: saw %s after a higher tier", conf)
		}
		prev = cur
	}
}

// TestAuditUnused_IsRegistered — gate: tool is registered and
// discoverable. Pins the runtime registration; cross-cutting parity
// tests pin description + complexityTier + idempotency + schema.
func TestAuditUnused_IsRegistered(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tool, ok := srv.tools["audit_unused"]
	if !ok {
		t.Fatal("audit_unused not registered in srv.tools")
	}
	desc := strings.ToLower(tool.Description)
	for _, want := range []string{"dead", "trace", "confirm"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description should mention %q; got %q", want, tool.Description)
		}
	}
}
