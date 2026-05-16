package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1232 (neighborhood arm): silent-cross-project default flipped to
// strict-error. neighborhood is the most dangerous shape of the three
// (symbol / context / neighborhood) — it returns up to 500 in-file
// siblings, every one of them belonging to the cross-project tree if
// the seed resolved off-session. Mirrors the symbol-arm and
// context-arm coverage in the sibling test files.

// Positive: rich-error path naming the found-in project + the opt-in
// remediation. Verifies the error specifically calls out the in-file
// refactor hazard (the neighborhood-specific reason to default strict).
func TestHandleNeighborhood_CrossProject_StrictErrorByDefault(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.Seed#Function", ProjectID: "proj-B",
		FilePath: "x.go", Name: "Seed", QualifiedName: "pkg.Seed",
		Kind: "Function", Language: "Go",
	}})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.Seed#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true on silent-cross-project seed; got false")
	}
	body := decode(t, res)
	errMsg, _ := body["error"].(string)
	if !strings.Contains(errMsg, "proj-B") {
		t.Errorf("error must name the project where the seed lives; got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "cross_project=true") {
		t.Errorf("error must mention the opt-in flag; got: %s", errMsg)
	}
	// neighborhood-specific hazard: in-file refactor planning. The
	// error must call this out so the caller knows what they were
	// about to ship (an edit to the wrong file).
	if !strings.Contains(errMsg, "refactor") || !strings.Contains(errMsg, "in-file") {
		t.Errorf("neighborhood error must specifically call out the in-file refactor hazard (it's WHY the strict guard matters more here than symbol); got: %s", errMsg)
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

// Positive opt-in: cross_project=true returns the neighbor list from
// the other project + emits the warning. The warning must mention the
// downstream neighbor leak (every neighbor's file_path also belongs
// to the off-tree project).
func TestHandleNeighborhood_CrossProject_OptInReturnsLegacyShape(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "x.go::pkg.Seed#Function", ProjectID: "proj-B",
			FilePath: "x.go", Name: "Seed", QualifiedName: "pkg.Seed",
			Kind: "Function", Language: "Go"},
		{ID: "x.go::pkg.Sibling#Function", ProjectID: "proj-B",
			FilePath: "x.go", Name: "Sibling", QualifiedName: "pkg.Sibling",
			Kind: "Function", Language: "Go"},
	})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":            "x.go::pkg.Seed#Function",
		"cross_project": true,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		t.Fatalf("cross_project=true must NOT error; got IsError=true")
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	if len(warnings) == 0 {
		t.Errorf("cross_project=true must still emit a warning; got no warnings")
	}
	saw := false
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "proj-B") && strings.Contains(s, "neighbor list") {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("opt-in warning must name the cross-project AND warn about the neighbor list belonging to the off-tree project; got %v", warnings)
	}
}

// Negative: in-session seed → no error.
func TestHandleNeighborhood_CrossProject_SessionProjectHit_NoStrictError(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.Seed#Function", ProjectID: "proj-A",
		FilePath: "x.go", Name: "Seed", QualifiedName: "pkg.Seed",
		Kind: "Function", Language: "Go",
	}})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.Seed#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		t.Fatalf("in-session seed must not error; got IsError=true")
	}
}

// Cross-check: explicit project= arg bypasses the strict guard.
func TestHandleNeighborhood_CrossProject_ExplicitProjectBypassesStrict(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.Seed#Function", ProjectID: "proj-B",
		FilePath: "x.go", Name: "Seed", QualifiedName: "pkg.Seed",
		Kind: "Function", Language: "Go",
	}})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":      "x.go::pkg.Seed#Function",
		"project": "projB",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		t.Fatalf("explicit project=projB must bypass strict guard; got IsError=true")
	}
}

// Cross-check: project="*" sentinel bypasses the strict guard.
func TestHandleNeighborhood_CrossProject_StarSentinelBypassesStrict(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-A", "/tmp/A", "projA")
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	srv.sessionID = "proj-A"
	srv.sessionRoot = "/tmp/A"

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.Seed#Function", ProjectID: "proj-B",
		FilePath: "x.go", Name: "Seed", QualifiedName: "pkg.Seed",
		Kind: "Function", Language: "Go",
	}})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id":      "x.go::pkg.Seed#Function",
		"project": "*",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		t.Fatalf("project=\"*\" must bypass strict guard; got IsError=true")
	}
}

// Control: no session set → no strict guard.
func TestHandleNeighborhood_CrossProject_NoSessionMeansNoStrictGuard(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	mustUpsertProject(t, store, "proj-B", "/tmp/B", "projB")
	// Deliberately NOT setting srv.sessionID.

	mustUpsertSymbols(t, store, []db.Symbol{{
		ID: "x.go::pkg.Seed#Function", ProjectID: "proj-B",
		FilePath: "x.go", Name: "Seed", QualifiedName: "pkg.Seed",
		Kind: "Function", Language: "Go",
	}})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": "x.go::pkg.Seed#Function",
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	if res.IsError {
		t.Fatalf("no-session unscoped lookup must not error; got IsError=true")
	}
}
