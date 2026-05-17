package index

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1164 coverage push: emitEvent's "callback is set" branch was
// uncovered (50% baseline). The CLI path leaves onEvent nil; the
// MCP server wires it via SetEventHook. Without this test the
// branch only runs in MCP integration tests, never in the
// internal/index package's own suite.
//
// Indirectly covers SetEventHook by going through it (the right
// API for production wiring) rather than poking onEvent directly.

func TestEmitEvent_FiresRegisteredCallback(t *testing.T) {
	store, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()

	idx := New(store)
	var mu sync.Mutex
	var got []string
	idx.SetEventHook(func(eventType string, payload map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, eventType)
	})

	// Index a one-file project; that fires index_started + index_complete
	// (per the two emitEvent call sites in indexer.go).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Errorf("expected at least 2 events (index_started + index_complete); got %v", got)
	}
	hasStart, hasComplete := false, false
	for _, ev := range got {
		switch ev {
		case "index_started":
			hasStart = true
		case "index_complete":
			hasComplete = true
		}
	}
	if !hasStart {
		t.Errorf("missing index_started event; got %v", got)
	}
	if !hasComplete {
		t.Errorf("missing index_complete event; got %v", got)
	}
}

// TestEmitEvent_NoCallbackIsSilent pins the back-compat path:
// indexers without a registered callback (the bare CLI) must not
// panic or error when index_started / index_complete fire. The
// pre-#654 code path; protects against a future refactor that
// accidentally requires the callback.
func TestEmitEvent_NoCallbackIsSilent(t *testing.T) {
	store, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()

	idx := New(store) // SetEventHook NOT called — onEvent stays nil
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	// Must not panic.
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index with nil onEvent errored: %v", err)
	}
}
