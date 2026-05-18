package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1425 (neighborhood arm): scope-to-session-first when projectArg is
// empty. Pre-fix the no-project path went straight to unscoped
// GetSymbol — when the seed ID existed in BOTH the session project
// and a fork (e.g. d:\codex\sniffer mirror of pincher-repo),
// GetSymbol returned whichever row the schema-driven ORDER hit
// first; the #1232 strict-cross-project guard then fired with
// "exists only in project X" — pointing the agent AWAY from the
// session project where the seed DOES also live, breaking the
// canonical search→neighborhood workflow.
//
// Identical shape to the handleSymbol fix from #1409 and the
// handleContext fix from the same PR. Neighborhood was missed
// because its lookup path is in a separate file from handleSymbol's
// switch — #1425 surfaced the gap during dogfood on a machine with
// pincher-repo + sniffer + pincherMCP all indexed (three rows for
// the same ID).

// Positive — seed ID is in BOTH session project AND another project.
// Without explicit project=, neighborhood must prefer the session
// project and NOT fire the strict-cross-project guard (because the
// seed IS in the session project — the fact that it also exists
// elsewhere is irrelevant once the scoped lookup succeeds).
func TestHandleNeighborhood_DuplicateIDInTwoProjects_PrefersSession(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	// Same symbol ID exists in BOTH projects (the dogfood shape:
	// fork + mainline mirror the same source tree). Seed in proj-A
	// (session) MUST be preferred over proj-B's row.
	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "x.go::pkg.Seed#Function", ProjectID: "proj-A",
			FilePath: "x.go", Name: "Seed", QualifiedName: "pkg.Seed",
			Kind: "Function", Language: "Go"},
		{ID: "x.go::pkg.Seed#Function", ProjectID: "proj-B",
			FilePath: "x.go", Name: "Seed", QualifiedName: "pkg.Seed",
			Kind: "Function", Language: "Go"},
		// Sibling in the session project so the neighbor list has
		// something to return — without a sibling the test would
		// pass trivially even on the pre-fix code path.
		{ID: "x.go::pkg.Sibling#Function", ProjectID: "proj-A",
			FilePath: "x.go", Name: "Sibling", QualifiedName: "pkg.Sibling",
			Kind: "Function", Language: "Go"},
	})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.Seed#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		body := decode(t, res)
		errMsg, _ := body["error"].(string)
		t.Fatalf("must not error when seed is in session project; got: %s", errMsg)
	}
	body := decode(t, res)
	// Cross-check: the neighbor we DID get came from proj-A (the
	// session project), not proj-B. Verifying via Sibling presence —
	// proj-A is the only project where Sibling exists.
	neighbors, _ := body["neighbors"].([]any)
	sawSibling := false
	for _, n := range neighbors {
		m, _ := n.(map[string]any)
		if name, _ := m["name"].(string); name == "Sibling" {
			sawSibling = true
		}
	}
	if !sawSibling {
		t.Errorf("neighborhood must return Sibling from session project (proj-A); got %v", neighbors)
	}
}

// Cross-check — the strict-cross-project guard STILL fires when the
// seed is genuinely NOT in the session project (regression guard
// for the existing #1232 behaviour).
func TestHandleNeighborhood_OnlyInOtherProject_StrictGuardStillFires(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	// Symbol ONLY in proj-B (session is proj-A). The strict guard
	// should still fire — the scope-to-session fix from #1425
	// targets the "exists in both" case only.
	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.SeedB#Function", ProjectID: "proj-B",
		FilePath: "x.go", Name: "SeedB", QualifiedName: "pkg.SeedB",
		Kind: "Function", Language: "Go",
	}})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.SeedB#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected strict guard to fire for seed only in other project; got no error")
	}
	body := decode(t, res)
	errMsg, _ := body["error"].(string)
	if !strings.Contains(errMsg, "proj-B") {
		t.Errorf("error must name the project where the seed lives; got: %s", errMsg)
	}
}
