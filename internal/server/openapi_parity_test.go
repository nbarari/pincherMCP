package server

import (
	"encoding/json"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// #558 Phase 1: parity gate. Every tool registered in s.handlers MUST
// appear in /v1/openapi.json. Pre-fix the openAPISpec hardcoded a
// 15-element slice that drifted whenever a new tool was added —
// dead_code, guide, neighborhood, init were silently invisible to
// OpenAPI consumers (Postman, Cursor, copilots) for releases.
//
// Same shape as TestStore_AllExportedMethodsClassified: when someone
// adds a new tool, the dynamic openAPISpec auto-includes it; this
// test guarantees the auto-include actually happened by re-deriving
// the expected set from s.handlers and asserting equality.
func TestOpenAPI_ParityWithRegisteredHandlers(t *testing.T) {
	srv, _, _ := newTestServer(t)

	// Expected: every handler name shows up at /v1/<name>.
	expected := map[string]bool{}
	for name := range srv.handlers {
		expected[name] = true
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/openapi.json", nil)
	srv.ServeHTTP(w, r)

	var spec map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatalf("openapi.json parse: %v", err)
	}
	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatalf("openapi.json missing paths object")
	}

	// Strip the /v1/ prefix to recover tool names.
	got := map[string]bool{}
	for path := range paths {
		if name := strings.TrimPrefix(path, "/v1/"); name != path {
			got[name] = true
		}
	}

	// Every handler must be in the spec.
	var missing []string
	for name := range expected {
		if !got[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("handlers absent from /v1/openapi.json: %v\n"+
			"openAPISpec must enumerate every name in s.handlers — pre-#558 "+
			"this slice was hardcoded and silently drifted as new tools landed.",
			missing)
	}

	// And the spec shouldn't claim a tool that doesn't exist.
	var extra []string
	for name := range got {
		// Skip the special-cased GET endpoint surfaced separately.
		if name == "health" {
			continue
		}
		if !expected[name] {
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		t.Errorf("/v1/openapi.json claims paths with no registered handler: %v", extra)
	}
}

// TestOpenAPI_PerToolSchemaIsRealNotPlaceholder pins that the
// dynamic spec uses the tool's actual InputSchema (with properties,
// required fields, enums) rather than the bare {type: object}
// placeholder the pre-fix hardcoded version emitted. Catches
// regressions where InputSchema retrieval breaks silently.
func TestOpenAPI_PerToolSchemaIsRealNotPlaceholder(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/openapi.json", nil)
	srv.ServeHTTP(w, r)

	var spec map[string]any
	json.Unmarshal(w.Body.Bytes(), &spec)
	paths := spec["paths"].(map[string]any)

	// `search` is the canonical tool with a rich schema (kind, language,
	// corpus enum, limit, query). If its OpenAPI schema is bare {type:
	// object}, the InputSchema pull-through is broken.
	searchPath, ok := paths["/v1/search"].(map[string]any)
	if !ok {
		t.Fatal("/v1/search missing from openapi paths")
	}
	post := searchPath["post"].(map[string]any)
	body := post["requestBody"].(map[string]any)
	content := body["content"].(map[string]any)
	app := content["application/json"].(map[string]any)
	schema, ok := app["schema"].(map[string]any)
	if !ok {
		t.Fatal("/v1/search request schema not an object")
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		t.Errorf("/v1/search schema has no properties — InputSchema pull-through broken; got: %v", schema)
	}
	if _, hasQuery := props["query"]; !hasQuery {
		t.Errorf("/v1/search schema missing 'query' property; got props: %v", props)
	}
}

// TestOpenAPI_EveryToolHasNonPlaceholderResponseSchema is the
// response-side parity gate (#581). Sibling of
// TestOpenAPI_PerToolSchemaIsRealNotPlaceholder which gates the
// request side.
//
// Pre-#581 every endpoint emitted a 200 schema of the bare
// {type: object} placeholder — useless for generated SDKs and
// auto-importing OpenAPI clients. Now every tool registered in
// s.handlers must have an OutputSchema with `properties` AND a
// `_meta` reference. Catches new tools added without an entry in
// outputSchemas.
func TestOpenAPI_EveryToolHasNonPlaceholderResponseSchema(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/openapi.json", nil)
	srv.ServeHTTP(w, r)

	var spec map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatalf("openapi.json parse: %v", err)
	}
	paths := spec["paths"].(map[string]any)

	var missing []string
	for name := range srv.handlers {
		path, ok := paths["/v1/"+name].(map[string]any)
		if !ok {
			t.Errorf("/v1/%s missing from openapi paths (parity gate failure?)", name)
			continue
		}
		post, ok := path["post"].(map[string]any)
		if !ok {
			continue
		}
		responses, ok := post["responses"].(map[string]any)
		if !ok {
			t.Errorf("/v1/%s has no responses", name)
			continue
		}
		ok200, ok := responses["200"].(map[string]any)
		if !ok {
			t.Errorf("/v1/%s has no 200 response", name)
			continue
		}
		content := ok200["content"].(map[string]any)
		app := content["application/json"].(map[string]any)
		schema, ok := app["schema"].(map[string]any)
		if !ok {
			missing = append(missing, name+" (no schema object)")
			continue
		}
		props, hasProps := schema["properties"].(map[string]any)
		if !hasProps || len(props) == 0 {
			missing = append(missing, name+" (placeholder {type: object})")
			continue
		}
		// Every response must reference the shared _meta envelope.
		if _, hasMeta := props["_meta"]; !hasMeta {
			missing = append(missing, name+" (missing _meta in schema)")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("tools without real OpenAPI response schema (#581):\n  - %s\n"+
			"Add an entry in internal/server/openapi_output_schemas.go's outputSchemas map. "+
			"Every tool's success response must declare its top-level fields plus a _meta "+
			"reference so consumers (Postman, Cursor, generated SDKs) get a real contract.",
			strings.Join(missing, "\n  - "))
	}
}

// TestOpenAPI_HasSharedMetaAndErrorComponents pins the
// components/schemas/Meta + Error definitions emitted by
// openAPIComponentSchemas (#581). Per-endpoint schemas $ref these,
// so removing them silently breaks every response shape.
func TestOpenAPI_HasSharedMetaAndErrorComponents(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/openapi.json", nil)
	srv.ServeHTTP(w, r)

	var spec map[string]any
	json.Unmarshal(w.Body.Bytes(), &spec)
	components, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatal("openapi.json missing components")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatal("components.schemas missing")
	}
	for _, want := range []string{"Meta", "Error"} {
		if _, ok := schemas[want].(map[string]any); !ok {
			t.Errorf("components/schemas/%s missing", want)
		}
	}
	// Sanity: Meta declares baseline_method / tokens_used / latency_ms.
	if meta, ok := schemas["Meta"].(map[string]any); ok {
		props, _ := meta["properties"].(map[string]any)
		for _, want := range []string{"baseline_method", "tokens_used", "tokens_saved", "latency_ms"} {
			if _, ok := props[want]; !ok {
				t.Errorf("Meta schema missing property %q", want)
			}
		}
	}
}
