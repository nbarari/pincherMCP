package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1232 (context arm): silent-cross-project default flipped to
// strict-error. context is the more dangerous shape than symbol —
// EdgesFrom calls walk the leaked project's graph so callees + imports
// also belong to the wrong tree. Mirrors the symbol-arm coverage in
// symbol_cross_project_strict_test.go.

// Positive: rich-error path naming the found-in project + the opt-in
// remediation.
func TestHandleContext_CrossProject_StrictErrorByDefault(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.X#Function", ProjectID: "proj-B",
		FilePath: "x.go", Name: "X", QualifiedName: "pkg.X",
		Kind: "Function", Language: "Go",
		Signature: "// from project B mirror",
	}})

	res, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.X#Function",
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true on silent-cross-project request; got false")
	}
	body := decode(t, res)
	errMsg, _ := body["error"].(string)
	if !strings.Contains(errMsg, "proj-B") {
		t.Errorf("error must name the project where the symbol lives; got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "cross_project=true") {
		t.Errorf("error must mention the opt-in flag; got: %s", errMsg)
	}
	// context's error must also mention callees + imports specifically —
	// that's the difference vs symbol: silently returning context data
	// from the wrong project also leaks callees + imports from that tree.
	if !strings.Contains(errMsg, "callees") || !strings.Contains(errMsg, "imports") {
		t.Errorf("context error must specifically mention callees + imports (context's extra hazard vs symbol); got: %s", errMsg)
	}
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("expected _meta on rich-error envelope")
	}
	steps, _ := meta["next_steps"].([]any)
	if len(steps) < 2 {
		t.Fatalf("next_steps must offer at least 2 fix actions; got %d", len(steps))
	}
}

// Positive opt-in: cross_project=true returns the other project's
// data (legacy shape) and emits the warning so the opt-in caller has
// a programmatic signal.
func TestHandleContext_CrossProject_OptInReturnsLegacyShape(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.X#Function", ProjectID: "proj-B",
		FilePath: "x.go", Name: "X", QualifiedName: "pkg.X",
		Kind: "Function", Language: "Go",
		Signature: "// from project B mirror",
	}})

	res, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id":            "x.go::pkg.X#Function",
		"cross_project": true,
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	if res.IsError {
		t.Fatalf("cross_project=true must NOT error; got IsError=true")
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	if len(warnings) == 0 {
		t.Errorf("cross_project=true must still emit a warning so the opt-in caller has a programmatic signal; got no warnings")
	}
	saw := false
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "proj-B") && strings.Contains(s, "callees") {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("opt-in warning must name the cross-project AND warn about callee/import leak; got %v", warnings)
	}
}

// Negative: in-session lookup, no strict guard fires.
func TestHandleContext_CrossProject_SessionProjectHit_NoStrictError(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.X#Function", ProjectID: "proj-A",
		FilePath: "x.go", Name: "X", QualifiedName: "pkg.X",
		Kind: "Function", Language: "Go",
		Signature: "// from project A (session)",
	}})

	res, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.X#Function",
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	if res.IsError {
		t.Fatalf("in-session lookup must not error; got IsError=true")
	}
}

// Cross-check: explicit project= arg bypasses the strict guard. Same
// shape as the symbol-arm equivalent — the caller already chose, no
// silent fallback risk.
func TestHandleContext_CrossProject_ExplicitProjectBypassesStrict(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.X#Function", ProjectID: "proj-B",
		FilePath: "x.go", Name: "X", QualifiedName: "pkg.X",
		Kind: "Function", Language: "Go",
	}})

	res, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id":      "x.go::pkg.X#Function",
		"project": "projB",
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	if res.IsError {
		t.Fatalf("explicit project=projB must bypass strict guard; got IsError=true")
	}
}

// Cross-check: project="*" sentinel bypasses the strict guard.
func TestHandleContext_CrossProject_StarSentinelBypassesStrict(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.X#Function", ProjectID: "proj-B",
		FilePath: "x.go", Name: "X", QualifiedName: "pkg.X",
		Kind: "Function", Language: "Go",
	}})

	res, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id":      "x.go::pkg.X#Function",
		"project": "*",
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	if res.IsError {
		t.Fatalf("project=\"*\" must bypass strict guard; got IsError=true")
	}
}

// Control: no session set → no strict guard (the comparison has
// nothing to compare). Preserves pre-#1232 behaviour for HTTP CLI /
// no-session paths.
func TestHandleContext_CrossProject_NoSessionMeansNoStrictGuard(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	// Deliberately NOT setting srv.sessionID.

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.X#Function", ProjectID: "proj-B",
		FilePath: "x.go", Name: "X", QualifiedName: "pkg.X",
		Kind: "Function", Language: "Go",
	}})

	res, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.X#Function",
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	if res.IsError {
		t.Fatalf("no-session unscoped lookup must not error; got IsError=true")
	}
}

// TestHandleContext_DuplicateIDInTwoProjects_PrefersSession (#1408)
// regression guard. Pre-fix `s.store.GetSymbol(id)` was unscoped and
// could return the FORK's row when the same ID lived in both the
// session project AND a fork (e.g. d:\codex\sniffer mirroring
// pincher-repo). The downstream strict-guard then errored with
// "exists only in project X" — even though the symbol DID also live
// in the session project. Fix: scope to session first when no
// project= arg, fall back to unscoped only if session miss.
func TestHandleContext_DuplicateIDInTwoProjects_PrefersSession(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	// Same ID in BOTH projects — fork shape. Distinct signatures so
	// we can verify which project's row came back.
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "x.go::pkg.X#Function", ProjectID: "proj-A",
			FilePath: "x.go", Name: "X", QualifiedName: "pkg.X",
			Kind: "Function", Language: "Go",
			Signature: "// session-project (proj-A) row — MUST be returned"},
		{ID: "x.go::pkg.X#Function", ProjectID: "proj-B",
			FilePath: "x.go", Name: "X", QualifiedName: "pkg.X",
			Kind: "Function", Language: "Go",
			Signature: "// fork (proj-B) row — must NOT be preferred"},
	})

	res, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.X#Function",
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	// Primary assertion: with the fix, the session-scoped lookup
	// returns proj-A's row and the strict-cross-project guard
	// (s.sessionID != sym.ProjectID) doesn't fire. Pre-fix the
	// unscoped lookup could pick proj-B's row, then the strict
	// guard erroneously errored with "exists only in proj-B".
	if res.IsError {
		body := decode(t, res)
		t.Fatalf("session-hit must not error even when fork holds same ID; got IsError=true, body=%v", body)
	}
}
