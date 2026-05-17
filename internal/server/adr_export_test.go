package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// #1331 v0.72 slice 1: ADR export — renders the project's runtime ADR
// map as a single Markdown document. Tests pin: deterministic ordering
// (lexicographic keys → identical re-runs avoid diff churn when the
// maintainer pipes export into docs/adr/), empty-project messaging,
// section structure, and the single-entry plural-grammar edge case.

func TestRenderADRsAsMarkdown_EmptyProject(t *testing.T) {
	md := renderADRsAsMarkdown("proj", map[string]string{})
	if !strings.Contains(md, "No ADRs recorded yet") {
		t.Errorf("empty-project export should advise how to record; got:\n%s", md)
	}
	if !strings.Contains(md, "proj") {
		t.Errorf("export should name the project; got:\n%s", md)
	}
	if !strings.Contains(md, "0 entries") {
		t.Errorf("empty-project export should report 0 entries; got:\n%s", md)
	}
}

// TestRenderADRsAsMarkdown_DeterministicOrdering pins the lexicographic
// key sort. Map iteration is unordered in Go, so a naive `for k, v := range`
// would produce diff churn every run — defeats the purpose of piping the
// export into a checked-in docs/adr/ file.
func TestRenderADRsAsMarkdown_DeterministicOrdering(t *testing.T) {
	entries := map[string]string{
		"ZULU":  "last alphabetically",
		"ALPHA": "first alphabetically",
		"MIKE":  "middle",
	}
	for i := 0; i < 5; i++ {
		md := renderADRsAsMarkdown("proj", entries)
		alphaIdx := strings.Index(md, "## ALPHA")
		mikeIdx := strings.Index(md, "## MIKE")
		zuluIdx := strings.Index(md, "## ZULU")
		if alphaIdx == -1 || mikeIdx == -1 || zuluIdx == -1 {
			t.Fatalf("missing expected section; got:\n%s", md)
		}
		if !(alphaIdx < mikeIdx && mikeIdx < zuluIdx) {
			t.Errorf("section order wrong: ALPHA=%d MIKE=%d ZULU=%d (want strictly increasing)", alphaIdx, mikeIdx, zuluIdx)
		}
	}
}

// TestRenderADRsAsMarkdown_SectionStructure pins the per-key block
// shape: H2 header with the raw key, blank line, value body, blank
// line, then ---  separator (between sections only, not after the
// last). A regression that drops the separator would render adjacent
// values as if they were the same section.
func TestRenderADRsAsMarkdown_SectionStructure(t *testing.T) {
	entries := map[string]string{
		"STACK":   "Go + SQLite",
		"PURPOSE": "code intelligence MCP server",
	}
	md := renderADRsAsMarkdown("proj", entries)
	if !strings.Contains(md, "## PURPOSE\n\ncode intelligence MCP server\n") {
		t.Errorf("expected PURPOSE H2 section with value body; got:\n%s", md)
	}
	if !strings.Contains(md, "## STACK\n\nGo + SQLite\n") {
		t.Errorf("expected STACK H2 section with value body; got:\n%s", md)
	}
	if !strings.Contains(md, "\n---\n") {
		t.Errorf("expected --- separator between sections; got:\n%s", md)
	}
	// Separator count == sections - 1 (no trailing separator).
	if got := strings.Count(md, "\n---\n"); got != 1 {
		t.Errorf("expected exactly 1 separator for 2 sections; got %d separators in:\n%s", got, md)
	}
}

// TestRenderADRsAsMarkdown_SingleEntryPlural pins the "1 entry" vs
// "N entries" grammar. Pre-fix grammar bugs in user-visible exported
// docs are minor but read sloppy; pin the plural rule.
func TestRenderADRsAsMarkdown_SingleEntryPlural(t *testing.T) {
	md1 := renderADRsAsMarkdown("proj", map[string]string{"K": "v"})
	if !strings.Contains(md1, "1 entry") || strings.Contains(md1, "1 entries") {
		t.Errorf("single-entry export should say '1 entry' (not entries); got:\n%s", md1)
	}
	md2 := renderADRsAsMarkdown("proj", map[string]string{"K1": "v1", "K2": "v2"})
	if !strings.Contains(md2, "2 entries") {
		t.Errorf("multi-entry export should say 'N entries'; got:\n%s", md2)
	}
}

// TestHandleADR_ExportEndToEnd pins the full handler wiring:
// invoke adr action=export against a project with two stored ADRs,
// verify the response carries the rendered Markdown + format tag +
// count. Catches regressions in the new switch-case wiring (e.g.,
// future refactor that returns the wrong data field).
func TestHandleADR_ExportEndToEnd(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "exp-proj"
	if err := store.UpsertProject(db.Project{ID: "exp-proj", Path: "/tmp/exp", Name: "exp"}); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	if err := store.SetADR("exp-proj", "STACK", "Go + SQLite"); err != nil {
		t.Fatalf("SetADR STACK: %v", err)
	}
	if err := store.SetADR("exp-proj", "PURPOSE", "code intelligence"); err != nil {
		t.Fatalf("SetADR PURPOSE: %v", err)
	}

	args, _ := json.Marshal(map[string]any{"action": "export"})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
		Name: "adr", Arguments: args,
	}}
	r, err := srv.handleADR(context.Background(), req)
	if err != nil {
		t.Fatalf("handleADR export: %v", err)
	}
	if r.IsError {
		t.Fatalf("export returned error envelope: %s", textOf(t, r))
	}

	body := textOf(t, r)
	// The response is a JSON envelope wrapping the FixReport-shaped data.
	// Grep the markdown body for both stored keys + the format tag.
	if !strings.Contains(body, `"format":"markdown"`) && !strings.Contains(body, `"format": "markdown"`) {
		t.Errorf("expected format=markdown tag in envelope; got:\n%s", body)
	}
	// JSON-escaped Markdown — the H2 headers should appear with escaped newlines.
	if !strings.Contains(body, "## STACK") {
		t.Errorf("export body missing STACK section; got:\n%s", body)
	}
	if !strings.Contains(body, "## PURPOSE") {
		t.Errorf("export body missing PURPOSE section; got:\n%s", body)
	}
	if !strings.Contains(body, `"count":2`) && !strings.Contains(body, `"count": 2`) {
		t.Errorf("expected count=2 in envelope; got:\n%s", body)
	}
}
