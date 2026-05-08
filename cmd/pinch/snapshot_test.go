package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pincherMCP/pincher/internal/db"
	"github.com/pincherMCP/pincher/internal/index"
)

// corpora lists every pinned corpus the snapshot test should drive.
// Adding a new corpus = add a directory under testdata/corpus/ + commit
// a <name>.snapshot.json + add the name here. The Makefile's CORPORA
// list mirrors this — keep in sync.
var corpora = []string{
	"go-project",
	"k8s-ops",
	"node-monorepo",
}

// TestCorpusSnapshot pins the snapshot round-trip end-to-end without
// relying on Make / jq (#33 substrate).
//
// For each pinned corpus it indexes testdata/corpus/<name>, builds the
// snapshot via the same code path that --json-summary uses, and asserts
// byte-identical equality to the committed <name>.snapshot.json (modulo
// the noisy fields stripped by the Makefile pipeline).
//
// Why this test exists alongside `make corpus-test`:
//   - CI on platforms without jq still gets coverage (Windows particularly).
//   - Run as part of `go test ./...` — no separate make target needed.
//   - Regression debug surface: the test failure shows the structural diff
//     directly, not a "diff -u" on serialized JSON which can be hard to read.
func TestCorpusSnapshot(t *testing.T) {
	for _, name := range corpora {
		t.Run(name, func(t *testing.T) {
			runCorpusSnapshot(t, name)
		})
	}
}

func runCorpusSnapshot(t *testing.T, name string) {
	t.Helper()

	corpusPath, err := filepath.Abs("../../testdata/corpus/" + name)
	if err != nil {
		t.Fatalf("abs corpus path: %v", err)
	}
	snapshotPath, err := filepath.Abs("../../testdata/corpus/" + name + ".snapshot.json")
	if err != nil {
		t.Fatalf("abs snapshot path: %v", err)
	}

	// Use the test harness's own temp dir so the scratch DB is GC'd by the
	// testing framework — no need to mirror the Makefile's mkdir dance.
	dataDir := t.TempDir()
	store, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()

	idx := index.New(store)
	result, err := idx.Index(context.Background(), corpusPath, false)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	// Capture the same JSON output emitSnapshotJSON would emit by
	// redirecting stdout for the duration of the call.
	stdoutOrig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	emitSnapshotJSON(store, result, dataDir)
	w.Close()
	os.Stdout = stdoutOrig

	var actualBuf bytes.Buffer
	if _, err := actualBuf.ReadFrom(r); err != nil {
		t.Fatalf("read snapshot stdout: %v", err)
	}

	// Strip noisy fields (db_size_kb, duration_ms, schema_version is kept)
	// — same as the Makefile's jq filter.
	var actual map[string]any
	if err := json.Unmarshal(actualBuf.Bytes(), &actual); err != nil {
		t.Fatalf("unmarshal actual: %v\n%s", err, actualBuf.String())
	}
	delete(actual, "db_size_kb")
	delete(actual, "duration_ms")

	wantBytes, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot file: %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}

	// Compare via canonical JSON marshal so map iteration order doesn't
	// produce false diffs.
	actualJSON := canonicalJSON(t, actual)
	wantJSON := canonicalJSON(t, want)
	if string(actualJSON) != string(wantJSON) {
		t.Errorf("snapshot mismatch for %q.\n"+
			"If this change is intentional, run `make corpus-snapshot-update`\n"+
			"and review the diff in your PR.\n\n"+
			"--- want\n%s\n\n+++ got\n%s",
			name, wantJSON, actualJSON)
	}
}

// canonicalJSON produces deterministic, indented JSON for diff readability.
// Sorts map keys at every level so the comparison is structural, not
// dependent on Go's map iteration order.
func canonicalJSON(t *testing.T, v any) []byte {
	t.Helper()
	// json.Marshal already sorts top-level map keys alphabetically. For
	// nested maps we'd need a recursive sort; ours are flat enough that
	// the default behaviour suffices.
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal canonical: %v", err)
	}
	return b
}

// TestCorpusSnapshot_MakeTargetIsRunnable smoke-tests `make corpus-test`
// itself if make + jq are available. Skipped otherwise to keep CI green
// on platforms without those tools (Windows). The Go-only test above
// is the canonical gate.
func TestCorpusSnapshot_MakeTargetIsRunnable(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not available")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}

	cmd := exec.Command("make", "corpus-test")
	cmd.Dir = "../.." // repo root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make corpus-test failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("All corpus snapshots match")) {
		t.Errorf("unexpected output:\n%s", out)
	}
}
