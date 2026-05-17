package server

import (
	"context"
	"encoding/json"
	"testing"
)

// #1098 v0.71: structured warnings_v2 lives alongside the legacy string
// warnings array for one release cycle. Side-by-side back-compat per
// the maintainer's option (a) call.

func TestUnknownArgs_SurfacesBothStringAndStructuredV2_1098(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.sessionID = "p1"

	req := makeReq(map[string]any{
		"tool": "neighborhood",
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

	// Legacy string-form warnings must still be populated for back-compat.
	wsAny, ok := meta["warnings"].([]any)
	if !ok || len(wsAny) == 0 {
		raw, _ := json.Marshal(meta["warnings"])
		t.Fatalf("string warnings missing or empty (back-compat broken): %s", raw)
	}

	// Structured warnings_v2 must also be populated, with the same count.
	v2Any, ok := meta["warnings_v2"].([]any)
	if !ok || len(v2Any) == 0 {
		raw, _ := json.Marshal(meta["warnings_v2"])
		t.Fatalf("warnings_v2 missing or empty: %s", raw)
	}
	if len(wsAny) != len(v2Any) {
		t.Errorf("warnings count (%d) and warnings_v2 count (%d) should match", len(wsAny), len(v2Any))
	}

	// Inspect the v2 entry shape: code, severity, message, data.
	entry, ok := v2Any[0].(map[string]any)
	if !ok {
		t.Fatalf("v2[0] should be an object; got %T", v2Any[0])
	}
	if entry["code"] != "unknown_arg" {
		t.Errorf("v2 code = %v, want unknown_arg", entry["code"])
	}
	if entry["severity"] != "warning" {
		t.Errorf("v2 severity = %v, want warning", entry["severity"])
	}
	if _, has := entry["message"]; !has {
		t.Errorf("v2 message field missing")
	}
	data, ok := entry["data"].(map[string]any)
	if !ok {
		t.Fatalf("v2 data should be an object; got %T", entry["data"])
	}
	if data["unknown"] != "tool" {
		t.Errorf("v2 data.unknown = %v, want %q", data["unknown"], "tool")
	}
	if data["tool"] != "schema" {
		t.Errorf("v2 data.tool = %v, want %q", data["tool"], "schema")
	}
	if accepted, _ := data["accepted"].([]any); len(accepted) == 0 {
		t.Errorf("v2 data.accepted should be non-empty list of accepted keys; got %v", data["accepted"])
	}
}

// attachWarningStructured emit-path unit test: when called without a
// preceding attachWarning, warnings_v2 lands but the legacy `warnings`
// field stays untouched.
func TestAttachWarningStructured_DoesNotPopulateLegacyField_1098(t *testing.T) {
	t.Parallel()
	data := map[string]any{}
	attachWarningStructured(data, "test_code", WarningSeverityInfo, "test message",
		map[string]any{"k": "v"})

	meta, _ := data["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("expected _meta to be auto-created")
	}
	if _, has := meta["warnings"]; has {
		t.Errorf("legacy warnings slice should NOT be auto-populated; helpers are independent")
	}
	v2, ok := meta["warnings_v2"].([]map[string]any)
	if !ok || len(v2) != 1 {
		t.Fatalf("warnings_v2 should have one entry; got %v", meta["warnings_v2"])
	}
	if v2[0]["code"] != "test_code" || v2[0]["severity"] != "info" || v2[0]["message"] != "test message" {
		t.Errorf("v2 entry shape wrong: %+v", v2[0])
	}
	d, _ := v2[0]["data"].(map[string]any)
	if d["k"] != "v" {
		t.Errorf("v2 data field lost: %+v", v2[0])
	}
}

// setDiagnosisStructured lands at data["diagnosis_v2"] without
// touching the existing string `diagnosis` field.
func TestSetDiagnosisStructured_DoesNotOverwriteLegacyField_1098(t *testing.T) {
	t.Parallel()
	data := map[string]any{"diagnosis": "legacy prose string"}
	setDiagnosisStructured(data, "no_matches", "no matches at min_confidence ≥ 0.71",
		map[string]any{"effective_min_confidence": 0.71})

	if data["diagnosis"] != "legacy prose string" {
		t.Errorf("legacy diagnosis string was overwritten; got %v", data["diagnosis"])
	}
	v2, _ := data["diagnosis_v2"].(map[string]any)
	if v2 == nil {
		t.Fatal("diagnosis_v2 not set")
	}
	if v2["code"] != "no_matches" {
		t.Errorf("v2 code wrong: %v", v2["code"])
	}
	d, _ := v2["data"].(map[string]any)
	if d["effective_min_confidence"] != 0.71 {
		t.Errorf("v2 data field wrong: %v", v2["data"])
	}
}

// Nil extra: the `data` field is omitted from the structured entry
// (not present as an empty map). Same for diagnosis.
func TestAttachWarningStructured_NilExtra_OmitsDataField_1098(t *testing.T) {
	t.Parallel()
	data := map[string]any{}
	attachWarningStructured(data, "code", WarningSeverityWarning, "msg", nil)
	v2 := data["_meta"].(map[string]any)["warnings_v2"].([]map[string]any)
	if _, has := v2[0]["data"]; has {
		t.Errorf("nil extra should omit data field; got %+v", v2[0])
	}
}
