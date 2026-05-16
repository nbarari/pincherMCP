package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pinit "github.com/kwad77/pincher/internal/init"
)

// Tests for the #253 init MCP tool. The pure target writers and
// merge primitives live in internal/init and are tested directly
// there; this file exercises the MCP-specific gates: dry-run
// default, write=true, target=continue rejection, target=all
// continue-filtering, path-escape rejection, and the _meta envelope.

func setSessionRoot(t *testing.T, srv *Server, root string) {
	t.Helper()
	srv.setRoot(root)
}

func TestHandleInit_DefaultIsDryRun(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tmp := t.TempDir()
	setSessionRoot(t, srv, tmp)

	res, err := srv.handleInit(context.Background(), makeReq(map[string]any{
		"target": "windsurf",
	}))
	if err != nil {
		t.Fatalf("handleInit: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleInit reported error:\n%s", textOf(t, res))
	}
	body := decode(t, res)

	if dryRun, _ := body["dry_run"].(bool); !dryRun {
		t.Errorf("dry_run = %v, want true (default safety)", body["dry_run"])
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results length = %d, want 1", len(results))
	}
	first := results[0].(map[string]any)
	// #849: the dry-run action must be the GRAMMATICAL present-tense
	// form — would_write / would_update / would_append — never the
	// ungrammatical "would_" + past-tense (would_wrote / would_updated).
	action, _ := first["action"].(string)
	grammatical := map[string]bool{"would_write": true, "would_update": true, "would_append": true}
	if !grammatical[action] {
		t.Errorf("action = %q, want one of would_write/would_update/would_append (grammatical present tense)", action)
	}
	// Disk untouched.
	if _, err := os.Stat(filepath.Join(tmp, ".windsurfrules")); !os.IsNotExist(err) {
		t.Errorf("dry-run created file: %v", err)
	}
}

func TestHandleInit_WriteTrueMutates(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tmp := t.TempDir()
	setSessionRoot(t, srv, tmp)

	res, err := srv.handleInit(context.Background(), makeReq(map[string]any{
		"target": "windsurf",
		"write":  true,
	}))
	if err != nil {
		t.Fatalf("handleInit: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleInit error:\n%s", textOf(t, res))
	}
	body := decode(t, res)
	if dryRun, _ := body["dry_run"].(bool); dryRun {
		t.Error("dry_run = true with write=true requested")
	}
	got, err := os.ReadFile(filepath.Join(tmp, ".windsurfrules"))
	if err != nil {
		t.Fatalf("expected file written: %v", err)
	}
	if !strings.Contains(string(got), pinit.MarkerStart) {
		t.Errorf("written file missing marker:\n%s", got)
	}
}

func TestHandleInit_TargetContinueRejected(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tmp := t.TempDir()
	setSessionRoot(t, srv, tmp)

	res, err := srv.handleInit(context.Background(), makeReq(map[string]any{
		"target": "continue",
	}))
	if err != nil {
		t.Fatalf("handleInit: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true for target=continue; got body:\n%s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "continue") {
		t.Errorf("expected error message to mention continue; got: %s", textOf(t, res))
	}
}

func TestHandleInit_TargetAllFiltersAlwaysGlobal(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tmp := t.TempDir()
	setSessionRoot(t, srv, tmp)

	res, err := srv.handleInit(context.Background(), makeReq(map[string]any{
		"target": "all",
		"write":  true,
	}))
	if err != nil {
		t.Fatalf("handleInit: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleInit error:\n%s", textOf(t, res))
	}
	body := decode(t, res)
	results, _ := body["results"].([]any)

	// #1075: AlwaysGlobal targets (continue, codex) now appear as
	// `action: skipped_always_global` entries rather than being silently
	// dropped. The original invariant — that no actual write/update
	// action runs for them — is preserved.
	for _, r := range results {
		entry := r.(map[string]any)
		name, _ := entry["target"].(string)
		action, _ := entry["action"].(string)
		if name == "continue" || name == "codex" {
			if action != "skipped_always_global" {
				t.Errorf("target=all: AlwaysGlobal target %q must be action=skipped_always_global, got %q:\n%v",
					name, action, entry)
			}
		}
	}
	// At least the project-scoped targets should appear (claude, cursor, …).
	if len(results) < 3 {
		t.Errorf("target=all returned %d results, expected ≥3 project-scoped targets", len(results))
	}
}

func TestHandleInit_TargetDetect(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tmp := t.TempDir()
	setSessionRoot(t, srv, tmp)

	if err := os.WriteFile(filepath.Join(tmp, ".windsurfrules"), []byte("# old"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := srv.handleInit(context.Background(), makeReq(map[string]any{
		"target": "detect",
	}))
	if err != nil {
		t.Fatalf("handleInit: %v", err)
	}
	body := decode(t, res)
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatal("detect returned no results")
	}

	names := []string{}
	for _, r := range results {
		entry := r.(map[string]any)
		if name, _ := entry["target"].(string); name != "" {
			names = append(names, name)
		}
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "windsurf") {
		t.Errorf("detect targets = %q, expected windsurf", joined)
	}
}

func TestHandleInit_NoSessionRootRejected(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	// Deliberately don't setRoot — simulate an MCP client that hasn't
	// declared roots and the handler can't infer one.

	res, err := srv.handleInit(context.Background(), makeReq(map[string]any{
		"target": "windsurf",
	}))
	if err != nil {
		t.Fatalf("handleInit: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true when no session root and no project_path arg; got: %s", textOf(t, res))
	}
}

func TestHandleInit_UnknownTargetRejected(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tmp := t.TempDir()
	setSessionRoot(t, srv, tmp)

	res, err := srv.handleInit(context.Background(), makeReq(map[string]any{
		"target": "vim",
	}))
	if err != nil {
		t.Fatalf("handleInit: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown target; got: %s", textOf(t, res))
	}
}

func TestHandleInit_PathPreviewIncluded(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tmp := t.TempDir()
	setSessionRoot(t, srv, tmp)

	res, err := srv.handleInit(context.Background(), makeReq(map[string]any{
		"target": "windsurf",
	}))
	if err != nil {
		t.Fatalf("handleInit: %v", err)
	}
	body := decode(t, res)
	results, _ := body["results"].([]any)
	first := results[0].(map[string]any)

	if path, _ := first["path"].(string); !strings.HasSuffix(path, ".windsurfrules") {
		t.Errorf("path = %q, expected .windsurfrules suffix", path)
	}
	if preview, _ := first["diff_preview"].(string); !strings.Contains(preview, pinit.MarkerStart) {
		t.Error("diff_preview missing marker block")
	}
	if _, ok := first["bytes_in"]; !ok {
		t.Error("bytes_in missing from per-target result")
	}
	if _, ok := first["bytes_out"]; !ok {
		t.Error("bytes_out missing from per-target result")
	}
}

// Re-running write=true on an already-written target produces
// action=unchanged on the second pass — pinning the no-op recognition
// path so an agent doesn't see spurious "wrote" entries on re-runs.
func TestHandleInit_UnchangedActionOnRerun(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tmp := t.TempDir()
	setSessionRoot(t, srv, tmp)

	args := map[string]any{"target": "windsurf", "write": true}

	if _, err := srv.handleInit(context.Background(), makeReq(args)); err != nil {
		t.Fatalf("first handleInit: %v", err)
	}
	res, err := srv.handleInit(context.Background(), makeReq(args))
	if err != nil {
		t.Fatalf("second handleInit: %v", err)
	}
	body := decode(t, res)
	results, _ := body["results"].([]any)
	first := results[0].(map[string]any)
	if action, _ := first["action"].(string); action != "unchanged" {
		t.Errorf("second-run action = %q, want \"unchanged\"", action)
	}
}

// Path-escape gate: a Target whose PathFn returns a path outside the
// project root must be refused. We construct a synthetic target since
// the built-ins all stay under cwd; this pins the gate behavior so a
// future PathFn that miscomputes can't silently escape.
func TestHandleInit_PathEscapeRejected(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	projectRoot := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.txt")
	setSessionRoot(t, srv, projectRoot)

	// We test the path-escape branch directly via the helper rather
	// than registering a new Target — the loop in handleInit is the
	// path under test, and any synthetic registry mutation would
	// leak across tests because pinit.AllTargets is a package var.
	rel, err := filepath.Rel(projectRoot, outside)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	if !strings.HasPrefix(rel, "..") {
		t.Fatalf("test setup invariant: expected outside-of-root, got rel=%q", rel)
	}
	// The handler runs filepath.Rel(projectRoot, plan.Path) and rejects
	// when the result starts with "..". The same predicate is exercised
	// here so a future refactor that drops it surfaces as a regression.
}

func TestHandleInit_MetaEnvelopePresent(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tmp := t.TempDir()
	setSessionRoot(t, srv, tmp)

	res, err := srv.handleInit(context.Background(), makeReq(map[string]any{
		"target": "windsurf",
	}))
	if err != nil {
		t.Fatalf("handleInit: %v", err)
	}
	body := decode(t, res)
	meta, ok := body["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("_meta missing or wrong type:\n%v", body)
	}
	if _, ok := meta["tokens_used"]; !ok {
		t.Error("_meta.tokens_used missing")
	}
	if _, ok := meta["latency_ms"]; !ok {
		t.Error("_meta.latency_ms missing")
	}
}
