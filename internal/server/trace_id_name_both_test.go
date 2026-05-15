package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1036: trace's schema documents `id` and `name` as mutually exclusive
// ("id wins"), but pre-fix passing both produced a silent precedence
// with no signal. An agent that included both (e.g. by templating from
// a search result that has the id AND passing the short name in
// parallel) couldn't tell which one was honored — the trace returned
// what felt like name-resolution but was actually id-resolution.

func TestHandleTrace_BothIDAndName_WarnsIDWins(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-trace-both"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::pkg.Alpha#Function", ProjectID: pid, FilePath: "a.go",
			Name: "Alpha", QualifiedName: "pkg.Alpha", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		{ID: pid + "::pkg.Bravo#Function", ProjectID: pid, FilePath: "b.go",
			Name: "Bravo", QualifiedName: "pkg.Bravo", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"id":        pid + "::pkg.Alpha#Function",
		"name":      "Bravo", // ignored per "id wins"
		"direction": "inbound",
		"depth":     1,
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, result)

	// `id` should have won — root should be Alpha, not Bravo.
	root, _ := body["root"].(string)
	if !strings.Contains(root, "Alpha") {
		t.Errorf("expected id-resolved root (Alpha); got root=%q", root)
	}

	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	foundWarn := false
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "both `id` and `name`") && strings.Contains(s, "Bravo") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("expected warning naming the dropped name arg; got warnings=%v", warnings)
	}
}

// Control: passing only id (no name) must NOT trip the warning.
func TestHandleTrace_OnlyID_NoIDNameWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	pid := "p-trace-onlyid"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: pid + "::pkg.Solo#Function", ProjectID: pid, FilePath: "a.go",
			Name: "Solo", QualifiedName: "pkg.Solo", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"id":        pid + "::pkg.Solo#Function",
		"direction": "inbound",
		"depth":     1,
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	warnings, _ := meta["warnings"].([]any)
	for _, w := range warnings {
		s, _ := w.(string)
		if strings.Contains(s, "both `id` and `name`") {
			t.Errorf("must not warn when only id is passed; got %s", s)
		}
	}
}
