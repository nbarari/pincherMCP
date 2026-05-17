package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1232: silent-cross-project default flipped to strict-error for
// handleSymbol. Pre-fix, when projectArg was omitted AND the ID
// existed only in some other indexed project (mirror, staging,
// snapshot), the handler returned that other project's row with a
// warning string. Agents that don't parse warnings consumed
// cross-project data with no programmatic signal — silent-
// confidently-wrong family (#935 / #1217).

// Positive (the new strict-error path): two projects, ID lives in
// project B only, session=A → rich-error mentioning B's project_id
// and naming the next_steps remediations.
func TestHandleSymbol_CrossProject_StrictErrorByDefault(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	// Symbol exists ONLY in project B.
	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.X#Function", ProjectID: "proj-B",
		FilePath: "x.go", Name: "X", QualifiedName: "pkg.X",
		Kind: "Function", Language: "Go",
		Signature: "// from project B mirror",
	}})

	res, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.X#Function",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true on silent-cross-project request; got IsError=false")
	}
	body := decode(t, res)
	errMsg, _ := body["error"].(string)
	if !strings.Contains(errMsg, "proj-B") {
		t.Errorf("error message must name the project where the symbol actually lives; got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "cross_project=true") {
		t.Errorf("error message must mention the opt-in flag so the caller can recover; got: %s", errMsg)
	}
	// Verify next_steps proposes BOTH fix actions: re-issue with
	// project=found-in, AND search-by-name in session project.
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("expected _meta on rich-error envelope")
	}
	steps, _ := meta["next_steps"].([]any)
	if len(steps) < 2 {
		t.Fatalf("next_steps must offer at least 2 fix actions; got %d", len(steps))
	}
}

// Positive opt-in: same scenario + cross_project=true → returns the
// cross-project row (legacy behaviour preserved for opted-in callers).
// Warning must still fire so the opt-in caller sees the programmatic
// signal that the lookup crossed projects.
func TestHandleSymbol_CrossProject_OptInReturnsLegacyShape(t *testing.T) {
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

	res, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id":            "x.go::pkg.X#Function",
		"cross_project": true,
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if res.IsError {
		t.Fatalf("cross_project=true must NOT error; got IsError=true")
	}
	body := decode(t, res)
	if got, _ := body["signature"].(string); got != "// from project B mirror" {
		t.Errorf("expected project B's signature returned; got %q", got)
	}
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	if len(warnings) == 0 {
		t.Errorf("cross_project=true must still emit a warning so the opt-in caller has a programmatic signal of cross-project resolution; got no warnings")
	}
}

// Negative: ID lives in the session project → no error, no warning,
// straightforward data return. The strict guard must not fire for
// the in-session-project case.
func TestHandleSymbol_CrossProject_SessionProjectHit_NoStrictError(t *testing.T) {
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

	res, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.X#Function",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if res.IsError {
		t.Fatalf("in-session lookup must not error; got IsError=true")
	}
	body := decode(t, res)
	if got, _ := body["signature"].(string); got != "// from project A (session)" {
		t.Errorf("expected session project's signature; got %q", got)
	}
}

// Cross-check: explicit project= arg bypasses the strict check. A
// caller who explicitly asks for project=other has already made the
// choice the strict guard would have forced — no rich-error.
func TestHandleSymbol_CrossProject_ExplicitProjectBypassesStrict(t *testing.T) {
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

	res, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id":      "x.go::pkg.X#Function",
		"project": "projB",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if res.IsError {
		t.Fatalf("explicit project=projB must bypass the strict guard; got IsError=true")
	}
}

// Cross-check: project="*" (the documented cross-project sentinel)
// bypasses the strict check. Callers passing "*" have asked for
// global lookup deliberately — applying the strict guard would
// double-restrict them.
func TestHandleSymbol_CrossProject_StarSentinelBypassesStrict(t *testing.T) {
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

	res, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id":      "x.go::pkg.X#Function",
		"project": "*",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if res.IsError {
		t.Fatalf("project=\"*\" must bypass the strict guard; got IsError=true")
	}
}

// Control: with NO session set, the strict guard cannot fire (the
// session-vs-found-project comparison has nothing to compare).
// Unscoped lookup preserves pre-#1232 behaviour for callers that
// don't establish a session at all (HTTP CLI users, scripts).
func TestHandleSymbol_CrossProject_NoSessionMeansNoStrictGuard(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	// Deliberately NOT setting srv.sessionID — emulates HTTP CLI
	// path where no session was established.

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.X#Function", ProjectID: "proj-B",
		FilePath: "x.go", Name: "X", QualifiedName: "pkg.X",
		Kind: "Function", Language: "Go",
		Signature: "// from project B mirror",
	}})

	res, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.X#Function",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if res.IsError {
		t.Fatalf("no-session unscoped lookup must not error; got IsError=true")
	}
}

// TestHandleSymbol_DuplicateIDInTwoProjects_PrefersSession (#1408)
// regression guard. Pre-fix `s.store.GetSymbol(id)` was unscoped and
// could return the FORK's row when the same ID lived in both the
// session project AND a fork (e.g. d:\codex\sniffer mirroring
// pincher-repo). The strict-cross-project guard then erroneously
// errored with "exists only in project X" — even though the symbol
// DID also live in the session project, which is what the agent's
// search→symbol workflow actually wanted.
func TestHandleSymbol_DuplicateIDInTwoProjects_PrefersSession(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	// Same ID in BOTH projects — fork shape.
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "x.go::pkg.X#Function", ProjectID: "proj-A",
			FilePath: "x.go", Name: "X", QualifiedName: "pkg.X",
			Kind: "Function", Language: "Go",
			Signature: "// session-project (proj-A) row"},
		{ID: "x.go::pkg.X#Function", ProjectID: "proj-B",
			FilePath: "x.go", Name: "X", QualifiedName: "pkg.X",
			Kind: "Function", Language: "Go",
			Signature: "// fork (proj-B) row"},
	})

	res, err := srv.handleSymbol(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.X#Function",
	}))
	if err != nil {
		t.Fatalf("handleSymbol: %v", err)
	}
	if res.IsError {
		body := decode(t, res)
		t.Fatalf("session-hit must not error even when fork holds same ID; got IsError=true, body=%v", body)
	}
}
