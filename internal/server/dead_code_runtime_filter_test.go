package server

import (
	"context"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

func TestIsRuntimeInvokedGoSymbol_Init(t *testing.T) {
	if !isRuntimeInvokedGoSymbol("Go", "init") {
		t.Errorf("Go init() must be runtime-invoked")
	}
}

func TestIsRuntimeInvokedGoSymbol_TestMain(t *testing.T) {
	if !isRuntimeInvokedGoSymbol("Go", "TestMain") {
		t.Errorf("Go TestMain must be runtime-invoked")
	}
}

func TestIsRuntimeInvokedGoSymbol_MainFunction(t *testing.T) {
	if !isRuntimeInvokedGoSymbol("Go", "main") {
		t.Errorf("Go main must be runtime-invoked")
	}
}

func TestIsRuntimeInvokedGoSymbol_OtherLanguageInit(t *testing.T) {
	if isRuntimeInvokedGoSymbol("Python", "init") {
		t.Errorf("Python init is not runtime-invoked in the Go sense")
	}
	if isRuntimeInvokedGoSymbol("JavaScript", "main") {
		t.Errorf("JS main is just a function name; filter should be Go-only")
	}
}

func TestIsRuntimeInvokedGoSymbol_OrdinaryFunction(t *testing.T) {
	if isRuntimeInvokedGoSymbol("Go", "doStuff") {
		t.Errorf("ordinary Go function must not match the runtime filter")
	}
}

// End-to-end: handleDeadCode must NOT return Go init / TestMain / main
// even though the static graph has no inbound edges for them.
func TestHandleDeadCode_FiltersGoRuntimeInvoked(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.sessionID = "p1"
	store.UpsertProject(db.Project{ID: "p1", Path: "/tmp/p1", Name: "p1", IndexedAt: time.Now()})

	mustUpsertSymbols(t, store, []db.Symbol{
		{ID: "p1::pkg.init#Function", ProjectID: "p1", FilePath: "internal/svc/svc.go",
			Name: "init", QualifiedName: "pkg.init", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		{ID: "p1::pkg.TestMain#Function", ProjectID: "p1", FilePath: "internal/svc/svc_main_test.go",
			Name: "TestMain", QualifiedName: "pkg.TestMain", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
		// A legitimate dead function to confirm the handler still works.
		{ID: "p1::pkg.unreached#Function", ProjectID: "p1", FilePath: "internal/svc/svc.go",
			Name: "unreached", QualifiedName: "pkg.unreached", Kind: "Function", Language: "Go",
			ExtractionConfidence: 1.0},
	})

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
	if got["init"] {
		t.Errorf("Go init() should not appear in dead_code (runtime-invoked, #492)")
	}
	if got["TestMain"] {
		t.Errorf("Go TestMain should not appear in dead_code (runtime-invoked, #492)")
	}
	if !got["unreached"] {
		t.Errorf("legitimate dead symbol 'unreached' must still surface; got %v", got)
	}
}
