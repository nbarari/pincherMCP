package server

import (
	"context"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
	"github.com/kwad77/pincher/internal/index"
)

// #1259: context_for_task — composite tool tests.
//
// Coverage follows the project's positive/negative/control/cross-check
// pattern. The handler composes search → context → trace + neighbors +
// changes; each branch needs deterministic exercise.

const composeGoSrc = `package compose

// Compute is the seed function for composite tests.
func Compute(x int) int {
	return helperA(x) + helperB(x)
}

func helperA(x int) int {
	return x * 2
}

func helperB(x int) int {
	return x + 1
}

func Caller() {
	_ = Compute(42)
}

type Widget struct{ ID int }

func (w *Widget) Render() string {
	return Compute(w.ID).String()
}
`

func setupComposeTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	srv, store, root := newTestServer(t)
	srv.sessionRoot = root
	writeGoFile(t, root, "compose.go", composeGoSrc)
	idx := index.New(store)
	res, err := idx.Index(context.Background(), root, false)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	// Indexer auto-allocates a project_id from the path hash — use
	// that one for handler scoping rather than a manually-upserted ID.
	srv.sessionID = res.ProjectID
	// Suppress unused-import warning if test doesn't otherwise touch db.
	_ = db.Project{}
	_ = time.Now()
	return srv, root, res.ProjectID
}

// Positive: task-driven composite returns a non-empty envelope with at
// least one seed (matching "Compute") and the expected envelope keys.
func TestContextForTask_TaskDriven_ReturnsComposite(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupComposeTestServer(t)

	res, err := srv.handleContextForTask(context.Background(), makeReq(map[string]any{
		"task":    "Compute",
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handleContextForTask: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success; got %s", textOf(t, res))
	}
	body := decode(t, res)

	for _, key := range []string{"task", "seeds", "neighbors", "callers", "callees", "recent_changes"} {
		if _, ok := body[key]; !ok {
			t.Errorf("envelope missing key %q; got keys: %v", key, mapKeysContextForTask(body))
		}
	}

	seeds, _ := body["seeds"].([]any)
	if len(seeds) == 0 {
		t.Fatalf("expected at least one seed for task=\"Compute\"; got 0. body: %#v", body)
	}

	// Top seed should be Compute itself — the BM25 anchor.
	first, _ := seeds[0].(map[string]any)
	if name, _ := first["name"].(string); name != "Compute" {
		t.Errorf("expected top seed name=Compute; got %q", name)
	}
}

// Positive: seed_id-driven composite skips search and goes straight to
// callers + callees + neighbors. Validates the seed-anchor branch.
func TestContextForTask_SeedIDDriven_SkipsSearch(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupComposeTestServer(t)

	seedID := "compose.go::compose.Compute#Function"
	res, err := srv.handleContextForTask(context.Background(), makeReq(map[string]any{
		"seed_id": seedID,
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handleContextForTask: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success; got %s", textOf(t, res))
	}
	body := decode(t, res)
	seeds, _ := body["seeds"].([]any)
	if len(seeds) != 1 {
		t.Fatalf("seed_id-driven path should yield exactly 1 seed; got %d. body: %#v", len(seeds), body)
	}
	first, _ := seeds[0].(map[string]any)
	if got, _ := first["id"].(string); got != seedID {
		t.Errorf("expected seed.id=%q; got %q", seedID, got)
	}

	// Compute calls helperA + helperB → both should appear in callees.
	callees, _ := body["callees"].([]any)
	calleeNames := map[string]bool{}
	for _, c := range callees {
		if m, ok := c.(map[string]any); ok {
			if name, _ := m["name"].(string); name != "" {
				calleeNames[name] = true
			}
		}
	}
	if !calleeNames["helperA"] || !calleeNames["helperB"] {
		t.Errorf("expected callees to include helperA + helperB; got %v", calleeNames)
	}

	// Caller and Render both call Compute → both should appear in callers.
	callers, _ := body["callers"].([]any)
	callerNames := map[string]bool{}
	for _, c := range callers {
		if m, ok := c.(map[string]any); ok {
			if name, _ := m["name"].(string); name != "" {
				callerNames[name] = true
			}
		}
	}
	if !callerNames["Caller"] {
		t.Errorf("expected callers to include Caller; got %v", callerNames)
	}
}

// Negative: missing both task AND seed_id produces a rich-error
// envelope (failure-as-pedagogy) — not a bare error.
func TestContextForTask_MissingAnchor_RichError(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupComposeTestServer(t)

	res, err := srv.handleContextForTask(context.Background(), makeReq(map[string]any{
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handleContextForTask: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected error result when neither task nor seed_id passed; got success")
	}
	text := textOf(t, res)
	// Error should name both anchor args + the "next_steps" remediation.
	for _, want := range []string{"task", "seed_id", "either"} {
		if !containsSubstr(text, want) {
			t.Errorf("rich error should mention %q; got: %s", want, text)
		}
	}
}

// Negative: passing both task AND seed_id is rejected (mutually
// exclusive per InputSchema).
func TestContextForTask_BothAnchors_RichError(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupComposeTestServer(t)

	res, err := srv.handleContextForTask(context.Background(), makeReq(map[string]any{
		"task":    "Compute",
		"seed_id": "compose.go::compose.Compute#Function",
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handleContextForTask: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected error when both task AND seed_id passed; got success")
	}
}

// Control: empty-seed path stamps _meta.empty_reason (#1252 enum) AND
// surfaces a next_steps recovery list. Tests the integration between
// #1259 and the empty_reason taxonomy.
func TestContextForTask_NoMatchingSeeds_StampsEmptyReason(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupComposeTestServer(t)

	res, err := srv.handleContextForTask(context.Background(), makeReq(map[string]any{
		"task":    "definitely_no_such_symbol_xyzzy_unique_token",
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handleContextForTask: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("expected _meta on empty-seed response; got body: %#v", body)
	}
	reason, _ := meta["empty_reason"].(string)
	if reason != EmptyReasonNoResultsInCorpus {
		t.Errorf("empty_reason = %q; want %q (the #1252 enum)", reason, EmptyReasonNoResultsInCorpus)
	}
	if _, hasDiag := meta["diagnosis"]; !hasDiag {
		t.Errorf("diagnosis must accompany empty_reason; got: %v", mapKeysContextForTask(meta))
	}
	if _, hasNext := meta["next_steps"]; !hasNext {
		t.Errorf("empty-seed response should include next_steps recovery suggestions")
	}
	// Envelope keys still present (zero-len arrays, not omitted).
	for _, key := range []string{"seeds", "neighbors", "callers", "callees", "recent_changes"} {
		v, ok := body[key]
		if !ok {
			t.Errorf("envelope key %q missing on empty path; want zero-len array", key)
			continue
		}
		arr, _ := v.([]any)
		if len(arr) != 0 {
			t.Errorf("envelope key %q should be zero-len on empty path; got %d entries", key, len(arr))
		}
	}
}

// #1591 v0.83: seed_id-not-found is target-not-resolved, not
// no-results-in-corpus. The two failure shapes are distinct and
// why_empty answers each differently — conflating them sends the
// caller to the wrong recovery action.
func TestContextForTask_UnresolvedSeedID_StampsTargetNotResolved(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupComposeTestServer(t)

	res, err := srv.handleContextForTask(context.Background(), makeReq(map[string]any{
		"seed_id": "doesnotexist.go::nowhere.NoSuch#Function",
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handleContextForTask: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("expected _meta on empty-seed response")
	}
	reason, _ := meta["empty_reason"].(string)
	if reason != EmptyReasonTargetNotResolved {
		t.Errorf("seed_id-not-found should stamp %q; got %q",
			EmptyReasonTargetNotResolved, reason)
	}
}

// Control: max_seeds cap is honored. Even when the task could match
// many symbols, the composite expands at most max_seeds. Validates the
// payload-bounding contract.
func TestContextForTask_MaxSeedsCap_HonorsLimit(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupComposeTestServer(t)

	res, err := srv.handleContextForTask(context.Background(), makeReq(map[string]any{
		// "helper" matches helperA + helperB; max_seeds=1 limits to one.
		"task":      "helper",
		"max_seeds": 1,
		"project":   projectID,
	}))
	if err != nil {
		t.Fatalf("handleContextForTask: %v", err)
	}
	body := decode(t, res)
	seeds, _ := body["seeds"].([]any)
	if len(seeds) > 1 {
		t.Errorf("max_seeds=1 should cap to 1 seed; got %d", len(seeds))
	}
}

// Cross-check: composite includes the seed's same-file neighbors. The
// neighbors-loop in the handler depends on GetSymbolsForFile; if that
// returns nothing, the cross-check tells us the test corpus shape is
// wrong (or the composite is silently dropping the neighbor branch).
func TestContextForTask_IncludesSameFileNeighbors(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupComposeTestServer(t)

	res, err := srv.handleContextForTask(context.Background(), makeReq(map[string]any{
		"seed_id": "compose.go::compose.Compute#Function",
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handleContextForTask: %v", err)
	}
	body := decode(t, res)
	neighbors, _ := body["neighbors"].([]any)
	if len(neighbors) == 0 {
		t.Fatalf("expected non-empty neighbors (helperA, helperB, Caller, Widget at minimum); got 0. body: %#v", body)
	}
	neighborNames := map[string]bool{}
	for _, n := range neighbors {
		if m, ok := n.(map[string]any); ok {
			if name, _ := m["name"].(string); name != "" {
				neighborNames[name] = true
			}
		}
	}
	// The neighbors should include the other top-level symbols in compose.go.
	for _, want := range []string{"helperA", "helperB", "Caller"} {
		if !neighborNames[want] {
			t.Errorf("expected neighbor %q in composite; got %v", want, neighborNames)
		}
	}
}

func mapKeysContextForTask(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
