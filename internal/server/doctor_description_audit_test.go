package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// v0.65 description-honesty audit (continuation): the doctor tool's
// description named five returned items but missed two load-bearing
// fields: `binary_version` (top-level — the running pincher binary's
// version) and `advisories` (an array surfacing large-DB sizing
// hints + ghost-project warnings). It also omitted the `top` field's
// 50-row hard ceiling — agents passing top=100 got silently clamped
// to 50 with no signal from the description that this would happen.
//
// Table-from-the-start (#1152):
//   - Positive: description names every top-level field doctor
//     returns + the per-project schema/binary drift columns.
//   - Negative: `top` schema field does not still claim "Default
//     10" without mentioning the 50-row ceiling.
//   - Cross-check: a real handleDoctor call returns a response
//     containing every field the description names — description
//     vs runtime parity.

func TestDoctorDescription_NamesAllResponseFields(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tool := srv.tools["doctor"]
	if tool == nil {
		t.Fatal("doctor tool not registered")
	}
	desc := tool.Description
	for _, want := range []string{
		"binary_version",   // top-level field, was missing
		"advisories",       // array surfaced via attachWarning; was missing
		"schema_version_at_index", // per-project drift column; was glossed as "staleness"
		"extraction_failures",
		"slow_queries",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("doctor description missing %q\nGOT:\n%s", want, desc)
		}
	}
}

// Negative: top schema description must mention the 50-row
// ceiling that handleDoctor enforces. Pre-fix the schema said
// "Default 10" without naming the max — agents passing top=100
// got silently clamped, with the clamp warning only available
// _after_ the call.
func TestDoctorSchema_TopDescribesCeiling(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tool := srv.tools["doctor"]
	if tool == nil {
		t.Fatal("doctor tool not registered")
	}
	// Find the top field description in the raw schema string.
	raw, ok := tool.InputSchema.(json.RawMessage)
	if !ok {
		t.Fatalf("InputSchema not json.RawMessage: %T", tool.InputSchema)
	}
	rawSchema := string(raw)
	// The raw form is JSON, but search for the substring directly
	// since we just need to confirm the prose contains the ceiling.
	if !strings.Contains(rawSchema, "max 50") &&
		!strings.Contains(rawSchema, "50 (max)") &&
		!strings.Contains(rawSchema, "above 50") {
		t.Errorf("doctor.top schema description missing 50-row ceiling hint — agents passing top=100 get silently clamped\nGOT raw schema:\n%s",
			rawSchema)
	}
}

// Cross-check: a real handleDoctor call returns a response
// containing every field the description names. Pre-fix a
// future refactor could drop one of the fields without the
// contract test noticing.
func TestHandleDoctor_ResponseContainsDescriptionFields(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	result, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, result)

	// Description promises these keys. Some land empty when no
	// data is present (e.g. extraction_failures on a fresh DB);
	// the key being PRESENT is what matters for the contract.
	for _, want := range []string{
		"schema_version",
		"binary_version",
		"db_size_bytes",
		"wal_size_bytes",
		"projects",
		"extraction_failures",
		"slow_queries",
	} {
		if _, ok := body[want]; !ok {
			t.Errorf("doctor response missing key %q promised by description", want)
		}
	}
}
