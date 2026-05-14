package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #703 + #704: trace/symbol/neighborhood not-found paths and out-of-range
// depth used to leave the agent stuck — bare text errors with no
// remediation hint, plus a `{hops:[], total:N>0}` invariant violation
// when negative depth was passed. These tests pin the failure-as-pedagogy
// envelope so future refactors don't quietly regress to text-only.

// TestTrace_DepthNegativeClamps confirms depth=-1 no longer produces
// `{hops:[], total:N>0}` — depth gets clamped to 1, a warning surfaces,
// and the response is internally consistent.
func TestTrace_DepthNegativeClamps(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "p", Path: "/tmp/p", Name: "p", EdgeCount: 1})
	srv.sessionID = "p"
	// Seed two symbols + a CALLS edge so the trace has something to
	// return when depth normalizes to 1.
	callerID := "x.go::pkg.Caller#Function"
	calleeID := "x.go::pkg.Callee#Function"
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: callerID, ProjectID: "p", Name: "Caller", QualifiedName: "pkg.Caller", Kind: "Function", FilePath: "x.go"},
		{ID: calleeID, ProjectID: "p", Name: "Callee", QualifiedName: "pkg.Callee", Kind: "Function", FilePath: "x.go"},
	})
	store.BulkUpsertEdges([]db.Edge{
		{ProjectID: "p", FromID: callerID, ToID: calleeID, Kind: "CALLS"},
	})

	res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":  "Caller",
		"depth": -1,
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	if res.IsError {
		t.Fatalf("trace returned IsError: %s", textOf(t, res))
	}
	body := decode(t, res)

	// Invariant: total must equal the sum of nodes across all hop levels.
	// Pre-fix: total was unbounded BFS output count while hops was empty.
	totalAny := body["total"]
	hopsAny, _ := body["hops"].([]any)
	total, _ := totalAny.(float64)
	hopNodeCount := 0
	for _, h := range hopsAny {
		hop, _ := h.(map[string]any)
		nodes, _ := hop["nodes"].([]any)
		hopNodeCount += len(nodes)
	}
	if int(total) != hopNodeCount {
		t.Errorf("invariant violated: total=%d but hops contain %d nodes (response: %s)", int(total), hopNodeCount, textOf(t, res))
	}

	// Warning must surface so the caller learns about the clamp.
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	foundClamp := false
	for _, w := range warnings {
		if s, ok := w.(string); ok && strings.Contains(s, "depth=-1") && strings.Contains(s, "clamped") {
			foundClamp = true
			break
		}
	}
	if !foundClamp {
		t.Errorf("expected _meta.warnings to contain a depth clamp message; got: %v", warnings)
	}
}

// TestTrace_InvalidDirectionWarnsAndRecovers confirms #839: a non-canonical
// `direction` value used to fall through every branch in db.traceViaCTE and
// silently return 0 hops — indistinguishable from a genuine "no callers"
// result. `callers`/`callees` now map to inbound/outbound with a warning;
// anything else falls back to `both` with a warning. In every case the
// trace still produces real hops.
func TestTrace_InvalidDirectionWarnsAndRecovers(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "p", Path: "/tmp/p", Name: "p", EdgeCount: 1})
	srv.sessionID = "p"
	callerID := "x.go::pkg.Caller#Function"
	calleeID := "x.go::pkg.Callee#Function"
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: callerID, ProjectID: "p", Name: "Caller", QualifiedName: "pkg.Caller", Kind: "Function", FilePath: "x.go"},
		{ID: calleeID, ProjectID: "p", Name: "Callee", QualifiedName: "pkg.Callee", Kind: "Function", FilePath: "x.go"},
	})
	store.BulkUpsertEdges([]db.Edge{
		{ProjectID: "p", FromID: callerID, ToID: calleeID, Kind: "CALLS"},
	})

	cases := []struct {
		name       string
		direction  string
		traceName  string // symbol to trace
		wantHops   int    // expected hop count after synonym/fallback mapping
		warnSubstr string
	}{
		// callers → inbound: tracing Callee inbound finds Caller.
		{"callers maps to inbound", "callers", "Callee", 1, `direction="callers"`},
		// callees → outbound: tracing Caller outbound finds Callee.
		{"callees maps to outbound", "callees", "Caller", 1, `direction="callees"`},
		// garbage → both: tracing Caller "both" still finds Callee.
		{"garbage falls back to both", "sideways", "Caller", 1, `direction="sideways"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
				"name":      c.traceName,
				"direction": c.direction,
				"depth":     1,
			}))
			if err != nil {
				t.Fatalf("handleTrace: %v", err)
			}
			if res.IsError {
				t.Fatalf("trace returned IsError: %s", textOf(t, res))
			}
			body := decode(t, res)

			total, _ := body["total"].(float64)
			if int(total) != c.wantHops {
				t.Errorf("direction=%q: total=%d, want %d — invalid direction must still produce hops, not a silent 0 (response: %s)",
					c.direction, int(total), c.wantHops, textOf(t, res))
			}

			meta, _ := body["_meta"].(map[string]any)
			warnings, _ := meta["warnings"].([]any)
			found := false
			for _, w := range warnings {
				if s, ok := w.(string); ok && strings.Contains(s, c.warnSubstr) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("direction=%q: expected _meta.warnings to mention %q; got: %v",
					c.direction, c.warnSubstr, warnings)
			}
		})
	}
}

// TestTrace_NameNotFoundCarriesNextSteps confirms the failure-as-pedagogy
// envelope on the name-not-found path — search remediation is surfaced,
// not just a bare error string.
func TestTrace_NameNotFoundCarriesNextSteps(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{ID: "p", Path: "/tmp/p", Name: "p", EdgeCount: 1})
	srv.sessionID = "p"

	res, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name": "NopeThisDoesNotExist",
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true on not-found, got: %s", textOf(t, res))
	}

	// Body is now JSON-shaped, not bare text.
	var body map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &body); err != nil {
		t.Fatalf("expected JSON-shaped error body; unmarshal failed: %v\nraw: %s", err, textOf(t, res))
	}
	if errStr, _ := body["error"].(string); !strings.Contains(errStr, "not found") {
		t.Errorf("expected error field to mention 'not found'; got: %v", body["error"])
	}
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("expected _meta envelope on rich-error response")
	}
	steps, _ := meta["next_steps"].([]any)
	if len(steps) == 0 {
		t.Fatalf("expected at least one _meta.next_steps entry on not-found error")
	}
	first, _ := steps[0].(map[string]any)
	if first["tool"] != "search" {
		t.Errorf("first next_step should be search; got: %v", first)
	}
	// args should mention the failed name so the caller can copy-paste
	// the remediation directly.
	argsStr, _ := first["args"].(string)
	if !strings.Contains(argsStr, "NopeThisDoesNotExist") {
		t.Errorf("expected first next_step args to contain the failed name; got: %v", argsStr)
	}
}

// TestSymbol_NotFoundCarriesNextSteps mirrors the trace coverage on
// handleSymbol — the same shape of failure should produce the same
// shape of remediation envelope.
func TestSymbol_NotFoundCarriesNextSteps(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	res, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id": "fake/path.go::pkg.MissingThing#Function",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true; got: %s", textOf(t, res))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &body); err != nil {
		t.Fatalf("expected JSON-shaped error body; got: %s", textOf(t, res))
	}
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("missing _meta on rich-error response")
	}
	steps, _ := meta["next_steps"].([]any)
	if len(steps) == 0 {
		t.Fatal("expected next_steps to be non-empty")
	}
	first, _ := steps[0].(map[string]any)
	if first["tool"] != "search" {
		t.Errorf("first next_step tool = %v, want 'search'", first["tool"])
	}
	// Short name should be extracted from the qualified name in the ID.
	if argsStr, _ := first["args"].(string); !strings.Contains(argsStr, "MissingThing") {
		t.Errorf("expected next_step args to contain extracted short name 'MissingThing'; got: %v", argsStr)
	}
}

// TestShortNameFromID covers the helper used to build the remediation
// hint. The qualified-name parser must survive edge cases (no prefix,
// pointer receiver in qn, missing kind suffix).
func TestShortNameFromID(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"internal/db/db.go::db.Open#Function", "Open"},
		{"internal/server/server.go::server.*Server.handleTrace#Method", "handleTrace"},
		{"x.go::Loose#Function", "Loose"}, // no dotted prefix
		{"weird-no-double-colon", "weird-no-double-colon"},
		{"x.go::pkg.Name", "Name"},  // missing #kind suffix
		{"x.go::A.B.C.D#Function", "D"},
	}
	for _, c := range cases {
		got := shortNameFromID(c.in)
		if got != c.want {
			t.Errorf("shortNameFromID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
