package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// #499: every tool handler must surface unknown args in _meta.warnings
// rather than silently ignoring them. Same failure-as-pedagogy contract
// as #473 (unknown pinchQL property warnings).
func TestUnknownArgs_NeighborhoodDepthArg_Warns(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.sessionID = "p1"

	got := srv.unknownArgs("neighborhood", map[string]any{
		"id":    "irrelevant",
		"depth": 1, // not in neighborhood's InputSchema
	})
	if len(got) == 0 {
		t.Fatalf("expected warning for unknown arg 'depth' on neighborhood; got none")
	}
	if !strings.Contains(got[0], "depth") {
		t.Errorf("warning should name the offending arg; got %q", got[0])
	}
	if !strings.Contains(got[0], "neighborhood") {
		t.Errorf("warning should name the tool; got %q", got[0])
	}
	// The hint should list at least one accepted key so the agent can
	// self-correct.
	if !strings.Contains(got[0], "accepted") {
		t.Errorf("warning should include 'accepted' key list; got %q", got[0])
	}
}

func TestUnknownArgs_AllValidArgs_Silent(t *testing.T) {
	srv, _, _ := newTestServer(t)

	got := srv.unknownArgs("neighborhood", map[string]any{
		"id":             "x",
		"include_source": true,
		"limit":          10,
	})
	if got != nil {
		t.Errorf("all-valid args must produce no warnings; got %v", got)
	}
}

func TestUnknownArgs_SchemaToolWithToolArg_Warns(t *testing.T) {
	srv, _, _ := newTestServer(t)

	got := srv.unknownArgs("schema", map[string]any{"tool": "neighborhood"})
	if len(got) == 0 {
		t.Fatalf("schema tool only accepts 'project'; 'tool' arg should warn")
	}
	if !strings.Contains(got[0], "tool") {
		t.Errorf("warning should name 'tool' arg; got %q", got[0])
	}
}

func TestUnknownArgs_UnknownTool_Skipped(t *testing.T) {
	srv, _, _ := newTestServer(t)
	got := srv.unknownArgs("nonexistent", map[string]any{"foo": 1})
	if got != nil {
		t.Errorf("unknown tool must not produce warnings (avoid false positives on bare-name lookups); got %v", got)
	}
}

// End-to-end: warnings appear in _meta.warnings of the actual response.
// makeReq doesn't set Params.Name; do it explicitly here so beginCall
// captures the tool name and unknownArgs can resolve the schema.
func TestHandleSchema_UnknownArg_SurfacesWarning(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.sessionID = "p1"

	req := makeReq(map[string]any{
		"tool": "neighborhood", // the friction case from #499 repro
	})
	req.Params.Name = "schema"

	result, err := srv.handleSchema(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSchema: %v", err)
	}
	body := decode(t, result)
	meta, _ := body["_meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("missing _meta")
	}
	warningsAny, present := meta["warnings"]
	if !present {
		t.Fatalf("expected _meta.warnings on unknown 'tool' arg; got meta=%v", meta)
	}
	// Warnings is []any from JSON round-trip.
	warnings, ok := warningsAny.([]any)
	if !ok || len(warnings) == 0 {
		raw, _ := json.Marshal(warningsAny)
		t.Fatalf("warnings should be non-empty array; got %s", string(raw))
	}
	first, _ := warnings[0].(string)
	if !strings.Contains(first, "tool") || !strings.Contains(first, "schema") {
		t.Errorf("warning shape wrong: got %q", first)
	}
}
