package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1145 part 2: context flags the #858 edge-graph-empty condition
// when callees + imports are both empty AND the dominant language
// has no cross-file edge resolution. Pre-fix, a TypeScript user
// calling context on a function got callees=[] + imports=[] and
// read it as "this function calls nothing" — when the resolver
// never ran for the language.

func TestHandleContext_EdgeGraphEmpty_FlagsLanguage(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{
		ID: "ts_ctx", Path: t.TempDir(), Name: "ts_ctx",
		IndexedAt: time.Now(),
	})
	srv.sessionID = "ts_ctx"
	// One symbol, no edges — edge-graph-empty + TypeScript dominant.
	id := "src/auth.ts::auth.login#Function"
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: id, ProjectID: "ts_ctx", Name: "login", QualifiedName: "auth.login",
			Kind: "Function", FilePath: "src/auth.ts", Language: "TypeScript"},
	})

	res, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id": id,
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
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

// Control: Go project — no warning (Go has edge resolution; empty
// callees is a real "leaf" finding).
func TestHandleContext_GoProjectEmptyCallees_NoEdgeGapWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{
		ID: "go_ctx", Path: t.TempDir(), Name: "go_ctx",
		IndexedAt: time.Now(),
	})
	srv.sessionID = "go_ctx"
	id := "pkg/auth.go::auth.Login#Function"
	store.BulkUpsertSymbols([]db.Symbol{
		{ID: id, ProjectID: "go_ctx", Name: "Login", QualifiedName: "auth.Login",
			Kind: "Function", FilePath: "pkg/auth.go", Language: "Go"},
	})

	res, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id": id,
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		if s, ok := w.(string); ok && strings.Contains(s, "#858") {
			t.Errorf("Go project must not trip #858 warning; got: %v", s)
		}
	}
}
