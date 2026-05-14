package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// dead_code: Function with no inbound CALLS and not exported/test/entry
// must surface; a sibling Function with one inbound caller must NOT.
func TestHandleDeadCode_BasicReachability(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()})

	mustUpsertSymbols(t, store, []db.Symbol{
		// Lonely Function — should surface as dead.
		{ID: "p1::pkg.lonely#Function", ProjectID: "p1", FilePath: "internal/svc/svc.go",
			Name: "lonely", QualifiedName: "pkg.lonely", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		// Reached Function — has 1 inbound CALLS edge.
		{ID: "p1::pkg.reached#Function", ProjectID: "p1", FilePath: "internal/svc/svc.go",
			Name: "reached", QualifiedName: "pkg.reached", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		// Caller — provides the edge to Reached.
		{ID: "p1::pkg.caller#Function", ProjectID: "p1", FilePath: "internal/svc/svc.go",
			Name: "caller", QualifiedName: "pkg.caller", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})
	if err := store.BulkUpsertEdges([]db.Edge{
		{ProjectID: "p1", FromID: "p1::pkg.caller#Function", ToID: "p1::pkg.reached#Function",
			Kind: "CALLS", Confidence: 1},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := srv.handleDeadCode(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleDeadCode: %v", err)
	}
	body := decode(t, result)
	dead, _ := body["dead_symbols"].([]any)

	got := map[string]bool{}
	for _, d := range dead {
		entry, _ := d.(map[string]any)
		if name, ok := entry["name"].(string); ok {
			got[name] = true
		}
	}
	if !got["lonely"] {
		t.Errorf("expected 'lonely' in dead-code result; got %v", got)
	}
	if got["reached"] {
		t.Errorf("'reached' has an inbound CALLS edge; should NOT be dead; got %v", got)
	}
	if got["caller"] {
		// caller has 0 inbound but it's the source of the edge to reached;
		// still expected to surface unless something else excludes it.
		// Permitted — 'caller' is genuinely unreachable from outside.
	}
}

// Exported + entry_point + test functions are never reported as dead,
// regardless of inbound edges.
func TestHandleDeadCode_ExcludesExportedEntryAndTest(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()})

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p1::pkg.PublicAPI#Function", ProjectID: "p1", FilePath: "internal/svc/svc.go",
			Name: "PublicAPI", QualifiedName: "pkg.PublicAPI", Kind: "Function", Language: "Go",
			IsExported: true, ExtractionConfidence: 1.0},
		{ID: "p1::main.main#Function", ProjectID: "p1", FilePath: "cmd/foo/main.go",
			Name: "main", QualifiedName: "main.main", Kind: "Function", Language: "Go",
			IsEntryPoint: true, ExtractionConfidence: 1.0},
		{ID: "p1::pkg.TestFoo#Function", ProjectID: "p1", FilePath: "internal/svc/svc_test.go",
			Name: "TestFoo", QualifiedName: "pkg.TestFoo", Kind: "Function", Language: "Go",
			IsTest: true, ExtractionConfidence: 1.0},
	})

	result, err := srv.handleDeadCode(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleDeadCode: %v", err)
	}
	body := decode(t, result)
	dead, _ := body["dead_symbols"].([]any)

	for _, d := range dead {
		entry, _ := d.(map[string]any)
		name, _ := entry["name"].(string)
		switch name {
		case "PublicAPI", "main", "TestFoo":
			t.Errorf("dead-code result includes %q which should be excluded by IsExported/IsEntryPoint/IsTest filters: %v", name, entry)
		}
	}
}

// Default min_confidence (0.95) excludes regex-tier symbols.
// Dropping the floor surfaces them.
func TestHandleDeadCode_MinConfidenceFilter(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()})

	mustUpsertSymbols(t, store, []db.Symbol{
		// Regex-tier confidence (0.85) — excluded by default 0.95 floor.
		{ID: "p1::pkg.lowConf#Function", ProjectID: "p1", FilePath: "scripts/foo.py",
			Name: "lowConf", QualifiedName: "pkg.lowConf", Kind: "Function", Language: "Python",
			ExtractionConfidence: 0.85},
	})

	// Default — should NOT include lowConf (0.85 < 0.95).
	result, err := srv.handleDeadCode(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	body := decode(t, result)
	dead, _ := body["dead_symbols"].([]any)
	if len(dead) != 0 {
		t.Errorf("default min_confidence=0.95 should exclude 0.85-tier symbols; got %v", dead)
	}

	// Drop floor to 0.0 — should include it.
	result, err = srv.handleDeadCode(context.Background(), makeReq(map[string]any{
		"min_confidence": 0.0,
	}))
	if err != nil {
		t.Fatal(err)
	}
	body = decode(t, result)
	dead, _ = body["dead_symbols"].([]any)
	if len(dead) != 1 {
		t.Errorf("min_confidence=0.0 should include lowConf; got %v", dead)
	}
}

// kinds filter: only requested kinds appear; default Function+Method.
func TestHandleDeadCode_KindsFilter(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()})

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p1::pkg.lonelyFn#Function", ProjectID: "p1", FilePath: "svc.go",
			Name: "lonelyFn", QualifiedName: "pkg.lonelyFn", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		{ID: "p1::pkg.lonelyClass#Class", ProjectID: "p1", FilePath: "svc.go",
			Name: "lonelyClass", QualifiedName: "pkg.lonelyClass", Kind: "Class", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	// Default kinds (Function, Method) — Class must NOT appear.
	result, _ := srv.handleDeadCode(context.Background(), makeReq(map[string]any{}))
	body := decode(t, result)
	dead, _ := body["dead_symbols"].([]any)
	for _, d := range dead {
		entry, _ := d.(map[string]any)
		if entry["kind"] == "Class" {
			t.Errorf("default kinds=Function,Method should exclude Class; got %v", entry)
		}
	}

	// Explicit kinds=Class — only Class.
	result, _ = srv.handleDeadCode(context.Background(), makeReq(map[string]any{
		"kinds": "Class",
	}))
	body = decode(t, result)
	dead, _ = body["dead_symbols"].([]any)
	if len(dead) != 1 {
		t.Fatalf("kinds=Class should return exactly the Class; got %v", dead)
	}
	entry, _ := dead[0].(map[string]any)
	if entry["kind"] != "Class" {
		t.Errorf("kinds=Class returned non-Class %v", entry)
	}
}

// #738: filters.kinds must be a JSON array, never null. A nil []string
// marshals to `null`, and consumers iterating filters.kinds without a
// null-check break — the recurring nil-slice-in-response class. The
// echoed filters.language already defaults to "" (not null); keep
// filters.kinds consistent.
func TestHandleDeadCode_FiltersKindsNeverNull(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()})

	// No kinds arg passed — pre-#738 filters.kinds came back as null.
	result, _ := srv.handleDeadCode(context.Background(), makeReq(map[string]any{}))
	body := decode(t, result)
	filters, ok := body["filters"].(map[string]any)
	if !ok {
		t.Fatalf("filters block missing or wrong type: %v", body["filters"])
	}
	kinds, present := filters["kinds"]
	if !present {
		t.Fatal("filters.kinds key missing")
	}
	if kinds == nil {
		t.Errorf("filters.kinds = null; want [] (nil slice marshals to null — JSON invariant)")
	}
	if _, isArray := kinds.([]any); !isArray {
		t.Errorf("filters.kinds = %T (%v); want a JSON array", kinds, kinds)
	}
}

// Developer scratch paths (scratch_*.go) are post-filtered — they're
// known-dead noise the developer doesn't need to be told about.
func TestHandleDeadCode_ExcludesScratchPaths(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()})

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p1::pkg.realDead#Function", ProjectID: "p1", FilePath: "internal/svc/svc.go",
			Name: "realDead", QualifiedName: "pkg.realDead", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		{ID: "p1::pkg.scratchDead#Function", ProjectID: "p1", FilePath: "scratch_foo.go",
			Name: "scratchDead", QualifiedName: "pkg.scratchDead", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, _ := srv.handleDeadCode(context.Background(), makeReq(map[string]any{}))
	body := decode(t, result)
	dead, _ := body["dead_symbols"].([]any)
	got := map[string]bool{}
	for _, d := range dead {
		entry, _ := d.(map[string]any)
		if name, ok := entry["name"].(string); ok {
			got[name] = true
		}
	}
	if !got["realDead"] {
		t.Errorf("realDead should appear; got %v", got)
	}
	if got["scratchDead"] {
		t.Errorf("scratchDead in scratch_foo.go should be filtered; got %v", got)
	}
}

// testdata/ fixtures (#393) are post-filtered from dead-code results
// alongside developer scratch paths. Fixture inputs aren't real code,
// so calling them "dead" is misleading.
func TestHandleDeadCode_ExcludesTestFixturePaths(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()})

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p1::pkg.realDead#Function", ProjectID: "p1", FilePath: "internal/svc/svc.go",
			Name: "realDead", QualifiedName: "pkg.realDead", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		{ID: "p1::corpus.helper#Function", ProjectID: "p1", FilePath: "testdata/corpus/foo/helper.go",
			Name: "helper", QualifiedName: "corpus.helper", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

	result, _ := srv.handleDeadCode(context.Background(), makeReq(map[string]any{}))
	body := decode(t, result)
	dead, _ := body["dead_symbols"].([]any)
	got := map[string]bool{}
	for _, d := range dead {
		entry, _ := d.(map[string]any)
		if name, ok := entry["name"].(string); ok {
			got[name] = true
		}
	}
	if !got["realDead"] {
		t.Errorf("realDead should appear; got %v", got)
	}
	if got["helper"] {
		t.Errorf("testdata/corpus/foo/helper.go fixture should be filtered; got %v", got)
	}
}

// Empty result: meta.diagnosis explains the empty, doesn't suggest
// next-step actions on a non-existent top dead symbol.
func TestHandleDeadCode_EmptyDiagnosis(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()})

	// No symbols seeded.
	result, _ := srv.handleDeadCode(context.Background(), makeReq(map[string]any{}))
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("_meta missing")
	}
	if _, hasDiag := meta["diagnosis"]; !hasDiag {
		t.Errorf("empty result should set _meta.diagnosis; got %v", meta)
	}
	// #712: an empty dead_code result now carries a next_steps hint —
	// failure-as-pedagogy. The right next move when nothing surfaced at
	// the default 0.95 floor is to LOWER min_confidence so regex-extracted
	// (sub-1.0) symbols enter the candidate pool. (The pre-#712 test
	// asserted next_steps should be absent; that was the wrong default —
	// a silent empty result with no remediation is exactly the
	// anti-pattern the v0.17 theme set out to kill.)
	steps, hasNext := meta["next_steps"].([]any)
	if !hasNext || len(steps) == 0 {
		t.Errorf("empty result should suggest next_steps (lower min_confidence); got %v", meta)
	}
	first, _ := steps[0].(map[string]any)
	if first["tool"] != "dead_code" {
		t.Errorf("first next_step should re-invoke dead_code with a lower floor; got %v", first)
	}
	if argsStr, _ := first["args"].(string); !strings.Contains(argsStr, "min_confidence") {
		t.Errorf("next_step args should lower min_confidence; got %v", argsStr)
	}
	// The diagnosis text must not tell the caller to "tighten" — that
	// raises the floor and surfaces FEWER candidates (the #712 inversion).
	diag, _ := meta["diagnosis"].(string)
	if strings.Contains(diag, "tighten") {
		t.Errorf("diagnosis must not say 'tighten' (inverted advice — #712); got: %q", diag)
	}
	if !strings.Contains(diag, "lower") {
		t.Errorf("diagnosis should advise lowering min_confidence; got: %q", diag)
	}
}
