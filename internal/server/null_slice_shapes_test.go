package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #332: trace/context/architecture endpoints must return [] (not null)
// for the empty-result branches of their main slice fields. Same JSON-
// shape class as #328 (health.extraction_coverage) and #330
// (changes.impacted/changed_symbols).

// trace with no hops → "hops":[] not "hops":null. Symbol exists but has
// no inbound or outbound CALLS edges, so the BFS produces zero hops.
func TestHandleTrace_NoHops_HopsIsEmptyArrayNotNull(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := "trace-null-test"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p::main.Lonely#Function", ProjectID: pid, FilePath: "main.go", Name: "Lonely",
			QualifiedName: "main.Lonely", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 30, StartLine: 1, EndLine: 3, ExtractionConfidence: 1.0},
	})
	// No edges seeded.

	result, err := srv.handleTrace(context.Background(), makeReq(map[string]any{
		"name":      "Lonely",
		"direction": "both",
		"depth":     2,
	}))
	if err != nil {
		t.Fatalf("handleTrace: %v", err)
	}
	body := decode(t, result)
	if v, present := body["hops"]; !present {
		t.Fatal("hops key missing from trace response")
	} else if v == nil {
		t.Errorf("hops is null; want [] (non-nil empty array)")
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), `"hops":null`) {
		t.Errorf("trace JSON contains \"hops\":null; want \"hops\":[]\nfull: %s", raw)
	}
}

// context on a symbol with no IMPORTS edges → "imports":[] not null.
func TestHandleContext_NoImports_ImportsIsEmptyArrayNotNull(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := "ctx-null-test"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p::main.Solo#Function", ProjectID: pid, FilePath: "main.go", Name: "Solo",
			QualifiedName: "main.Solo", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 30, StartLine: 1, EndLine: 3, ExtractionConfidence: 1.0},
	})
	// No IMPORTS edges seeded.

	result, err := srv.handleContext(context.Background(), makeReq(map[string]any{
		"id": "p::main.Solo#Function",
	}))
	if err != nil {
		t.Fatalf("handleContext: %v", err)
	}
	body := decode(t, result)
	if v, present := body["imports"]; !present {
		t.Fatal("imports key missing from context response")
	} else if v == nil {
		t.Errorf("imports is null; want [] (non-nil empty array)")
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), `"imports":null`) {
		t.Errorf("context JSON contains \"imports\":null; want \"imports\":[]\nfull: %s", raw)
	}
}

// architecture on an empty project → entry_points and hotspots are []
// not null. Project record exists but has no symbols.
func TestHandleArchitecture_EmptyProject_NullSlicesAreEmptyArrays(t *testing.T) {
	srv, store, _ := newTestServer(t)
	pid := "arch-null-test"
	store.UpsertProject(db.Project{ID: pid, Path: "/tmp/" + pid, Name: pid, IndexedAt: time.Now()})
	srv.sessionID = pid
	srv.sessionRoot = "/tmp/" + pid

	result, err := srv.handleArchitecture(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleArchitecture: %v", err)
	}
	body := decode(t, result)

	for _, key := range []string{"entry_points", "hotspots"} {
		v, present := body[key]
		if !present {
			t.Errorf("%s key missing from architecture response", key)
			continue
		}
		if v == nil {
			t.Errorf("%s is null; want [] (non-nil empty array)", key)
		}
	}
	raw, _ := json.Marshal(body)
	for _, bad := range []string{`"entry_points":null`, `"hotspots":null`} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("architecture JSON contains %s; want []\nfull: %s", bad, raw)
		}
	}
}
