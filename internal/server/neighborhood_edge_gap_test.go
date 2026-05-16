package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1145 (extends #858 honesty surface): neighborhood on a project
// whose edge graph is empty due to non-Go/Python dominant language
// must flag the cause. Without the warning, agents read the
// file-scope neighbor list as "this symbol has no graph-traversal
// neighbors" when really the graph itself is empty for the language.

func TestHandleNeighborhood_EdgeGraphEmpty_LangFlag(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{
		ID: "ts_proj", Path: t.TempDir(), Name: "ts_proj",
		IndexedAt: time.Now(), EdgeCount: 0,
	})
	srv.sessionID = "ts_proj"
	// Seed two TypeScript symbols in the same file — neighborhood
	// returns the sibling, edges=0 so the gap probe fires.
	seedID := "src/auth.ts::auth.login#Function"
	siblingID := "src/auth.ts::auth.logout#Function"
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: seedID, ProjectID: "ts_proj", Name: "login", QualifiedName: "auth.login",
			Kind: "Function", FilePath: "src/auth.ts", Language: "TypeScript"},
		{ID: siblingID, ProjectID: "ts_proj", Name: "logout", QualifiedName: "auth.logout",
			Kind: "Function", FilePath: "src/auth.ts", Language: "TypeScript"},
	})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": seedID,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)

	found := false
	for _, w := range warnings {
		if s, ok := w.(string); ok && strings.Contains(s, "TypeScript") && strings.Contains(s, "#858") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected #858 edge-graph-empty warning naming TypeScript; got: %v", warnings)
	}
}

// Control: Go project (has edge resolution) — no warning even when
// the seed has no actual edges populated yet.
func TestHandleNeighborhood_GoProject_NoEdgeGapWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{
		ID: "go_proj", Path: t.TempDir(), Name: "go_proj",
		IndexedAt: time.Now(),
	})
	srv.sessionID = "go_proj"
	seedID := "pkg/auth.go::auth.Login#Function"
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: seedID, ProjectID: "go_proj", Name: "Login", QualifiedName: "auth.Login",
			Kind: "Function", FilePath: "pkg/auth.go", Language: "Go"},
	})

	res, err := srv.handleNeighborhood(context.Background(), makeReq(map[string]any{
		"id": seedID,
	}))
	if err != nil {
		t.Fatalf("handleNeighborhood: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		if s, ok := w.(string); ok && strings.Contains(s, "#858") {
			t.Errorf("Go project must not trip #858 edge-coverage warning; got: %v", s)
		}
	}
	_ = json.Marshal // silence unused import
}
